package eventsource

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/launchdarkly/go-test-helpers/v3/httphelpers"
)

func toPublication(e httphelpers.SSEEvent) *publication {
	return &publication{
		id:    e.ID,
		event: e.Event,
		data:  e.Data,
	}
}

func TestStreamRestart(t *testing.T) {
	streamHandler1, streamControl1 := httphelpers.SSEHandler(nil)
	defer streamControl1.Close()
	streamHandler2, streamControl2 := httphelpers.SSEHandler(nil)
	defer streamControl2.Close()
	httpServer := httptest.NewServer(httphelpers.SequentialHandler(streamHandler1, streamHandler2))
	defer httpServer.Close()

	stream := mustSubscribe(t, httpServer.URL,
		StreamOptionInitialRetry(time.Millisecond))
	defer stream.Close()

	eventIn1 := httphelpers.SSEEvent{ID: "123"}
	streamControl1.Enqueue(eventIn1)
	eventOut1 := <-stream.Events
	assert.Equal(t, "123", eventOut1.Id())

	stream.Restart()

	eventIn2 := httphelpers.SSEEvent{ID: "456"}
	streamControl2.Enqueue(eventIn2)
	eventOut2 := <-stream.Events // received an event from streamHandler2
	assert.Equal(t, "456", eventOut2.Id())

	assert.Equal(t, 0, len(stream.Errors)) // restart is not reported as an error
}

func TestStreamClose(t *testing.T) {
	streamHandler, streamControl := httphelpers.SSEHandler(nil)
	defer streamControl.Close()
	httpServer := httptest.NewServer(streamHandler)
	defer httpServer.Close()

	stream := mustSubscribe(t, httpServer.URL)
	stream.Close()
	// it's safe to Close the stream multiple times
	stream.Close()

	select {
	case _, ok := <-stream.Events:
		if ok {
			t.Error("Expected stream.Events channel to be closed. Is still open.")
		}
	case <-time.After(timeToWaitForEvent):
		t.Error("Timed out waiting for stream.Events channel to close")
	}

	select {
	case _, ok := <-stream.Errors:
		if ok {
			t.Error("Expected stream.Errors channel to be closed. Is still open.")
		}
	case <-time.After(timeToWaitForEvent):
		t.Error("Timed out waiting for stream.Errors channel to close")
	}
}

// Cancelling the request's context on an established stream terminates the stream
// through the same path as Stream.Close: Events and Errors channels close, no
// goroutine is left waiting.
func TestStreamContextCancellationClosesEstablishedStream(t *testing.T) {
	streamHandler, streamControl := httphelpers.SSEHandler(nil)
	defer streamControl.Close()
	httpServer := httptest.NewServer(streamHandler)
	defer httpServer.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "GET", httpServer.URL, nil)
	assert.NoError(t, err)

	stream, err := SubscribeWithRequestAndOptions(req, StreamOptionHTTPClient(newIsolatedClient(t)))
	assert.NoError(t, err)
	defer stream.Close()

	// Deliver one event so we know the stream is fully established.
	streamControl.Enqueue(httphelpers.SSEEvent{ID: "123"})
	select {
	case <-stream.Events:
	case <-time.After(timeToWaitForEvent):
		t.Fatal("Timed out waiting for initial event")
	}

	cancel()

	// Both channels must close as if Close had been called.
	select {
	case _, ok := <-stream.Events:
		if ok {
			t.Error("Expected stream.Events channel to be closed after ctx cancel")
		}
	case <-time.After(timeToWaitForEvent):
		t.Error("Timed out waiting for stream.Events channel to close after ctx cancel")
	}
	select {
	case _, ok := <-stream.Errors:
		if ok {
			t.Error("Expected stream.Errors channel to be closed after ctx cancel")
		}
	case <-time.After(timeToWaitForEvent):
		t.Error("Timed out waiting for stream.Errors channel to close after ctx cancel")
	}
}

// Cancelling the context while the stream is in a mid-life reconnect sleep
// interrupts that sleep and terminates the stream promptly, rather than
// waiting for the retry delay to elapse.
func TestStreamContextCancellationInterruptsReconnectSleep(t *testing.T) {
	streamHandler, streamControl := httphelpers.SSEHandler(nil)
	defer streamControl.Close()
	httpServer := httptest.NewServer(streamHandler)
	defer httpServer.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "GET", httpServer.URL, nil)
	assert.NoError(t, err)

	// Very long reconnect delay: if the sleep isn't interruptible, the test
	// will hang past its 100ms wait budget below.
	stream, err := SubscribeWithRequestAndOptions(req,
		StreamOptionHTTPClient(newIsolatedClient(t)),
		StreamOptionInitialRetry(time.Hour))
	assert.NoError(t, err)
	defer stream.Close()

	streamControl.Enqueue(httphelpers.SSEEvent{ID: "123"})
	select {
	case <-stream.Events:
	case <-time.After(timeToWaitForEvent):
		t.Fatal("Timed out waiting for initial event")
	}

	// Force the stream into reconnect state.
	streamControl.EndAll()
	select {
	case <-stream.Errors:
	case <-time.After(timeToWaitForEvent):
		t.Fatal("Timed out waiting for post-disconnect error")
	}

	// Now the stream should be sitting in a 1h reconnect sleep. Cancel the
	// context and expect the loop to bail promptly.
	cancel()

	select {
	case _, ok := <-stream.Events:
		if ok {
			t.Error("Expected stream.Events channel to be closed after ctx cancel")
		}
	case <-time.After(timeToWaitForEvent):
		t.Error("Timed out waiting for stream.Events channel to close after ctx cancel")
	}
}

