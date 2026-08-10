package eventsource

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

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
// returning ActivateCurve on its result. This test exercises the wiring in
// SubscribeWithRequestAndOptions where the initial connect fails and the
// handler returns an extended curve. After the retry succeeds, the strategy's
// active curve must be the extended curve returned by the handler.
func TestStreamErrorHandlerActivateCurveWiredOnInitialConnect(t *testing.T) {
	streamHandler, streamControl := httphelpers.SSEHandler(nil)
	defer streamControl.Close()
	handler, requestsCh := httphelpers.RecordingHandler(httphelpers.SequentialHandler(
		handlerCausingHTTPError(401, nil),
		streamHandler,
	))
	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()

	ext := NewRetryCurve(RetryCurveBaseDelay(time.Millisecond))

	stream, err := SubscribeWithURL(httpServer.URL,
		StreamOptionInitialRetry(time.Millisecond),
		StreamOptionCanRetryFirstConnection(time.Second*2),
		StreamOptionRegisterRetryCurve(ext),
		StreamOptionErrorHandler(func(err error) StreamErrorHandlerResult {
			return StreamErrorHandlerResult{ActivateCurve: ext}
		}))
	defer func() {
		if stream != nil {
			stream.Close()
		}
	}()
	assert.NoError(t, err)
	assert.Equal(t, 2, len(requestsCh), "expected 401 then successful reconnect")

	// The initial-connect retry loop called activateCurve(ext) via the handler's
	// result. That state should be observable on the strategy after subscribe
	// returns, before any healthy-op reset could fire.
	assert.Same(t, ext, stream.getRetryDelayStrategy().activeCurve(),
		"error handler's ActivateCurve should have switched the active curve")
}

// Same wiring in the worker goroutine path: after a successful subscribe,
// dropping the connection triggers the error handler with an existing-stream
// error. If the handler returns ActivateCurve, the strategy should switch
// before the next retry.
func TestStreamErrorHandlerActivateCurveWiredOnExistingConnection(t *testing.T) {
	streamHandler1, streamControl1 := httphelpers.SSEHandler(nil)
	defer streamControl1.Close()
	streamHandler2, streamControl2 := httphelpers.SSEHandler(nil)
	defer streamControl2.Close()
	handler, requestsCh := httphelpers.RecordingHandler(httphelpers.SequentialHandler(streamHandler1, streamHandler2))
	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()

	ext := NewRetryCurve(RetryCurveBaseDelay(time.Millisecond))

	stream := mustSubscribe(t, httpServer.URL,
		StreamOptionInitialRetry(time.Millisecond),
		StreamOptionRegisterRetryCurve(ext),
		StreamOptionErrorHandler(func(err error) StreamErrorHandlerResult {
			return StreamErrorHandlerResult{ActivateCurve: ext}
		}))
	defer stream.Close()

	// Wait for the initial connection to arrive at the server.
	select {
	case <-requestsCh:
	case <-time.After(timeToWaitForEvent):
		t.Fatal("timed out waiting for initial connection")
	}

	// Drop the initial connection so the worker goroutine's error path fires,
	// invokes the handler, and applies ActivateCurve before scheduling retry.
	streamControl1.Close()

	// Wait for the reconnect. Once it lands, the handler has been invoked and
	// activateCurve(ext) has been called by the worker path.
	select {
	case <-requestsCh:
	case <-time.After(timeToWaitForEvent):
		t.Fatal("timed out waiting for reconnect after connection drop")
	}

	assert.Same(t, ext, stream.getRetryDelayStrategy().activeCurve(),
		"error handler's ActivateCurve should have switched the active curve on worker-path error")
}

// The public Stream.ActivateCurve method exposes the same activation primitive
// to callers who want to switch regimes out-of-band (not via an error handler).
// This test pins that the public method reaches the internal strategy.
func TestStreamActivateCurvePublicMethodReachesStrategy(t *testing.T) {
	streamHandler, streamControl := httphelpers.SSEHandler(nil)
	defer streamControl.Close()
	httpServer := httptest.NewServer(streamHandler)
	defer httpServer.Close()

	ext := NewRetryCurve(RetryCurveBaseDelay(time.Second * 5))

	stream := mustSubscribe(t, httpServer.URL,
		StreamOptionInitialRetry(time.Millisecond),
		StreamOptionRegisterRetryCurve(ext))
	defer stream.Close()

	strat := stream.getRetryDelayStrategy()

	// Sanity: pre-activation the active curve is whatever the effective default
	// resolved to (a synthesized curve, not ext).
	assert.NotSame(t, ext, strat.activeCurve(), "ext should not be active at start")

	stream.ActivateCurve(ext)
	assert.Same(t, ext, strat.activeCurve(), "public ActivateCurve should switch the active curve")

	// Reverting via the DefaultCurve sentinel should undo the switch.
	stream.ActivateCurve(DefaultCurve)
	assert.NotSame(t, ext, strat.activeCurve(), "DefaultCurve sentinel should revert to effective default")
}

func TestStreamBackoffCanUseResetInterval(t *testing.T) {
	// In this test, streamHandler1 sends an event, then breaks the connection too soon for the delay to be
	// reset. We ask the retryDelayStrategy to compute the next delay; it should be higher than the initial
	// value. Then streamHandler2 sends an event, and we verify that the next delay that *would* come from the
	// retryDelayStrategy if the reset interval elapsed would be the initial delay, not a higher value.
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
		StreamOptionRetryResetInterval(resetInterval))
	defer stream.Close()

	retry := stream.getRetryDelayStrategy()

	// The first stream connection sends an event, so the stream state becomes "good".
	event := httphelpers.SSEEvent{ID: "123"}
	streamControl1.Enqueue(event)

	// We ask the retryDelayStrategy to compute the next two delay values; they should show an increase.
	d0 := retry.NextRetryDelay(time.Now())
	d1 := retry.NextRetryDelay(time.Now())
	assert.Equal(t, baseDelay, d0)
	assert.Equal(t, baseDelay*2, d1)

	// The first connection is broken; the state becomes "bad".
	streamControl1.EndAll()

	// After it reconnects, the second connection receives an event and the state becomes "good" again.
	streamControl2.Enqueue(event)
	<-stream.Events

	// Now, ask the retryDelayStrategy what the next delay value would be if the next attempt happened
	// 200 milliseconds from now (assuming the stream remained good). It should go back to baseDelay.
	d2 := retry.NextRetryDelay(time.Now().Add(resetInterval))
	assert.Equal(t, baseDelay, d2)
}
