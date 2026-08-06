package eventsource

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// traceRecorder captures ServerTrace callbacks from any goroutine. Every field
// is guarded by mu because callbacks fire from both the handler goroutines and
// the Server's dispatch goroutine. The contexts observed by the handler-
// goroutine callbacks are captured so tests can assert they derive from the
// subscriber's request.
type traceRecorder struct {
	mu             sync.Mutex
	added          []SubscriberAddedInfo
	removed        []SubscriberRemovedInfo
	dropped        []SubscriberDroppedInfo
	eventsSent     []EventSentInfo
	commentsSent   []CommentSentInfo
	discarded      []EventDiscardedInfo
	writeErrors    []WriteErrorInfo
	replayStarted  []ReplayInfo
	replayFinished []ReplayFinishedInfo
	addedCtx       []context.Context
	eventSentCtx   []context.Context
}

func (r *traceRecorder) trace() *ServerTrace {
	return &ServerTrace{
		SubscriberAdded: func(ctx context.Context, i SubscriberAddedInfo) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.added = append(r.added, i)
			r.addedCtx = append(r.addedCtx, ctx)
		},
		SubscriberRemoved: func(_ context.Context, i SubscriberRemovedInfo) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.removed = append(r.removed, i)
		},
		SubscriberDropped: func(i SubscriberDroppedInfo) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.dropped = append(r.dropped, i)
		},
		EventSent: func(ctx context.Context, i EventSentInfo) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.eventsSent = append(r.eventsSent, i)
			r.eventSentCtx = append(r.eventSentCtx, ctx)
		},
		CommentSent: func(_ context.Context, i CommentSentInfo) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.commentsSent = append(r.commentsSent, i)
		},
		EventDiscarded: func(_ context.Context, i EventDiscardedInfo) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.discarded = append(r.discarded, i)
		},
		WriteError: func(_ context.Context, i WriteErrorInfo) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.writeErrors = append(r.writeErrors, i)
		},
		ReplayStarted: func(_ context.Context, i ReplayInfo) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.replayStarted = append(r.replayStarted, i)
		},
		ReplayFinished: func(_ context.Context, i ReplayFinishedInfo) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.replayFinished = append(r.replayFinished, i)
		},
	}
}

func (r *traceRecorder) snapshotAddedCtx() []context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]context.Context(nil), r.addedCtx...)
}

func (r *traceRecorder) snapshotEventSentCtx() []context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]context.Context(nil), r.eventSentCtx...)
}

func (r *traceRecorder) snapshotAdded() []SubscriberAddedInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]SubscriberAddedInfo(nil), r.added...)
}

func (r *traceRecorder) snapshotRemoved() []SubscriberRemovedInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]SubscriberRemovedInfo(nil), r.removed...)
}

func (r *traceRecorder) snapshotDropped() []SubscriberDroppedInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]SubscriberDroppedInfo(nil), r.dropped...)
}

func (r *traceRecorder) snapshotEventsSent() []EventSentInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]EventSentInfo(nil), r.eventsSent...)
}

func (r *traceRecorder) snapshotCommentsSent() []CommentSentInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]CommentSentInfo(nil), r.commentsSent...)
}

func (r *traceRecorder) snapshotDiscarded() []EventDiscardedInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]EventDiscardedInfo(nil), r.discarded...)
}

func (r *traceRecorder) snapshotWriteErrors() []WriteErrorInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]WriteErrorInfo(nil), r.writeErrors...)
}

func (r *traceRecorder) snapshotReplayStarted() []ReplayInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ReplayInfo(nil), r.replayStarted...)
}

func (r *traceRecorder) snapshotReplayFinished() []ReplayFinishedInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ReplayFinishedInfo(nil), r.replayFinished...)
}

// traceTestWriter is a minimal http.ResponseWriter and http.Flusher that a
// Server handler can be driven with directly. Its Write can be gated (to
// simulate a subscriber that is not reading), made to fail (to exercise the
// write-error path), or delayed (to make a measured write duration exceed the
// platform's clock granularity), and its Flush can be gated independently (to
// stall a handler in its initial flush). gate, flushGate, writeErr, and delay
// are configured before the handler starts and are not mutated afterwards,
// apart from closing the gates to release Write and Flush.
type traceTestWriter struct {
	mu         sync.Mutex
	hdr        http.Header
	buf        bytes.Buffer
	gate       chan struct{}
	flushGate  chan struct{}
	writeErr   error
	delay      time.Duration
	flushDelay time.Duration
}

func (w *traceTestWriter) Header() http.Header {
	if w.hdr == nil {
		w.hdr = http.Header{}
	}
	return w.hdr
}

func (w *traceTestWriter) WriteHeader(int) {}

func (w *traceTestWriter) Flush() {
	if w.flushGate != nil {
		<-w.flushGate
	}
	if w.flushDelay > 0 {
		time.Sleep(w.flushDelay)
	}
}

func (w *traceTestWriter) Write(p []byte) (int, error) {
	if w.gate != nil {
		<-w.gate
	}
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	if w.delay > 0 {
		time.Sleep(w.delay)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *traceTestWriter) bytesWritten() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Len()
}

func (w *traceTestWriter) contains(s string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return bytes.Contains(w.buf.Bytes(), []byte(s))
}

// captureLogger records formatted log lines so that the level-prefixed messages
// can be asserted.
type captureLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *captureLogger) Println(args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintln(args...))
}

func (l *captureLogger) Printf(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *captureLogger) containing(substr string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range l.lines {
		if bytes.Contains([]byte(line), []byte(substr)) {
			return true
		}
	}
	return false
}

// traceTestCtxKey is used to seed a value on a handler's request context so a
// test can confirm the value reaches the ServerTrace callbacks.
type traceTestCtxKey struct{}

// startTraceHandler runs a Server handler on its own goroutine, driving it with
// w and a cancelable request. The request context carries a traceTestCtxKey
// value so tests can assert the callback contexts derive from it. The returned
// cancel simulates the client closing the connection; done is closed once the
// handler returns.
func startTraceHandler(
	server *Server,
	channel string,
	w http.ResponseWriter,
	headers map[string]string,
) (context.CancelFunc, <-chan struct{}) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	ctx := context.WithValue(req.Context(), traceTestCtxKey{}, "request-scoped-value")
	ctx, cancel := context.WithCancel(ctx)
	req = req.WithContext(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.Handler(channel)(w, req)
	}()
	return cancel, done
}

func waitClosed(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		require.Fail(t, "timed out waiting for handler to exit")
	}
}

func TestServerTraceSubscriberAdded(t *testing.T) {
	t.Run("without Last-Event-ID", func(t *testing.T) {
		rec := &traceRecorder{}
		server := NewServer()
		server.Trace = rec.trace()
		defer server.Close()

		cancel, done := startTraceHandler(server, "test", &traceTestWriter{}, nil)
		defer func() {
			cancel()
			waitClosed(t, done)
		}()

		require.Eventually(t, func() bool {
			return len(rec.snapshotAdded()) == 1
		}, 2*time.Second, 10*time.Millisecond)

		added := rec.snapshotAdded()
		assert.Equal(t, "test", added[0].Channel)
		assert.False(t, added[0].HasLastEventID)
		assert.NotZero(t, added[0].SubscriberID)
	})

	t.Run("with Last-Event-ID", func(t *testing.T) {
		rec := &traceRecorder{}
		server := NewServer()
		server.Trace = rec.trace()
		defer server.Close()

		headers := map[string]string{"Last-Event-ID": "abc"}
		cancel, done := startTraceHandler(server, "test", &traceTestWriter{}, headers)
		defer func() {
			cancel()
			waitClosed(t, done)
		}()

		require.Eventually(t, func() bool {
			return len(rec.snapshotAdded()) == 1
		}, 2*time.Second, 10*time.Millisecond)

		assert.True(t, rec.snapshotAdded()[0].HasLastEventID)
	})
}

