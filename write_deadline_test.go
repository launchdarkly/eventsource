package eventsource

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// deadlineRecorder is a ResponseWriter that ResponseController can set deadlines on and
// flush with an error.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	mu        sync.Mutex
	deadlines []time.Time
	setErr    error
	clearErr  error
	flushErr  error
	onWrite   func([]byte)
}

func (w *deadlineRecorder) Write(p []byte) (int, error) {
	if w.onWrite != nil {
		w.onWrite(p)
	}
	return w.ResponseRecorder.Write(p)
}

func (w *deadlineRecorder) WriteString(s string) (int, error) {
	return w.Write([]byte(s))
}

func newDeadlineRecorder() *deadlineRecorder {
	return &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
}

func (w *deadlineRecorder) SetWriteDeadline(t time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.deadlines = append(w.deadlines, t)
	if t.IsZero() {
		return w.clearErr
	}
	return w.setErr
}

func (w *deadlineRecorder) FlushError() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.flushErr
}

func (w *deadlineRecorder) failFlushes(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.flushErr = err
}

func (w *deadlineRecorder) snapshotDeadlines() []time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]time.Time(nil), w.deadlines...)
}

func TestWriteDeadlineArmsAndClears(t *testing.T) {
	w := newDeadlineRecorder()
	d := newWriteDeadline(w, time.Minute)
	before := time.Now()
	require.NoError(t, d.arm())
	require.NoError(t, d.clear())
	require.NoError(t, d.clear(), "a second clear with nothing armed is a no-op")

	deadlines := w.snapshotDeadlines()
	require.Len(t, deadlines, 2)
	assert.False(t, deadlines[0].Before(before.Add(time.Minute)))
	assert.True(t, deadlines[1].IsZero())
}

func TestWriteDeadlineZeroTimeoutNeverTouchesDeadline(t *testing.T) {
	w := newDeadlineRecorder()
	d := newWriteDeadline(w, 0)
	require.NoError(t, d.arm())
	require.NoError(t, d.clear())
	assert.Empty(t, w.snapshotDeadlines())
}

func TestWriteDeadlineUnsupportedIsProbedOnce(t *testing.T) {
	d := newWriteDeadline(httptest.NewRecorder(), time.Minute)
	require.NoError(t, d.arm())
	assert.True(t, d.unsupported)
	assert.False(t, d.armed)
	require.NoError(t, d.arm())
	require.NoError(t, d.clear())
}

func TestWriteDeadlineArmFailureIsAnError(t *testing.T) {
	w := newDeadlineRecorder()
	w.setErr = net.ErrClosed
	d := newWriteDeadline(w, time.Minute)
	err := d.arm()
	require.True(t, errors.Is(err, net.ErrClosed), "got %v", err)
	assert.False(t, d.unsupported, "a transient failure must not disable the timeout")
}

func TestWriteDeadlineClearFailureIsAnError(t *testing.T) {
	w := newDeadlineRecorder()
	w.clearErr = net.ErrClosed
	d := newWriteDeadline(w, time.Minute)
	require.NoError(t, d.arm())
	err := d.clear()
	require.True(t, errors.Is(err, net.ErrClosed), "got %v", err)
}

func TestWriteDeadlineFlushReportsErrorWithoutTimeout(t *testing.T) {
	w := newDeadlineRecorder()
	w.flushErr = errors.New("sad")
	d := newWriteDeadline(w, 0)
	err := d.flush()
	require.True(t, errors.Is(err, w.flushErr), "got %v", err)
	assert.Contains(t, err.Error(), "eventsource flush")
}

// One set and one clear per unit of output: the header flush plus each event, regardless of
// how many underlying Write calls an event takes.
func TestServerWriteTimeoutArmsOncePerEvent(t *testing.T) {
	channel := "test"
	rec := &traceRecorder{}
	server := NewServer()
	server.WriteTimeout = time.Minute
	server.Trace = rec.trace()
	defer server.Close()

	w := newDeadlineRecorder()
	cancel, done := startTraceHandler(server, channel, w, nil)
	publishUntilSent(t, server, channel, rec, &publication{id: "1", event: "e", data: "line1\nline2\nline3"})
	cancel()
	waitClosed(t, done)

	// The last call is the deadline left armed for net/http's trailing flush.
	deadlines := w.snapshotDeadlines()
	assert.Len(t, deadlines, 2*(1+len(rec.snapshotEventsSent()))+1)
	for i, d := range deadlines {
		assert.Equal(t, i%2 == 1, d.IsZero(), "call %d", i)
	}
}

func TestServerHeaderFlushErrorEndsHandlerBeforeRegistration(t *testing.T) {
	channel := "test"
	rec := &traceRecorder{}
	server := NewServer()
	server.Trace = rec.trace()
	defer server.Close()

	w := newDeadlineRecorder()
	w.flushErr = errors.New("gone")
	_, done := startTraceHandler(server, channel, w, nil)
	waitClosed(t, done)

	writeErrors := rec.snapshotWriteErrors()
	require.Len(t, writeErrors, 1)
	assert.True(t, errors.Is(writeErrors[0].Err, w.flushErr), "got %v", writeErrors[0].Err)
	assert.Empty(t, rec.snapshotAdded())
}