// The error handler must not be invoked with a context.Canceled error caused
// by the caller cancelling the request's context. The caller has already
// decided to abandon; a spurious "your stream failed" callback would be noise.
func TestStreamContextCancellationSkipsErrorHandlerPostConnect(t *testing.T) {
	streamHandler, streamControl := httphelpers.SSEHandler(nil)
	defer streamControl.Close()
	httpServer := httptest.NewServer(streamHandler)
	defer httpServer.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "GET", httpServer.URL, nil)
	assert.NoError(t, err)

	var handlerCalls atomic.Int32
	stream, err := SubscribeWithRequestAndOptions(req,
		StreamOptionHTTPClient(newIsolatedClient(t)),
		StreamOptionErrorHandler(func(err error) StreamErrorHandlerResult {
			handlerCalls.Add(1)
			return StreamErrorHandlerResult{}
		}))
	assert.NoError(t, err)
	defer stream.Close()

	streamControl.Enqueue(httphelpers.SSEEvent{ID: "123"})
	select {
	case <-stream.Events:
	case <-time.After(timeToWaitForEvent):
		t.Fatal("Timed out waiting for initial event")
	}

	cancel()

	// Wait for the stream to actually shut down before checking the count,
	// otherwise a late errorHandler call could race the assertion.
	select {
	case _, ok := <-stream.Events:
		assert.False(t, ok, "Events channel should have closed")
	case <-time.After(timeToWaitForEvent):
		t.Fatal("Timed out waiting for stream to close")
	}

	assert.Equal(t, int32(0), handlerCalls.Load(),
		"error handler should not be invoked when the caller cancelled the context")
}

// Post-connect reconnect path: after a successful initial connect, the stream
// disconnects, the SDK enters mid-life reconnect handling, and the caller
// cancels the request context. The reconnect connect() call (or the reconnect
// sleep) sees the cancellation; the caller's errorHandler must NOT be invoked
// with a context.Canceled error, mirroring the behavior for a direct
// stream.Close.
func TestStreamContextCancellationDuringReconnectSkipsErrorHandler(t *testing.T) {
	streamHandler, streamControl := httphelpers.SSEHandler(nil)
	defer streamControl.Close()
	// First request establishes the stream; subsequent requests return 401
	// so a reconnect attempt is guaranteed to fail with a non-ctx error if
	// it ever completes.
	combined := httphelpers.SequentialHandler(streamHandler,
		handlerCausingHTTPError(401, nil), handlerCausingHTTPError(401, nil))
	httpServer := httptest.NewServer(combined)
	defer httpServer.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "GET", httpServer.URL, nil)
	assert.NoError(t, err)

	var handlerSawCanceled atomic.Bool
	stream, err := SubscribeWithRequestAndOptions(req,
		StreamOptionHTTPClient(newIsolatedClient(t)),
		StreamOptionInitialRetry(20*time.Millisecond),
		StreamOptionErrorHandler(func(err error) StreamErrorHandlerResult {
			if errors.Is(err, context.Canceled) {
				handlerSawCanceled.Store(true)
			}
			return StreamErrorHandlerResult{}
		}))
	assert.NoError(t, err)
	defer stream.Close()

	// Confirm the stream is fully established.
	streamControl.Enqueue(httphelpers.SSEEvent{ID: "123"})
	select {
	case <-stream.Events:
	case <-time.After(timeToWaitForEvent):
		t.Fatal("Timed out waiting for initial event")
	}

	// Force reconnect and cancel ctx during the reconnect handling window.
	streamControl.EndAll()
	time.Sleep(10 * time.Millisecond) // let the disconnect enter the retry state
	cancel()

	// Wait for the stream to fully shut down.
	select {
	case _, ok := <-stream.Events:
		assert.False(t, ok, "expected stream.Events to be closed")
	case <-time.After(timeToWaitForEvent):
		t.Fatal("stream did not close after ctx cancel")
	}

	assert.False(t, handlerSawCanceled.Load(),
		"errorHandler must not be invoked with context.Canceled during a reconnect")
}