func TestServerTraceSubscriberRemovedReasons(t *testing.T) {
	t.Run("client_closed", func(t *testing.T) {
		rec := &traceRecorder{}
		server := NewServer()
		server.Trace = rec.trace()
		defer server.Close()

		cancel, done := startTraceHandler(server, "test", &traceTestWriter{}, nil)
		require.Eventually(t, func() bool {
			return len(rec.snapshotAdded()) == 1
		}, 2*time.Second, 10*time.Millisecond)

		cancel()
		waitClosed(t, done)

		removed := rec.snapshotRemoved()
		require.Len(t, removed, 1)
		assert.Equal(t, ReasonClientClosed, removed[0].Reason)
		assert.Equal(t, rec.snapshotAdded()[0].SubscriberID, removed[0].SubscriberID)
	})

	t.Run("max_conn_time", func(t *testing.T) {
		rec := &traceRecorder{}
		server := NewServer()
		server.Trace = rec.trace()
		server.MaxConnTime = 100 * time.Millisecond
		defer server.Close()

		cancel, done := startTraceHandler(server, "test", &traceTestWriter{}, nil)
		defer cancel()

		waitClosed(t, done)

		removed := rec.snapshotRemoved()
		require.Len(t, removed, 1)
		assert.Equal(t, ReasonMaxConnTime, removed[0].Reason)
	})

	t.Run("server_closed", func(t *testing.T) {
		rec := &traceRecorder{}
		server := NewServer()
		server.Trace = rec.trace()

		cancel, done := startTraceHandler(server, "test", &traceTestWriter{}, nil)
		defer cancel()
		require.Eventually(t, func() bool {
			return len(rec.snapshotAdded()) == 1
		}, 2*time.Second, 10*time.Millisecond)

		server.Close()
		waitClosed(t, done)

		removed := rec.snapshotRemoved()
		require.Len(t, removed, 1)
		assert.Equal(t, ReasonServerClosed, removed[0].Reason)
	})

	t.Run("unregistered", func(t *testing.T) {
		rec := &traceRecorder{}
		server := NewServer()
		server.Trace = rec.trace()
		defer server.Close()

		cancel, done := startTraceHandler(server, "test", &traceTestWriter{}, nil)
		defer cancel()
		require.Eventually(t, func() bool {
			return len(rec.snapshotAdded()) == 1
		}, 2*time.Second, 10*time.Millisecond)

		server.Unregister("test", true)
		waitClosed(t, done)

		removed := rec.snapshotRemoved()
		require.Len(t, removed, 1)
		assert.Equal(t, ReasonUnregistered, removed[0].Reason)
	})

	t.Run("buffer_overflow", func(t *testing.T) {
		rec := &traceRecorder{}
		server := NewServer()
		server.Trace = rec.trace()
		server.BufferSize = 1
		defer server.Close()

		w := &traceTestWriter{gate: make(chan struct{})}
		cancel, done := startTraceHandler(server, "test", w, nil)
		defer cancel()
		require.Eventually(t, func() bool {
			return len(rec.snapshotAdded()) == 1
		}, 2*time.Second, 10*time.Millisecond)

		// The handler is stalled inside Write, so it stops draining its channel.
		// Publishing past BufferSize forces the dispatch goroutine to drop it.
		for _, data := range []string{"a", "b", "c"} {
			<-server.PublishWithAcknowledgment([]string{"test"}, &publication{data: data})
		}

		require.Eventually(t, func() bool {
			return len(rec.snapshotDropped()) >= 1
		}, 2*time.Second, 10*time.Millisecond)

		close(w.gate) // let the handler run to completion
		waitClosed(t, done)

		dropped := rec.snapshotDropped()
		require.GreaterOrEqual(t, len(dropped), 1)
		assert.Equal(t, "test", dropped[0].Channel)
		assert.Equal(t, 1, dropped[0].BufferSize)
		assert.Equal(t, rec.snapshotAdded()[0].SubscriberID, dropped[0].SubscriberID)

		removed := rec.snapshotRemoved()
		require.Len(t, removed, 1)
		assert.Equal(t, ReasonBufferOverflow, removed[0].Reason)
		assert.Equal(t, dropped[0].SubscriberID, removed[0].SubscriberID)
	})

	t.Run("write_error", func(t *testing.T) {
		rec := &traceRecorder{}
		server := NewServer()
		server.Trace = rec.trace()
		defer server.Close()

		w := &traceTestWriter{writeErr: errors.New("boom")}
		cancel, done := startTraceHandler(server, "test", w, nil)
		defer cancel()
		require.Eventually(t, func() bool {
			return len(rec.snapshotAdded()) == 1
		}, 2*time.Second, 10*time.Millisecond)

		server.Publish([]string{"test"}, &publication{data: "x"})
		waitClosed(t, done)

		writeErrors := rec.snapshotWriteErrors()
		require.Len(t, writeErrors, 1)
		assert.Equal(t, "test", writeErrors[0].Channel)
		assert.Error(t, writeErrors[0].Err)

		removed := rec.snapshotRemoved()
		require.Len(t, removed, 1)
		assert.Equal(t, ReasonWriteError, removed[0].Reason)
	})

	// Race-free pin of the exit-time sweep: the handler is parked mid-batch
	// (its main event channel is invisible to the read loop), so the overflow
	// drop cannot be observed by any select case; only the teardown's sweep of
	// the closed event channel can recover the buffer_overflow reason.
	t.Run("buffer_overflow detected by the exit sweep alone", func(t *testing.T) {
		rec := &traceRecorder{}
		server := NewServer()
		server.Trace = rec.trace()
		server.BufferSize = 1
		server.ReplayAll = true
		repo := gatedReplayRepository{
			before: 1,
			after:  0,
			gate:   make(chan struct{}),
			done:   make(chan struct{}),
		}
		server.Register("test", repo)
		defer close(repo.gate)
		defer server.Close()

		w := &traceTestWriter{}
		cancel, done := startTraceHandler(server, "test", w, nil)
		require.Eventually(t, func() bool {
			return len(rec.snapshotReplayStarted()) == 1 && w.bytesWritten() > 0
		}, 2*time.Second, 10*time.Millisecond)

		// The handler is waiting for the next batch event (the producer is
		// parked at its gate). Overflow the invisible main channel to force
		// the drop, then disconnect: closeNotify is the only ready case.
		for _, data := range []string{"a", "b", "c"} {
			<-server.PublishWithAcknowledgment([]string{"test"}, &publication{data: data})
		}
		require.Eventually(t, func() bool {
			return len(rec.snapshotDropped()) == 1
		}, 2*time.Second, 10*time.Millisecond)
		cancel()
		waitClosed(t, done)

		removed := rec.snapshotRemoved()
		require.Len(t, removed, 1)
		assert.Equal(t, ReasonBufferOverflow, removed[0].Reason)
	})

	// SubscriberDropped is a promise of a matching buffer_overflow removal, so
	// buffer_overflow outranks even a write error that the drop itself caused.
	t.Run("buffer_overflow wins over write_error", func(t *testing.T) {
		rec := &traceRecorder{}
		server := NewServer()
		server.Trace = rec.trace()
		server.BufferSize = 1
		defer server.Close()

		w := &traceTestWriter{gate: make(chan struct{}), writeErr: errors.New("boom")}
		cancel, done := startTraceHandler(server, "test", w, nil)
		defer cancel()
		require.Eventually(t, func() bool {
			return len(rec.snapshotAdded()) == 1
		}, 2*time.Second, 10*time.Millisecond)

		// The handler parks in the write that will fail; the publishes behind
		// it overflow the buffer, so the Server drops the subscriber and fires
		// SubscriberDropped; then the parked write completes with its error.
		for _, data := range []string{"a", "b", "c"} {
			<-server.PublishWithAcknowledgment([]string{"test"}, &publication{data: data})
		}
		require.Eventually(t, func() bool {
			return len(rec.snapshotDropped()) == 1
		}, 2*time.Second, 10*time.Millisecond)
		close(w.gate)
		waitClosed(t, done)

		require.Len(t, rec.snapshotWriteErrors(), 1)
		removed := rec.snapshotRemoved()
		require.Len(t, removed, 1)
		assert.Equal(t, ReasonBufferOverflow, removed[0].Reason,
			"SubscriberDropped promises a buffer_overflow removal")
	})

	// A write error the handler has already reported through WriteError is
	// definitive: a Server close that lands while the handler is parked in the
	// failing write must not mask it.
	t.Run("write_error wins over server_closed", func(t *testing.T) {
		rec := &traceRecorder{}
		server := NewServer()
		server.Trace = rec.trace()

		w := &traceTestWriter{gate: make(chan struct{}), writeErr: errors.New("boom")}
		cancel, done := startTraceHandler(server, "test", w, nil)
		defer cancel()
		require.Eventually(t, func() bool {
			return len(rec.snapshotAdded()) == 1
		}, 2*time.Second, 10*time.Millisecond)

		// The handler parks in the write that will fail; the Server closes
		// (recording server_closed and closing the event channel); then the
		// write completes with its error.
		server.Publish([]string{"test"}, &publication{data: "x"})
		server.Close()
		close(w.gate)
		waitClosed(t, done)

		require.Len(t, rec.snapshotWriteErrors(), 1)
		removed := rec.snapshotRemoved()
		require.Len(t, removed, 1)
		assert.Equal(t, ReasonWriteError, removed[0].Reason)
	})

	// A subscriber the Server drops for buffer overflow must be removed with
	// ReasonBufferOverflow even when the handler's read loop leaves through a
	// different exit first. The dropped subscriber is stalled in a slow write
	// with a full buffer behind it, so the handler cannot have observed the
	// closed channel before the competing exit condition fires.
	t.Run("buffer_overflow wins over max_conn_time", func(t *testing.T) {
		rec := &traceRecorder{}
		server := NewServer()
		server.Trace = rec.trace()
		server.BufferSize = 1
		server.MaxConnTime = 20 * time.Millisecond
		defer server.Close()

		w := &traceTestWriter{gate: make(chan struct{})}
		cancel, done := startTraceHandler(server, "test", w, nil)
		defer cancel()
		require.Eventually(t, func() bool {
			return len(rec.snapshotAdded()) == 1
		}, 2*time.Second, 10*time.Millisecond)

		for _, data := range []string{"a", "b", "c"} {
			<-server.PublishWithAcknowledgment([]string{"test"}, &publication{data: data})
		}
		require.Eventually(t, func() bool {
			return len(rec.snapshotDropped()) == 1
		}, 2*time.Second, 10*time.Millisecond)

		// Let MaxConnTime elapse while the handler is still stalled, then
		// release it so it unwinds with both exit conditions pending.
		time.Sleep(50 * time.Millisecond)
		close(w.gate)
		waitClosed(t, done)

		removed := rec.snapshotRemoved()
		require.Len(t, removed, 1)
		assert.Equal(t, ReasonBufferOverflow, removed[0].Reason)
		assert.Equal(t, rec.snapshotDropped()[0].SubscriberID, removed[0].SubscriberID)
	})

	t.Run("buffer_overflow wins over client_closed", func(t *testing.T) {
		rec := &traceRecorder{}
		server := NewServer()
		server.Trace = rec.trace()
		server.BufferSize = 1
		defer server.Close()

		w := &traceTestWriter{gate: make(chan struct{})}
		cancel, done := startTraceHandler(server, "test", w, nil)
		require.Eventually(t, func() bool {
			return len(rec.snapshotAdded()) == 1
		}, 2*time.Second, 10*time.Millisecond)

		for _, data := range []string{"a", "b", "c"} {
			<-server.PublishWithAcknowledgment([]string{"test"}, &publication{data: data})
		}
		require.Eventually(t, func() bool {
			return len(rec.snapshotDropped()) == 1
		}, 2*time.Second, 10*time.Millisecond)

		// The client hangs up before the stalled handler has had any chance to
		// observe the closed channel, then the handler unwinds.
		cancel()
		close(w.gate)
		waitClosed(t, done)

		removed := rec.snapshotRemoved()
		require.Len(t, removed, 1)
		assert.Equal(t, ReasonBufferOverflow, removed[0].Reason)
		assert.Equal(t, rec.snapshotDropped()[0].SubscriberID, removed[0].SubscriberID)
	})
}