// orderTrace wraps rec's trace to also record the order of WriteError and ReplayFinished.
func orderTrace(rec *traceRecorder) (*ServerTrace, func() []string) {
	var mu sync.Mutex
	var order []string
	trace := rec.trace()
	recordWriteError, recordReplayFinished := trace.WriteError, trace.ReplayFinished
	trace.WriteError = func(ctx context.Context, info WriteErrorInfo) {
		recordWriteError(ctx, info)
		mu.Lock()
		defer mu.Unlock()
		order = append(order, "WriteError")
	}
	trace.ReplayFinished = func(ctx context.Context, info ReplayFinishedInfo) {
		recordReplayFinished(ctx, info)
		mu.Lock()
		defer mu.Unlock()
		order = append(order, "ReplayFinished")
	}
	return trace, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), order...)
	}
}

func TestServerFailedReplayFlushAbortsBatch(t *testing.T) {
	channel := "test"
	rec := &traceRecorder{}
	w := newDeadlineRecorder()
	flushErr := errors.New("gone")
	trace, order := orderTrace(rec)
	recordAdded := trace.SubscriberAdded
	// Fires after the header flush and before the replay, so only the end-of-batch flush fails.
	trace.SubscriberAdded = func(ctx context.Context, info SubscriberAddedInfo) {
		recordAdded(ctx, info)
		w.failFlushes(flushErr)
	}
	server := NewServer()
	server.Trace = trace
	server.ReplayAll = true
	server.Register(channel, &testServerRepository{})
	defer server.Close()

	_, done := startTraceHandler(server, channel, w, nil)
	waitClosed(t, done)

	finished := rec.snapshotReplayFinished()
	require.Len(t, finished, 1)
	assert.True(t, finished[0].Aborted)
	assert.Equal(t, 1, finished[0].EventCount)
	writeErrors := rec.snapshotWriteErrors()
	require.Len(t, writeErrors, 1)
	assert.True(t, errors.Is(writeErrors[0].Err, flushErr), "got %v", writeErrors[0].Err)
	assert.Equal(t, []string{"WriteError", "ReplayFinished"}, order())
	removed := rec.snapshotRemoved()
	require.Len(t, removed, 1)
	assert.Equal(t, ReasonWriteError, removed[0].Reason)
}

// The request is cancelled while the replayed event is written, so the read loop's select
// races the batch-end sentinel against the cancellation. Whichever path wins, the failed
// end-of-batch flush must abort the batch.
func TestServerFailedReplayFlushAbortsBatchWhenDisconnectRacesBatchEnd(t *testing.T) {
	for i := 0; i < 40; i++ {
		channel := "test"
		rec := &traceRecorder{}
		w := newDeadlineRecorder()
		trace, order := orderTrace(rec)
		recordAdded := trace.SubscriberAdded
		trace.SubscriberAdded = func(ctx context.Context, info SubscriberAddedInfo) {
			recordAdded(ctx, info)
			w.failFlushes(errors.New("gone"))
		}
		server := NewServer()
		server.Trace = trace
		server.ReplayAll = true
		server.Register(channel, &testServerRepository{})

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		ctx, cancel := context.WithCancel(req.Context())
		w.onWrite = func(p []byte) {
			if bytes.Contains(p, []byte("replayed-from-start")) {
				// Give the Server time to close the batch, so both select cases are ready.
				time.Sleep(20 * time.Millisecond)
				cancel()
			}
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			server.Handler(channel)(w, req.WithContext(ctx))
		}()
		waitClosed(t, done)
		server.Close()

		finished := rec.snapshotReplayFinished()
		require.Len(t, finished, 1, "iteration %d", i)
		require.True(t, finished[0].Aborted, "iteration %d: batch reported complete though its flush failed", i)
		require.Equal(t, []string{"WriteError", "ReplayFinished"}, order(), "iteration %d", i)
	}
}

// A subscriber registers only after SubscriberAdded fires, so an event published in between
// reaches nobody; republish until one is sent, waiting long enough between rounds that a
// slow writer does not pile up a backlog.
func publishUntilSent(t *testing.T, server *Server, channel string, rec *traceRecorder, ev Event) {
	t.Helper()
	deadline := time.Now().Add(writeTimeoutTestDeadline)
	for time.Now().Before(deadline) {
		<-server.PublishWithAcknowledgment([]string{channel}, ev)
		round := time.Now().Add(500 * time.Millisecond)
		for time.Now().Before(round) {
			if len(rec.snapshotEventsSent()) > 0 {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	t.Fatalf("no event sent within %s", writeTimeoutTestDeadline)
}
