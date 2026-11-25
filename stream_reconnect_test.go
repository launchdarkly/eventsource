package eventsource

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/launchdarkly/go-test-helpers/v3/httphelpers"
)

func TestStreamReconnectsIfConnectionIsBroken(t *testing.T) {
	streamHandler, streamControl := httphelpers.SSEHandler(nil)
	defer streamControl.Close()
	httpServer := httptest.NewServer(streamHandler)
	defer httpServer.Close()

	stream := mustSubscribe(t, httpServer.URL, StreamOptionInitialRetry(time.Millisecond))
	defer stream.Close()

	event := httphelpers.SSEEvent{ID: "123"}
	streamControl.Enqueue(event)

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

	streamControl.Enqueue(event)

	// Consume errors until we've got another event
	for {
		select {
		case <-stream.Errors:
		case <-time.After(2 * time.Second):
			t.Error("Timed out waiting for event")
			return
		case receivedEvent := <-stream.Events:
			receivedWithLastId := receivedEvent.(EventWithLastID)
			receivedWithHeaders := receivedEvent.(EventWithHeaders)
			assert.Equal(t, "123", receivedEvent.Id())
			assert.Equal(t, "123", receivedWithLastId.LastEventID())
			assert.NotEmpty(t, receivedWithHeaders.Headers())
			return
		}
	}
}

func TestStreamCanUseBackoff(t *testing.T) {
	streamHandler, streamControl := httphelpers.SSEHandler(nil)
	defer streamControl.Close()
	httpServer := httptest.NewServer(streamHandler)
	defer httpServer.Close()

	baseDelay := time.Millisecond
	stream := mustSubscribe(t, httpServer.URL,
		StreamOptionInitialRetry(baseDelay),
		StreamOptionUseBackoff(time.Minute))
	defer stream.Close()

	retry := stream.getRetryDelayStrategy()
	assert.False(t, retry.hasJitter())
	d0 := retry.NextRetryDelay(time.Now())
	d1 := retry.NextRetryDelay(time.Now())
	d2 := retry.NextRetryDelay(time.Now())
	assert.Equal(t, baseDelay, d0)
	assert.Equal(t, baseDelay*2, d1)
	assert.Equal(t, baseDelay*4, d2)
}

func TestStreamCanUseJitter(t *testing.T) {
	streamHandler, streamControl := httphelpers.SSEHandler(nil)
	defer streamControl.Close()
	httpServer := httptest.NewServer(streamHandler)
	defer httpServer.Close()

	baseDelay := time.Millisecond
	stream := mustSubscribe(t, httpServer.URL,
		StreamOptionInitialRetry(baseDelay),
		StreamOptionUseBackoff(time.Minute),
		StreamOptionUseJitter(0.5))
	defer stream.Close()

	retry := stream.getRetryDelayStrategy()
	assert.True(t, retry.hasJitter())
	d0 := retry.NextRetryDelay(time.Now())
	d1 := retry.NextRetryDelay(time.Now())
	assert.True(t, d0 >= baseDelay/2)
	assert.True(t, d0 <= baseDelay)
	assert.True(t, d1 >= baseDelay)
	assert.True(t, d1 <= baseDelay*2)
}

func TestStreamCanSetMaximumDelayWithBackoff(t *testing.T) {
	streamHandler, streamControl := httphelpers.SSEHandler(nil)
	defer streamControl.Close()
	httpServer := httptest.NewServer(streamHandler)
	defer httpServer.Close()

	baseDelay := time.Millisecond
	max := baseDelay * 3
	stream := mustSubscribe(t, httpServer.URL,
		StreamOptionInitialRetry(baseDelay),
		StreamOptionUseBackoff(max))
	defer stream.Close()

	retry := stream.getRetryDelayStrategy()
	assert.False(t, retry.hasJitter())
	d0 := retry.NextRetryDelay(time.Now())
	d1 := retry.NextRetryDelay(time.Now())
	d2 := retry.NextRetryDelay(time.Now())
	assert.Equal(t, baseDelay, d0)
	assert.Equal(t, baseDelay*2, d1)
	assert.Equal(t, max, d2)
}