// slowDoneContext delays the first Done() call. A Server handler calls Done()
// exactly once, between registering its subscription and entering its read
// loop, so the delay deterministically pins the handler in the window where
// the dispatch goroutine attempts the replay batch enqueue.
type slowDoneContext struct {
	context.Context
	delay time.Duration
	once  sync.Once
}

func (c *slowDoneContext) Done() <-chan struct{} {
	c.once.Do(func() { time.Sleep(c.delay) })
	return c.Context.Done()
}

// With an unbuffered subscription (BufferSize zero), enqueueing the replay
// batch marker itself overflows the subscription and drops it. That drop must
// fire SubscriberDropped exactly like the publish-path overflow drop.
func TestServerTraceSubscriberDroppedOnReplayEnqueueOverflow(t *testing.T) {
	rec := &traceRecorder{}
	server := NewServer()
	server.Trace = rec.trace()
	server.BufferSize = 0
	server.ReplayAll = true
	server.Register("test", &testServerRepository{})
	defer server.Close()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	innerCtx, cancel := context.WithCancel(req.Context())
	defer cancel()
	req = req.WithContext(&slowDoneContext{Context: innerCtx, delay: 200 * time.Millisecond})
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.Handler("test")(&traceTestWriter{}, req)
	}()
	waitClosed(t, done)

	dropped := rec.snapshotDropped()
	require.Len(t, dropped, 1, "the replay-enqueue overflow drop must fire SubscriberDropped")
	assert.Equal(t, 0, dropped[0].BufferSize)

	removed := rec.snapshotRemoved()
	require.Len(t, removed, 1)
	assert.Equal(t, ReasonBufferOverflow, removed[0].Reason)
	assert.Equal(t, dropped[0].SubscriberID, removed[0].SubscriberID)
}

// A Server that closes while a handler is between its response start and its
// registration send must not leave that handler parked forever on the send;
// the connection ends and the already-fired SubscriberAdded gets its pairing
// SubscriberRemoved.
func TestServerTraceHandlerExitsWhenServerClosesDuringStartup(t *testing.T) {
	rec := &traceRecorder{}
	gate := make(chan struct{})
	trace := rec.trace()
	inner := trace.SubscriberAdded
	trace.SubscriberAdded = func(ctx context.Context, info SubscriberAddedInfo) {
		inner(ctx, info)
		<-gate
	}
	server := NewServer()
	server.Trace = trace

	cancel, done := startTraceHandler(server, "test", &traceTestWriter{}, nil)
	defer cancel()
	require.Eventually(t, func() bool {
		return len(rec.snapshotAdded()) == 1
	}, 2*time.Second, 10*time.Millisecond)

	// The handler is parked inside SubscriberAdded, before the registration
	// send. Close the Server, then let it proceed to the send.
	server.Close()
	close(gate)
	waitClosed(t, done)

	removed := rec.snapshotRemoved()
	require.Len(t, removed, 1)
	assert.Equal(t, ReasonServerClosed, removed[0].Reason)
	assert.Equal(t, rec.snapshotAdded()[0].SubscriberID, removed[0].SubscriberID)
}