// After Close, the AfterFunc listener installed on the caller's context must
// be released so the Stream becomes eligible for garbage collection. If the
// listener isn't released, the parent ctx's children map retains a reference
// to the Stream via the AfterFunc closure, defeating GC. Verified indirectly
// via runtime.SetFinalizer: after Close, dropping our Stream reference should
// allow the finalizer to run within a bounded number of GC cycles.
func TestStreamCloseReleasesAfterFuncListener(t *testing.T) {
	streamHandler, streamControl := httphelpers.SSEHandler(nil)
	defer streamControl.Close()
	httpServer := httptest.NewServer(streamHandler)
	defer httpServer.Close()

	// Long-lived context - if the AfterFunc listener isn't released on Close,
	// the parent ctx would keep the Stream alive as long as ctx itself lives.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", httpServer.URL, nil)
	assert.NoError(t, err)

	finalized := make(chan struct{})

	// Run Subscribe + Close in a nested scope so the local Stream reference
	// goes out of scope after this function returns, giving GC a chance to
	// reclaim it.
	func() {
		stream, err := SubscribeWithRequestAndOptions(req, StreamOptionHTTPClient(newIsolatedClient(t)))
		assert.NoError(t, err)
		runtime.SetFinalizer(stream, func(*Stream) { close(finalized) })
		stream.Close()
		// Wait for stream.stream() to exit before dropping our reference;
		// otherwise the goroutine's stack still holds the Stream alive.
		<-stream.Events
	}()

	// Give the GC several attempts. Finalizers aren't guaranteed to run
	// synchronously with GC, but on modern Go they typically do within a
	// few cycles.
	for i := 0; i < 20; i++ {
		runtime.GC()
		select {
		case <-finalized:
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatal("Stream not GC'd within 1s after Close; " +
		"AfterFunc listener is likely still retained on the parent context")
}

// If the caller stops draining stream.Events and then calls Close, the stream
// must shut down cleanly rather than deadlock on the internal Events send.
// The main-loop send arm is wrapped in a select with <-stream.closer so Close
// unblocks it; without that select, this goroutine would sit forever blocked
// on the send, unresponsive to Close.
func TestStreamCloseUnblocksStalledEventsSend(t *testing.T) {
	streamHandler, streamControl := httphelpers.SSEHandler(nil)
	defer streamControl.Close()
	httpServer := httptest.NewServer(streamHandler)
	defer httpServer.Close()

	req, err := http.NewRequest("GET", httpServer.URL, nil)
	assert.NoError(t, err)
	stream, err := SubscribeWithRequestAndOptions(req, StreamOptionHTTPClient(newIsolatedClient(t)))
	assert.NoError(t, err)

	// Enqueue an event but never drain stream.Events. The decoder produces
	// the event, the main loop accepts it into local `ev`, and then blocks
	// on `stream.Events <- ev` because no caller is reading.
	streamControl.Enqueue(httphelpers.SSEEvent{ID: "123"})

	// Give the stream loop time to reach the blocking send.
	time.Sleep(50 * time.Millisecond)

	// Close must unblock the stalled send by racing the closer arm against the
	// send arm. With the caller not reading, the send arm cannot win, so the
	// closer arm fires and the queued event is discarded.
	stream.Close()

	// Expect the channel to be closed WITHOUT the queued event being
	// delivered. If the internal send arm weren't wrapped in a select with
	// closer, this read would unblock the stalled send and receive the event
	// (ok=true) - the assertion below would then fail.
	select {
	case _, ok := <-stream.Events:
		assert.False(t, ok, "expected stream.Events to be closed without delivering the queued event; "+
			"receiving an event here means the send arm was not wrapped in a select with closer")
	case <-time.After(timeToWaitForEvent):
		t.Fatal("Timed out waiting for stream.Events to close after Close")
	}
}

func TestStreamCloseWhileReconnecting(t *testing.T) {
	streamHandler, streamControl := httphelpers.SSEHandler(nil)
	defer streamControl.Close()
	httpServer := httptest.NewServer(streamHandler)
	defer httpServer.Close()

	stream := mustSubscribe(t, httpServer.URL, StreamOptionInitialRetry(time.Hour))
	defer stream.Close()

	streamControl.Enqueue(httphelpers.SSEEvent{ID: "123"})

	select {
	case <-stream.Events:
	case <-time.After(timeToWaitForEvent):
		t.Error("Timed out waiting for event")
		return
	}

	streamControl.EndAll()

	// Expect at least one error
	select {
	case <-stream.Errors:
	case <-time.After(timeToWaitForEvent):
		t.Error("Timed out waiting for event")
		return
	}

	stream.Close()

	select {
	case _, ok := <-stream.Events:
		if ok {
			t.Error("Expected stream.Events channel to be closed. Is still open.")
		}
	case <-time.After(timeToWaitForEvent):
		t.Error("Timed out waiting for stream.Events channel to close")
	}

	select {
	case _, ok := <-stream.Errors:
		if ok {
			t.Error("Expected stream.Errors channel to be closed. Is still open.")
		}
	case <-time.After(timeToWaitForEvent):
		t.Error("Timed out waiting for stream.Errors channel to close")
	}
}