// The error handler advertised at interface.go can switch retry regimes by
// returning ActivateProfile on its result. This test exercises the wiring in
// SubscribeWithRequestAndOptions where the initial connect fails and the
// handler returns an extended profile. After the retry succeeds, the strategy's
// active profile must be the extended profile returned by the handler.
func TestStreamErrorHandlerActivateProfileWiredOnInitialConnect(t *testing.T) {
	streamHandler, streamControl := httphelpers.SSEHandler(nil)
	defer streamControl.Close()
	handler, requestsCh := httphelpers.RecordingHandler(httphelpers.SequentialHandler(
		handlerCausingHTTPError(401, nil),
		streamHandler,
	))
	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()

	ext := NewRetryProfile(RetryProfileBaseDelay(time.Millisecond))

	stream, err := SubscribeWithURL(httpServer.URL,
		StreamOptionInitialRetry(time.Millisecond),
		StreamOptionCanRetryFirstConnection(time.Second*2),
		StreamOptionRegisterRetryProfile(ext),
		StreamOptionErrorHandler(func(err error) StreamErrorHandlerResult {
			return StreamErrorHandlerResult{ActivateProfile: ext}
		}))
	defer func() {
		if stream != nil {
			stream.Close()
		}
	}()
	assert.NoError(t, err)
	assert.Equal(t, 2, len(requestsCh), "expected 401 then successful reconnect")

	// The initial-connect retry loop called activateProfile(ext) via the handler's
	// result. That state should be observable on the strategy after subscribe
	// returns, before any healthy-op reset could fire.
	assert.Same(t, ext, stream.getRetryDelayStrategy().activeProfile(),
		"error handler's ActivateProfile should have switched the active profile")
}

// Same wiring in the worker goroutine path: after a successful subscribe,
// dropping the connection triggers the error handler with an existing-stream
// error. If the handler returns ActivateProfile, the strategy should switch
// before the next retry.
func TestStreamErrorHandlerActivateProfileWiredOnExistingConnection(t *testing.T) {
	streamHandler1, streamControl1 := httphelpers.SSEHandler(nil)
	defer streamControl1.Close()
	streamHandler2, streamControl2 := httphelpers.SSEHandler(nil)
	defer streamControl2.Close()
	handler, requestsCh := httphelpers.RecordingHandler(httphelpers.SequentialHandler(streamHandler1, streamHandler2))
	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()

	ext := NewRetryProfile(RetryProfileBaseDelay(time.Millisecond))

	stream := mustSubscribe(t, httpServer.URL,
		StreamOptionInitialRetry(time.Millisecond),
		StreamOptionRegisterRetryProfile(ext),
		StreamOptionErrorHandler(func(err error) StreamErrorHandlerResult {
			return StreamErrorHandlerResult{ActivateProfile: ext}
		}))
	defer stream.Close()

	// Wait for the initial connection to arrive at the server.
	select {
	case <-requestsCh:
	case <-time.After(timeToWaitForEvent):
		t.Fatal("timed out waiting for initial connection")
	}

	// Drop the initial connection so the worker goroutine's error path fires,
	// invokes the handler, and applies ActivateProfile before scheduling retry.
	streamControl1.Close()

	// Wait for the reconnect. Once it lands, the handler has been invoked and
	// activateProfile(ext) has been called by the worker path.
	select {
	case <-requestsCh:
	case <-time.After(timeToWaitForEvent):
		t.Fatal("timed out waiting for reconnect after connection drop")
	}

	assert.Same(t, ext, stream.getRetryDelayStrategy().activeProfile(),
		"error handler's ActivateProfile should have switched the active profile on worker-path error")
}