// SubscriberAdded must precede every other callback for its subscriber, even
// when the handler stalls in its initial flush while the dispatch goroutine is
// already delivering (and potentially dropping) events for the subscription.
func TestServerTraceSubscriberAddedPrecedesDropped(t *testing.T) {
	var mu sync.Mutex
	var order []string
	record := func(name string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, name)
	}

	server := NewServer()
	server.BufferSize = 1
	server.Trace = &ServerTrace{
		SubscriberAdded:   func(context.Context, SubscriberAddedInfo) { record("added") },
		SubscriberDropped: func(SubscriberDroppedInfo) { record("dropped") },
	}
	defer server.Close()

	w := &traceTestWriter{gate: make(chan struct{}), flushGate: make(chan struct{})}
	cancel, done := startTraceHandler(server, "test", w, nil)
	defer cancel()

	// Publishing past BufferSize while the handler is parked in its initial
	// flush must not produce any callback that precedes SubscriberAdded: were
	// the subscription registered before SubscriberAdded fired, these would
	// overflow it and fire SubscriberDropped first.
	for _, data := range []string{"early-a", "early-b", "early-c"} {
		<-server.PublishWithAcknowledgment([]string{"test"}, &publication{data: data})
	}
	close(w.flushGate)

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(order) >= 1
	}, 2*time.Second, 10*time.Millisecond)

	// Now stall the first event write and overrun the buffer to force a drop.
	for _, data := range []string{"a", "b", "c"} {
		<-server.PublishWithAcknowledgment([]string{"test"}, &publication{data: data})
	}
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(order) >= 2
	}, 2*time.Second, 10*time.Millisecond)

	close(w.gate)
	waitClosed(t, done)

	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, order)
	assert.Equal(t, "added", order[0],
		"SubscriberAdded must be the first callback for a subscription, got %v", order)
	assert.Contains(t, order, "dropped")
}

// A consumer callback that panics on the handler goroutine ends that
// connection (net/http recovers the panic), but must not unbalance the trace:
// the subscription still unregisters and SubscriberRemoved still fires.
func TestServerTracePanicInHandlerCallbackStillRemovesSubscriber(t *testing.T) {
	rec := &traceRecorder{}
	trace := rec.trace()
	trace.EventSent = func(context.Context, EventSentInfo) {
		panic("consumer bug")
	}
	server := NewServer()
	server.Trace = trace
	defer server.Close()

	httpServer := httptest.NewUnstartedServer(server.Handler("test"))
	// Silence net/http's stderr report of the recovered panic.
	httpServer.Config.ErrorLog = log.New(io.Discard, "", 0)
	httpServer.Start()
	defer httpServer.Close()

	resp, err := http.Get(httpServer.URL)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Eventually(t, func() bool {
		return len(rec.snapshotAdded()) == 1
	}, 2*time.Second, 10*time.Millisecond)

	server.Publish([]string{"test"}, &publication{data: "boom"})

	require.Eventually(t, func() bool {
		return len(rec.snapshotRemoved()) == 1
	}, 2*time.Second, 10*time.Millisecond)
	removed := rec.snapshotRemoved()[0]
	assert.Equal(t, rec.snapshotAdded()[0].SubscriberID, removed.SubscriberID)
	assert.Equal(t, SubscriberRemovedReason(""), removed.Reason,
		"a panic exit has no recorded cause, so Reason is empty")
}

// gatedReplayRepository sends `before` events, parks until gate is closed,
// then sends `after` more and closes the batch. done is closed once the
// producer has sent everything, so a test can prove the producer was not
// stranded by an exiting handler.
type gatedReplayRepository struct {
	before, after int
	gate          chan struct{}
	done          chan struct{}
}

func (r gatedReplayRepository) Replay(channel, id string) chan Event {
	out := make(chan Event)
	go func() {
		defer close(out)
		defer close(r.done)
		for i := 0; i < r.before; i++ {
			out <- &publication{id: fmt.Sprintf("id-%d", i), data: "0123456789"}
		}
		<-r.gate
		for i := 0; i < r.after; i++ {
			out <- &publication{id: fmt.Sprintf("id-after-%d", i), data: "0123456789"}
		}
	}()
	return out
}