// The public Stream.ActivateProfile method exposes the same activation primitive
// to callers who want to switch regimes out-of-band (not via an error handler).
// This test pins that the public method reaches the internal strategy.
func TestStreamActivateProfilePublicMethodReachesStrategy(t *testing.T) {
	streamHandler, streamControl := httphelpers.SSEHandler(nil)
	defer streamControl.Close()
	httpServer := httptest.NewServer(streamHandler)
	defer httpServer.Close()

	ext := NewRetryProfile(RetryProfileBaseDelay(time.Second * 5))

	stream := mustSubscribe(t, httpServer.URL,
		StreamOptionInitialRetry(time.Millisecond),
		StreamOptionRegisterRetryProfile(ext))
	defer stream.Close()

	strat := stream.getRetryDelayStrategy()

	// Sanity: pre-activation the active profile is whatever the effective default
	// resolved to (a synthesized profile, not ext).
	assert.NotSame(t, ext, strat.activeProfile(), "ext should not be active at start")

	stream.ActivateProfile(ext)
	assert.Same(t, ext, strat.activeProfile(), "public ActivateProfile should switch the active profile")

	// Reverting via the DefaultProfile sentinel should undo the switch.
	stream.ActivateProfile(DefaultProfile)
	assert.NotSame(t, ext, strat.activeProfile(), "DefaultProfile sentinel should revert to effective default")
}

func TestStreamBackoffCanUseResetInterval(t *testing.T) {
	// streamHandler1 sends an event, then breaks the connection before the reset interval elapses.
	// streamHandler2 sends an event after the reconnect. Then we verify that the next delay would be
	// the initial delay if the reset interval elapsed after the event on the second connection.
	streamHandler1, streamControl1 := httphelpers.SSEHandler(nil)
	defer streamControl1.Close()
	streamHandler2, streamControl2 := httphelpers.SSEHandler(nil)
	defer streamControl2.Close()
	httpServer := httptest.NewServer(httphelpers.SequentialHandler(streamHandler1, streamHandler2))
	defer httpServer.Close()

	baseDelay := time.Millisecond
	resetInterval := time.Millisecond * 200
	stream := mustSubscribe(t, httpServer.URL,
		StreamOptionInitialRetry(baseDelay),
		StreamOptionUseBackoff(time.Hour),
		StreamOptionRetryResetInterval(resetInterval),
		// The test does not read stream.Errors. Without this handler, the stream blocks when it reports
		// the end of the first connection, and it does not reconnect.
		StreamOptionErrorHandler(func(error) StreamErrorHandlerResult { return StreamErrorHandlerResult{} }))
	defer stream.Close()

	retry := stream.getRetryDelayStrategy()

	receiveEvent := func(expectedID string) {
		t.Helper()
		select {
		case ev := <-stream.Events:
			assert.Equal(t, expectedID, ev.Id())
		case <-time.After(timeToWaitForEvent):
			require.FailNow(t, "timed out waiting for event", "expected event ID %q", expectedID)
		}
	}

	// The first connection sends an event. Read it, so the stream state is "good".
	streamControl1.Enqueue(httphelpers.SSEEvent{ID: "1"})
	receiveEvent("1")

	// Compute the next two delays. The reset interval has not elapsed, so the delay increases.
	d0 := retry.NextRetryDelay(time.Now())
	d1 := retry.NextRetryDelay(time.Now())
	assert.Equal(t, baseDelay, d0)
	assert.Equal(t, baseDelay*2, d1)

	// Break the first connection. The stream reconnects and the delay increases again.
	streamControl1.EndAll()

	// The second connection sends an event. The event ID shows that the event came from the second
	// connection. The stream state is "good" again.
	streamControl2.Enqueue(httphelpers.SSEEvent{ID: "2"})
	receiveEvent("2")

	// Compute the delay as if the next attempt occurs one reset interval from now. The state stayed
	// "good" for the full interval, so the delay goes back to baseDelay.
	d2 := retry.NextRetryDelay(time.Now().Add(resetInterval))
	assert.Equal(t, baseDelay, d2)
}