// A panic in a callback that the exit-time teardown itself invokes (the
// aborted ReplayFinished here) must not skip the rest of the teardown: the
// Repository producer must still be released and SubscriberRemoved must still
// fire.
func TestServerTracePanicInAbortedReplayReportStillTearsDown(t *testing.T) {
	rec := &traceRecorder{}
	trace := rec.trace()
	trace.ReplayFinished = func(context.Context, ReplayFinishedInfo) {
		panic("consumer bug")
	}
	repo := gatedReplayRepository{
		before: 3,
		after:  2,
		gate:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	server := NewServer()
	server.Trace = trace
	server.ReplayAll = true
	server.Register("test", repo)
	defer server.Close()

	httpServer := httptest.NewUnstartedServer(server.Handler("test"))
	// Silence net/http's stderr report of the recovered panic.
	httpServer.Config.ErrorLog = log.New(io.Discard, "", 0)
	httpServer.Start()
	defer httpServer.Close()

	reqCtx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, httpServer.URL, nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	// The producer is parked at its gate with the batch mid-drain; the client
	// hangs up, so the teardown fires the aborted ReplayFinished, which panics.
	require.Eventually(t, func() bool {
		return len(rec.snapshotReplayStarted()) == 1
	}, 2*time.Second, 10*time.Millisecond)
	cancel()

	require.Eventually(t, func() bool {
		return len(rec.snapshotRemoved()) == 1
	}, 2*time.Second, 10*time.Millisecond, "SubscriberRemoved must fire despite the panicking abort report")

	// The abandoned batch was handed to its drain before the callback panicked,
	// so releasing the gate lets the producer finish instead of stranding it.
	close(repo.gate)
	select {
	case <-repo.done:
	case <-time.After(2 * time.Second):
		require.Fail(t, "the replay producer was stranded by the panicking callback")
	}
}

// The same containment for the other teardown-invoked callback: a panic in
// the connection_ended EventDiscarded must not skip SubscriberRemoved.
func TestServerTracePanicInConnectionEndedDiscardStillTearsDown(t *testing.T) {
	rec := &traceRecorder{}
	var coalesced atomic.Int64
	trace := rec.trace()
	trace.EventDiscarded = func(_ context.Context, info EventDiscardedInfo) {
		if info.Reason == DiscardReasonJitterCoalesce {
			coalesced.Add(1)
			return
		}
		panic("consumer bug")
	}
	server := NewServerWithJitter(time.Hour)
	server.Trace = trace
	defer server.Close()

	httpServer := httptest.NewUnstartedServer(server.Handler("test"))
	httpServer.Config.ErrorLog = log.New(io.Discard, "", 0)
	httpServer.Start()
	defer httpServer.Close()

	reqCtx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, httpServer.URL, nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Eventually(t, func() bool {
		return len(rec.snapshotAdded()) == 1
	}, 2*time.Second, 10*time.Millisecond)

	// The coalesce discard of the second event proves the first is parked, so
	// hanging up now makes the teardown's connection_ended discard panic.
	for _, data := range []string{"parked", "coalesced"} {
		<-server.PublishWithAcknowledgment([]string{"test"}, &publication{data: data})
	}
	require.Eventually(t, func() bool {
		return coalesced.Load() == 1
	}, 2*time.Second, 10*time.Millisecond)
	cancel()

	require.Eventually(t, func() bool {
		return len(rec.snapshotRemoved()) == 1
	}, 2*time.Second, 10*time.Millisecond, "SubscriberRemoved must fire despite the panicking discard report")
}

func TestSinceOrZero(t *testing.T) {
	assert.Equal(t, time.Duration(0), sinceOrZero(time.Time{}),
		"an unmeasured start must map to a zero duration, not an epoch-based one")
	d := sinceOrZero(time.Now().Add(-time.Second))
	assert.True(t, d >= time.Second && d < time.Minute, "got %s", d)
}

// A panic inside the SubscriberAdded callback itself must still produce the
// balancing SubscriberRemoved: the pairing flag is set before the callback.
func TestServerTracePanicInSubscriberAddedStillRemovesSubscriber(t *testing.T) {
	rec := &traceRecorder{}
	trace := rec.trace()
	inner := trace.SubscriberAdded
	trace.SubscriberAdded = func(ctx context.Context, info SubscriberAddedInfo) {
		inner(ctx, info)
		panic("consumer bug")
	}
	server := NewServer()
	server.Trace = trace
	defer server.Close()

	httpServer := httptest.NewUnstartedServer(server.Handler("test"))
	httpServer.Config.ErrorLog = log.New(io.Discard, "", 0)
	httpServer.Start()
	defer httpServer.Close()

	resp, err := http.Get(httpServer.URL)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Eventually(t, func() bool {
		return len(rec.snapshotRemoved()) == 1
	}, 2*time.Second, 10*time.Millisecond)
	removed := rec.snapshotRemoved()[0]
	assert.Equal(t, rec.snapshotAdded()[0].SubscriberID, removed.SubscriberID)
	assert.Equal(t, SubscriberRemovedReason(""), removed.Reason,
		"a panic exit has no recorded cause")
}

// An event that was written when its jitter delay elapsed must not ALSO be
// reported as discarded just because the EventSent callback for it panicked:
// the parking slot is cleared before the write, not after.
func TestServerTracePanicInEventSentDoesNotDoubleReportJitterEvent(t *testing.T) {
	rec := &traceRecorder{}
	trace := rec.trace()
	trace.EventSent = func(context.Context, EventSentInfo) {
		panic("consumer bug")
	}
	server := NewServerWithJitter(20 * time.Millisecond)
	server.Trace = trace
	defer server.Close()

	httpServer := httptest.NewUnstartedServer(server.Handler("test"))
	httpServer.Config.ErrorLog = log.New(io.Discard, "", 0)
	httpServer.Start()
	defer httpServer.Close()

	resp, err := http.Get(httpServer.URL)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Eventually(t, func() bool {
		return len(rec.snapshotAdded()) == 1
	}, 2*time.Second, 10*time.Millisecond)

	// The event parks, its delay elapses, the write succeeds, and EventSent
	// panics on the way out.
	<-server.PublishWithAcknowledgment([]string{"test"}, &publication{data: "x"})
	require.Eventually(t, func() bool {
		return len(rec.snapshotRemoved()) == 1
	}, 2*time.Second, 10*time.Millisecond)

	assert.Empty(t, rec.snapshotDiscarded(),
		"the written event must not also be reported as discarded")
}

// A panic in the SubscriberDropped callback fires on the Server's dispatch
// goroutine, where an unrecovered panic would kill the whole process rather
// than one connection. The Server must contain it and keep dispatching.
func TestServerTracePanicInSubscriberDroppedIsRecovered(t *testing.T) {
	// The channel stands in for a credential; a callback that panics with the
	// info struct it was handed must not put it in the log.
	const channel = "sdk-key-do-not-log"

	rec := &traceRecorder{}
	logger := &captureLogger{}
	trace := rec.trace()
	trace.SubscriberDropped = func(info SubscriberDroppedInfo) {
		panic(info)
	}
	server := NewServer()
	server.Trace = trace
	server.Logger = logger
	server.BufferSize = 1
	defer server.Close()

	w := &traceTestWriter{gate: make(chan struct{})}
	cancel, done := startTraceHandler(server, channel, w, nil)
	defer cancel()
	require.Eventually(t, func() bool {
		return len(rec.snapshotAdded()) == 1
	}, 2*time.Second, 10*time.Millisecond)

	// The third publish overruns the stalled subscriber's buffer, so its own
	// acknowledgment proves the dispatch goroutine survived the panic that its
	// drop triggered.
	for _, data := range []string{"a", "b", "c"} {
		<-server.PublishWithAcknowledgment([]string{channel}, &publication{data: data})
	}
	<-server.PublishWithAcknowledgment([]string{"other"}, &publication{data: "x"})

	assert.True(t, logger.containing("panic in ServerTrace callback (eventsource.SubscriberDroppedInfo)"),
		"the recovered panic should be reported by type")
	assert.False(t, logger.containing(channel),
		"the panic value is consumer-controlled and must not be rendered into the log")

	close(w.gate)
	waitClosed(t, done)

	removed := rec.snapshotRemoved()
	require.Len(t, removed, 1)
	assert.Equal(t, ReasonBufferOverflow, removed[0].Reason)
}

// warnPanickyLogger panics on the drop path's WARN line and on the recover's
// own ERROR report, modelling a broken log sink; other lines pass through.
type warnPanickyLogger struct {
	captureLogger
}

func (l *warnPanickyLogger) Printf(format string, args ...interface{}) {
	if strings.HasPrefix(format, "[WARN]") || strings.HasPrefix(format, "[ERROR]") {
		panic("log sink broken")
	}
	l.captureLogger.Printf(format, args...)
}

// A panicking Logger on the drop path runs on the dispatch goroutine just like
// the SubscriberDropped callback; the recover's own report re-enters that same
// Logger, so both panics must be contained or the process dies.
func TestServerTracePanicInLoggerOnDropPathIsRecovered(t *testing.T) {
	logger := &warnPanickyLogger{}
	server := NewServer()
	server.Logger = logger
	server.BufferSize = 1
	defer server.Close()

	w := &traceTestWriter{gate: make(chan struct{})}
	cancel, done := startTraceHandler(server, "test", w, nil)
	defer cancel()
	require.Eventually(t, func() bool {
		return logger.containing("subscriber added")
	}, 2*time.Second, 10*time.Millisecond)

	for _, data := range []string{"a", "b", "c"} {
		<-server.PublishWithAcknowledgment([]string{"test"}, &publication{data: data})
	}
	// The dispatch goroutine survived both the WARN panic and the ERROR-report
	// panic: it still acknowledges publishes.
	<-server.PublishWithAcknowledgment([]string{"other"}, &publication{data: "x"})

	close(w.gate)
	waitClosed(t, done)
	assert.True(t, logger.containing("subscriber removed"),
		"the handler still completed its teardown")
}

func TestServerTraceEventAndCommentSent(t *testing.T) {
	rec := &traceRecorder{}
	server := NewServer()
	server.Trace = rec.trace()
	defer server.Close()

	// Each write is delayed so the measured duration clears the platform's
	// monotonic clock granularity. On Windows that clock advances on the system
	// timer tick (~15.6ms), so an undelayed in-memory write can correctly measure
	// as zero, and "greater than zero" is not a sound assertion there.
	//
	// The asserted floor is well below the delay on purpose: the same granularity
	// that rounds a fast write down to zero can round a 30ms write down to a
	// single tick, so asserting the full delay would just move the flake.
	// The upper bound catches a measurement that was never started: a duration
	// computed from the zero time.Time reads as centuries, not milliseconds.
	const (
		writeDelay       = 30 * time.Millisecond
		minWriteDuration = 5 * time.Millisecond
		maxWriteDuration = time.Minute
	)
	cancel, done := startTraceHandler(server, "test", &traceTestWriter{delay: writeDelay}, nil)
	defer func() {
		cancel()
		waitClosed(t, done)
	}()
	require.Eventually(t, func() bool {
		return len(rec.snapshotAdded()) == 1
	}, 2*time.Second, 10*time.Millisecond)

	<-server.PublishWithAcknowledgment([]string{"test"}, &publication{event: "put", data: "hello"})
	server.PublishComment([]string{"test"}, "keepalive")

	require.Eventually(t, func() bool {
		return len(rec.snapshotEventsSent()) == 1 && len(rec.snapshotCommentsSent()) == 1
	}, 2*time.Second, 10*time.Millisecond)

	eventsSent := rec.snapshotEventsSent()
	assert.Equal(t, "test", eventsSent[0].Channel)
	assert.Equal(t, "put", eventsSent[0].EventType)
	assert.Equal(t, len("hello"), eventsSent[0].DataSize)
	assert.True(t, eventsSent[0].WriteDuration >= minWriteDuration,
		"expected write duration >= %s, got %s", minWriteDuration, eventsSent[0].WriteDuration)
	assert.True(t, eventsSent[0].WriteDuration < maxWriteDuration,
		"expected a measured write duration, got %s", eventsSent[0].WriteDuration)

	commentsSent := rec.snapshotCommentsSent()
	assert.Equal(t, "test", commentsSent[0].Channel)
	assert.True(t, commentsSent[0].WriteDuration >= minWriteDuration,
		"expected comment write duration >= %s, got %s", minWriteDuration, commentsSent[0].WriteDuration)
	assert.True(t, commentsSent[0].WriteDuration < maxWriteDuration,
		"expected a measured comment write duration, got %s", commentsSent[0].WriteDuration)
}

func TestServerTraceContextFromRequest(t *testing.T) {
	rec := &traceRecorder{}
	server := NewServer()
	server.Trace = rec.trace()
	defer server.Close()

	cancel, done := startTraceHandler(server, "test", &traceTestWriter{}, nil)
	defer func() {
		cancel()
		waitClosed(t, done)
	}()
	require.Eventually(t, func() bool {
		return len(rec.snapshotAdded()) == 1
	}, 2*time.Second, 10*time.Millisecond)

	addedCtx := rec.snapshotAddedCtx()
	require.Len(t, addedCtx, 1)
	assert.Equal(t, "request-scoped-value", addedCtx[0].Value(traceTestCtxKey{}))

	<-server.PublishWithAcknowledgment([]string{"test"}, &publication{event: "put", data: "hello"})
	require.Eventually(t, func() bool {
		return len(rec.snapshotEventSentCtx()) == 1
	}, 2*time.Second, 10*time.Millisecond)

	eventSentCtx := rec.snapshotEventSentCtx()
	require.Len(t, eventSentCtx, 1)
	assert.Equal(t, "request-scoped-value", eventSentCtx[0].Value(traceTestCtxKey{}))
}

func TestServerTraceEventDiscardedByJitter(t *testing.T) {
	rec := &traceRecorder{}
	// A large jitter keeps the first event pending long enough that the events
	// published behind it are coalesced away.
	server := NewServerWithJitter(2 * time.Second)
	server.Trace = rec.trace()
	defer server.Close()

	cancel, done := startTraceHandler(server, "test", &traceTestWriter{}, nil)
	defer func() {
		cancel()
		waitClosed(t, done)
	}()
	require.Eventually(t, func() bool {
		return len(rec.snapshotAdded()) == 1
	}, 2*time.Second, 10*time.Millisecond)

	for _, data := range []string{"first", "second", "third"} {
		<-server.PublishWithAcknowledgment([]string{"test"}, &publication{data: data})
	}

	require.Eventually(t, func() bool {
		return len(rec.snapshotDiscarded()) == 2
	}, 2*time.Second, 10*time.Millisecond)

	for _, info := range rec.snapshotDiscarded() {
		assert.Equal(t, "test", info.Channel)
		assert.Equal(t, DiscardReasonJitterCoalesce, info.Reason)
	}
}

// An event still parked awaiting its jitter delay when the connection ends
// must be reported as discarded, so that every event delivered to a
// subscription is accounted for as either sent or discarded.
func TestServerTraceEventDiscardedOnConnectionEnd(t *testing.T) {
	rec := &traceRecorder{}
	server := NewServerWithJitter(time.Hour)
	server.Trace = rec.trace()
	defer server.Close()

	cancel, done := startTraceHandler(server, "test", &traceTestWriter{}, nil)
	require.Eventually(t, func() bool {
		return len(rec.snapshotAdded()) == 1
	}, 2*time.Second, 10*time.Millisecond)

	// The first event parks; the coalesce discard for the second proves the
	// first is parked before the client hangs up.
	for _, data := range []string{"parked", "coalesced"} {
		<-server.PublishWithAcknowledgment([]string{"test"}, &publication{data: data})
	}
	require.Eventually(t, func() bool {
		return len(rec.snapshotDiscarded()) == 1
	}, 2*time.Second, 10*time.Millisecond)

	cancel()
	waitClosed(t, done)

	discarded := rec.snapshotDiscarded()
	require.Len(t, discarded, 2)
	assert.Equal(t, DiscardReasonJitterCoalesce, discarded[0].Reason)
	assert.Equal(t, DiscardReasonConnectionEnded, discarded[1].Reason)
	assert.Empty(t, rec.snapshotEventsSent(),
		"neither event was written, so both must be accounted for as discards")
}

func TestServerTraceReplay(t *testing.T) {
	rec := &traceRecorder{}
	server := NewServer()
	server.Trace = rec.trace()
	server.ReplayAll = true
	server.Register("test", &testServerRepository{})
	defer server.Close()

	cancel, done := startTraceHandler(server, "test", &traceTestWriter{}, nil)
	defer func() {
		cancel()
		waitClosed(t, done)
	}()

	require.Eventually(t, func() bool {
		return len(rec.snapshotReplayStarted()) == 1 && len(rec.snapshotReplayFinished()) == 1
	}, 2*time.Second, 10*time.Millisecond)

	assert.Equal(t, "test", rec.snapshotReplayStarted()[0].Channel)

	finished := rec.snapshotReplayFinished()
	assert.Equal(t, "test", finished[0].Channel)
	assert.Equal(t, 1, finished[0].EventCount)
	// testServerRepository replays a single event whose data is "example".
	assert.Equal(t, int64(len("example")), finished[0].TotalDataSize)
	// The upper bound catches a drain duration computed from an unmeasured
	// start, which reads as centuries.
	assert.True(t, finished[0].DrainDuration >= 0 && finished[0].DrainDuration < time.Minute,
		"expected a measured drain duration, got %s", finished[0].DrainDuration)

	// Replayed events are flushed once per batch, so EventSent must not fire for
	// them; replay is reported only through ReplayStarted/ReplayFinished.
	assert.Empty(t, rec.snapshotEventsSent())
	assert.False(t, finished[0].Aborted)
}

// listReplayRepository replays one event per element of data, so a test can
// control the exact payload sizes in a batch.
type listReplayRepository struct {
	data []string
}

func (r listReplayRepository) Replay(channel, id string) chan Event {
	out := make(chan Event, len(r.data))
	for i, d := range r.data {
		out <- &publication{id: fmt.Sprintf("id-%d", i), data: d}
	}
	close(out)
	return out
}

// TotalDataSize must be the sum over the whole batch; distinct sizes make an
// implementation that reports only one event's size (first, last, or count
// times either) come out wrong.
func TestServerTraceReplayTotalDataSizeSumsDistinctSizes(t *testing.T) {
	rec := &traceRecorder{}
	server := NewServer()
	server.Trace = rec.trace()
	server.ReplayAll = true
	data := []string{"a", "bcd", "efghi"}
	server.Register("test", listReplayRepository{data: data})
	defer server.Close()

	cancel, done := startTraceHandler(server, "test", &traceTestWriter{}, nil)
	defer func() {
		cancel()
		waitClosed(t, done)
	}()

	require.Eventually(t, func() bool {
		return len(rec.snapshotReplayFinished()) == 1
	}, 2*time.Second, 10*time.Millisecond)

	finished := rec.snapshotReplayFinished()[0]
	assert.Equal(t, len(data), finished.EventCount)
	assert.Equal(t, int64(len("a")+len("bcd")+len("efghi")), finished.TotalDataSize)
	assert.False(t, finished.Aborted)
}

// DrainDuration is documented to include the single flush at the end of the
// batch; a flush that takes real time must show up in the reported duration.
func TestServerTraceReplayDrainDurationIncludesBatchFlush(t *testing.T) {
	const flushDelay = 50 * time.Millisecond

	rec := &traceRecorder{}
	server := NewServer()
	server.Trace = rec.trace()
	server.ReplayAll = true
	server.Register("test", &testServerRepository{})
	defer server.Close()

	cancel, done := startTraceHandler(server, "test", &traceTestWriter{flushDelay: flushDelay}, nil)
	defer func() {
		cancel()
		waitClosed(t, done)
	}()

	require.Eventually(t, func() bool {
		return len(rec.snapshotReplayFinished()) == 1
	}, 2*time.Second, 10*time.Millisecond)

	finished := rec.snapshotReplayFinished()[0]
	assert.True(t, finished.DrainDuration >= flushDelay,
		"DrainDuration must include the end-of-batch flush: expected >= %s, got %s",
		flushDelay, finished.DrainDuration)
	assert.True(t, finished.DrainDuration < time.Minute,
		"expected a measured drain duration, got %s", finished.DrainDuration)
}

// writeEntryGateWriter signals when its first Write is entered and then blocks
// until released, so a test can act at the precise moment the handler is
// mid-write.
type writeEntryGateWriter struct {
	traceTestWriter
	entered chan struct{}
	once    sync.Once
	release chan struct{}
}

func (w *writeEntryGateWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return w.traceTestWriter.Write(p)
}

// A batch that fully drained before the client disconnected must be reported
// as completed even when the disconnect and the end-of-batch sentinel race in
// the read loop's select; mislabeling it aborted would inflate the aborted
// dimension for the common "client takes the payload and drops" pattern.
func TestServerTraceReplayCompletedDespiteDisconnectRace(t *testing.T) {
	// After the write is released, the read loop's select chooses between the
	// disconnect and the already-closed batch channel by coin flip, so a
	// single run only exercises the exit-time reclassification about half the
	// time; iterating makes missing it vanishingly unlikely, and the asserted
	// outcome is identical on both paths.
	for i := 0; i < 20; i++ {
		rec := &traceRecorder{}
		server := NewServer()
		server.Trace = rec.trace()
		server.ReplayAll = true
		// listReplayRepository closes its batch channel up front, so once the
		// single event is consumed only the sentinel remains.
		server.Register("test", listReplayRepository{data: []string{"solitary"}})

		w := &writeEntryGateWriter{entered: make(chan struct{}), release: make(chan struct{})}
		cancel, done := startTraceHandler(server, "test", w, nil)

		// The handler is mid-write of the last event; hanging up now puts the
		// disconnect and the already-closed batch channel in the same select.
		<-w.entered
		cancel()
		close(w.release)
		waitClosed(t, done)

		finished := rec.snapshotReplayFinished()
		require.Len(t, finished, 1)
		assert.False(t, finished[0].Aborted,
			"the batch fully drained; losing the select race to the disconnect must not mislabel it aborted")
		assert.Equal(t, 1, finished[0].EventCount)
		assert.Equal(t, int64(len("solitary")), finished[0].TotalDataSize)
		server.Close()
	}
}

// A replay drain that ends in a write error is aborted by definition, even
// when the batch channel is already closed and empty (the failing write was
// the batch's last event): the events the report counts never all reached the
// connection.
func TestServerTraceReplayWriteErrorIsAlwaysAborted(t *testing.T) {
	rec := &traceRecorder{}
	server := NewServer()
	server.Trace = rec.trace()
	server.ReplayAll = true
	server.Register("test", listReplayRepository{data: []string{"solitary"}})
	defer server.Close()

	w := &traceTestWriter{writeErr: errors.New("boom")}
	cancel, done := startTraceHandler(server, "test", w, nil)
	defer cancel()
	waitClosed(t, done)

	require.Len(t, rec.snapshotWriteErrors(), 1)
	finished := rec.snapshotReplayFinished()
	require.Len(t, finished, 1)
	assert.True(t, finished[0].Aborted,
		"a write-error exit must never be reported as a completed drain")
	assert.Equal(t, 0, finished[0].EventCount)
	removed := rec.snapshotRemoved()
	require.Len(t, removed, 1)
	assert.Equal(t, ReasonWriteError, removed[0].Reason)
}

// With the Server already closed there is no unsubscription path left to
// rescue a stranded producer, so the teardown itself must hand the abandoned
// batch to its drain before running the consumer callback that panics.
func TestServerTracePanicInAbortReportWithClosedServerStillFreesProducer(t *testing.T) {
	rec := &traceRecorder{}
	trace := rec.trace()
	trace.ReplayFinished = func(context.Context, ReplayFinishedInfo) {
		panic("consumer bug")
	}
	repo := gatedReplayRepository{
		before: 3,
		after:  2,
		gate:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	server := NewServer()
	server.Trace = trace
	server.ReplayAll = true
	server.Register("test", repo)

	httpServer := httptest.NewUnstartedServer(server.Handler("test"))
	httpServer.Config.ErrorLog = log.New(io.Discard, "", 0)
	httpServer.Start()
	defer httpServer.Close()

	reqCtx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, httpServer.URL, nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Eventually(t, func() bool {
		return len(rec.snapshotReplayStarted()) == 1
	}, 2*time.Second, 10*time.Millisecond)

	// Close the Server while the client is still connected (its shutdown path
	// leaves the batch to the handler), then hang up: the teardown's aborted
	// ReplayFinished panics with no run() left to drain the batch afterwards.
	server.Close()
	cancel()

	require.Eventually(t, func() bool {
		return len(rec.snapshotRemoved()) == 1
	}, 2*time.Second, 10*time.Millisecond, "SubscriberRemoved must fire despite the panic")

	close(repo.gate)
	select {
	case <-repo.done:
	case <-time.After(2 * time.Second):
		require.Fail(t, "the replay producer was stranded: the teardown must drain before its callbacks")
	}
}

// slowReplayRepository streams a large batch one event at a time through an
// unbuffered channel, so a test can end the connection while the batch is
// still draining.
type slowReplayRepository struct {
	count int
	data  string
}

func (r slowReplayRepository) Replay(channel, id string) chan Event {
	out := make(chan Event)
	go func() {
		defer close(out)
		for i := 0; i < r.count; i++ {
			out <- &publication{id: fmt.Sprintf("id-%d", i), data: r.data}
			time.Sleep(100 * time.Microsecond)
		}
	}()
	return out
}

func TestServerTraceReplayAborted(t *testing.T) {
	rec := &traceRecorder{}
	server := NewServer()
	server.Trace = rec.trace()
	server.ReplayAll = true
	// Large enough that the cancel below lands mid-batch, small enough that
	// the post-test background drain finishes promptly.
	repo := slowReplayRepository{count: 2000, data: "0123456789"}
	server.Register("test", repo)
	defer server.Close()

	w := &traceTestWriter{}
	cancel, done := startTraceHandler(server, "test", w, nil)

	// Wait until the batch is genuinely mid-drain: the replay has started and
	// some replayed events have been encoded to the connection.
	require.Eventually(t, func() bool {
		return len(rec.snapshotReplayStarted()) == 1 && w.bytesWritten() > 0
	}, 5*time.Second, time.Millisecond)

	cancel() // the client hangs up mid-batch
	waitClosed(t, done)

	finished := rec.snapshotReplayFinished()
	require.Len(t, finished, 1, "an aborted batch must still report ReplayFinished")
	assert.True(t, finished[0].Aborted)
	assert.Equal(t, "test", finished[0].Channel)
	assert.Greater(t, finished[0].EventCount, 0)
	assert.Less(t, finished[0].EventCount, repo.count)
	assert.Equal(t, int64(finished[0].EventCount*len(repo.data)), finished[0].TotalDataSize)
	assert.Empty(t, rec.snapshotEventsSent())
}

// countingDataEvent counts Data() invocations, so a test can pin how many
// times the server reads a caller-supplied payload.
type countingDataEvent struct {
	data  string
	calls *atomic.Int64
}

func (e *countingDataEvent) Id() string    { return "1" } //nolint:revive // required by Event
func (e *countingDataEvent) Event() string { return "" }
func (e *countingDataEvent) Data() string {
	e.calls.Add(1)
	return e.data
}

type countingReplayRepository struct {
	calls *atomic.Int64
	count int
	data  string
}

func (r countingReplayRepository) Replay(channel, id string) chan Event {
	out := make(chan Event, r.count)
	for i := 0; i < r.count; i++ {
		out <- &countingDataEvent{data: r.data, calls: r.calls}
	}
	close(out)
	return out
}

// The replay byte accounting reads Event.Data() a second time per event (the
// encoder's own read is the first). Data() is caller-supplied code with no
// cost contract, so that second read must happen only when something consumes
// the sum: the ReplayFinished callback or a Logger.
func TestServerTraceReplayDataCallGating(t *testing.T) {
	run := func(t *testing.T, configure func(*Server)) int64 {
		var calls atomic.Int64
		server := NewServer()
		server.ReplayAll = true
		configure(server)
		server.Register("test", countingReplayRepository{calls: &calls, count: 5, data: "0123456789"})
		defer server.Close()

		w := &traceTestWriter{}
		cancel, done := startTraceHandler(server, "test", w, nil)
		defer func() {
			cancel()
			waitClosed(t, done)
		}()

		// Replay output appearing proves the subscription is registered and the
		// batch enqueued, so the marker published next is ordered behind the
		// batch. A live marker event is delivered only after the replay batch
		// has been fully drained, so its appearance means every batch Data()
		// call has happened.
		require.Eventually(t, func() bool {
			return w.contains("0123456789")
		}, 2*time.Second, time.Millisecond)
		<-server.PublishWithAcknowledgment([]string{"test"}, &publication{data: "marker"})
		require.Eventually(t, func() bool {
			return w.contains("marker")
		}, 2*time.Second, time.Millisecond)

		return calls.Load()
	}

	t.Run("no replay consumer reads Data once per event", func(t *testing.T) {
		calls := run(t, func(s *Server) {
			s.Trace = &ServerTrace{SubscriberAdded: func(context.Context, SubscriberAddedInfo) {}}
		})
		assert.Equal(t, int64(5), calls,
			"with neither ReplayFinished nor a Logger set, only the encoder should read Data()")
	})

	t.Run("ReplayFinished consumer reads Data twice per event", func(t *testing.T) {
		calls := run(t, func(s *Server) {
			s.Trace = &ServerTrace{ReplayFinished: func(context.Context, ReplayFinishedInfo) {}}
		})
		assert.Equal(t, int64(10), calls)
	})
}

func TestServerTraceNilCallbacksAreNoOps(t *testing.T) {
	t.Run("nil trace pointer", func(t *testing.T) {
		server := NewServer()
		defer server.Close()

		cancel, done := startTraceHandler(server, "test", &traceTestWriter{}, nil)
		<-server.PublishWithAcknowledgment([]string{"test"}, &publication{data: "x"})
		server.PublishComment([]string{"test"}, "c")
		cancel()
		waitClosed(t, done)
	})

	t.Run("all nil fields", func(t *testing.T) {
		server := NewServer()
		server.Trace = &ServerTrace{}
		defer server.Close()

		cancel, done := startTraceHandler(server, "test", &traceTestWriter{}, nil)
		<-server.PublishWithAcknowledgment([]string{"test"}, &publication{data: "x"})
		server.PublishComment([]string{"test"}, "c")
		cancel()
		waitClosed(t, done)
	})

	t.Run("only some fields set", func(t *testing.T) {
		var addedCount int
		var mu sync.Mutex
		server := NewServer()
		server.Trace = &ServerTrace{
			SubscriberAdded: func(context.Context, SubscriberAddedInfo) {
				mu.Lock()
				defer mu.Unlock()
				addedCount++
			},
		}
		defer server.Close()

		cancel, done := startTraceHandler(server, "test", &traceTestWriter{}, nil)
		require.Eventually(t, func() bool {
			mu.Lock()
			defer mu.Unlock()
			return addedCount == 1
		}, 2*time.Second, 10*time.Millisecond)

		// SubscriberRemoved and the rest are nil; triggering them must not panic.
		<-server.PublishWithAcknowledgment([]string{"test"}, &publication{data: "x"})
		cancel()
		waitClosed(t, done)
	})
}

func TestServerTraceLogging(t *testing.T) {
	// The channel name stands in for a value that must never be logged; in
	// ld-relay the channel is a raw SDK credential.
	const sensitiveChannel = "sdk-key-do-not-log"

	t.Run("subscriber added and removed at debug", func(t *testing.T) {
		logger := &captureLogger{}
		server := NewServer()
		server.Logger = logger
		defer server.Close()

		cancel, done := startTraceHandler(server, sensitiveChannel, &traceTestWriter{}, nil)
		require.Eventually(t, func() bool {
			return logger.containing("[DEBUG] eventsource: subscriber added (id=1")
		}, 2*time.Second, 10*time.Millisecond)

		cancel()
		waitClosed(t, done)

		assert.True(t, logger.containing("[DEBUG] eventsource: subscriber removed (id=1"))
		assert.False(t, logger.containing(sensitiveChannel),
			"log lines must never include the channel name")
	})

	t.Run("buffer overflow at warn", func(t *testing.T) {
		logger := &captureLogger{}
		server := NewServer()
		server.Logger = logger
		server.BufferSize = 1
		defer server.Close()

		w := &traceTestWriter{gate: make(chan struct{})}
		cancel, done := startTraceHandler(server, sensitiveChannel, w, nil)
		defer cancel()
		require.Eventually(t, func() bool {
			return logger.containing("[DEBUG] eventsource: subscriber added")
		}, 2*time.Second, 10*time.Millisecond)

		for _, data := range []string{"a", "b", "c"} {
			<-server.PublishWithAcknowledgment([]string{sensitiveChannel}, &publication{data: data})
		}

		require.Eventually(t, func() bool {
			return logger.containing("[WARN] eventsource: dropped subscriber (id=1")
		}, 2*time.Second, 10*time.Millisecond)

		close(w.gate)
		waitClosed(t, done)

		assert.False(t, logger.containing(sensitiveChannel),
			"log lines must never include the channel name")
	})

	t.Run("replay drained at debug", func(t *testing.T) {
		logger := &captureLogger{}
		server := NewServer()
		server.Logger = logger
		server.ReplayAll = true
		server.Register(sensitiveChannel, &testServerRepository{})
		defer server.Close()

		cancel, done := startTraceHandler(server, sensitiveChannel, &traceTestWriter{}, nil)
		defer func() {
			cancel()
			waitClosed(t, done)
		}()

		require.Eventually(t, func() bool {
			return logger.containing("[DEBUG] eventsource: replay drained (id=1")
		}, 2*time.Second, 10*time.Millisecond)

		assert.False(t, logger.containing(sensitiveChannel),
			"log lines must never include the channel name")
	})
}
