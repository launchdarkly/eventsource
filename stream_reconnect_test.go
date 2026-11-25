package eventsource

import (
	"fmt"
	"net/http"
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

func TestStreamResetIntervalIsMeasuredFromFirstEvent(t *testing.T) {
	// The stream receives events more often than the reset interval for longer than the reset
	// interval. A failure right after the last event must still reset the backoff, because the
	// healthy period started with the first event.
	streamHandler, streamControl := httphelpers.SSEHandler(nil)
	defer streamControl.Close()
	httpServer := httptest.NewServer(streamHandler)
	defer httpServer.Close()

	baseDelay := time.Millisecond
	resetInterval := time.Millisecond * 200
	stream := mustSubscribe(t, httpServer.URL,
		StreamOptionInitialRetry(baseDelay),
		StreamOptionUseBackoff(time.Hour),
		StreamOptionRetryResetInterval(resetInterval))
	defer stream.Close()

	retry := stream.getRetryDelayStrategy()

	// Advance the backoff counter before the stream becomes good.
	_ = retry.NextRetryDelay(time.Now())
	_ = retry.NextRetryDelay(time.Now())

	event := httphelpers.SSEEvent{ID: "123"}
	sendAndReceive := func() {
		streamControl.Enqueue(event)
		select {
		case <-stream.Events:
		case <-time.After(time.Second * 2):
			require.FailNow(t, "timed out waiting for event")
		}
	}
	sendAndReceive()
	firstEvent := time.Now()
	for time.Since(firstEvent) < resetInterval+resetInterval/2 {
		time.Sleep(resetInterval / 4)
		sendAndReceive()
	}

	d := retry.NextRetryDelay(time.Now())
	assert.Equal(t, baseDelay, d)
}

// delayLogger sends each reconnect delay that the stream logs to a channel.
type delayLogger struct {
	delays chan time.Duration
}

func (l delayLogger) Println(...interface{}) {}

func (l delayLogger) Printf(format string, args ...interface{}) {
	if format == "Reconnecting in %0.4f secs" && len(args) == 1 {
		if secs, ok := args[0].(float64); ok {
			l.delays <- time.Duration(secs * float64(time.Second)).Round(time.Millisecond)
		}
	}
}

func TestStreamHealthyPeriodRestartsOnEachConnection(t *testing.T) {
	// Each connection starts its own healthy period at its first event.
	// Connection 1 fails with no event, so the backoff counter climbs.
	// Connection 2 stays healthy longer than the reset interval, so its drop resets the backoff.
	// Connection 3 drops soon after its first event. Connection 2 started long ago, but that
	// must not count, so the backoff climbs again.
	immediateEOF := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	})
	streamHandler2, streamControl2 := httphelpers.SSEHandler(nil)
	defer streamControl2.Close()
	streamHandler3, streamControl3 := httphelpers.SSEHandler(nil)
	defer streamControl3.Close()
	streamHandler4, streamControl4 := httphelpers.SSEHandler(nil)
	defer streamControl4.Close()
	handler, requestsCh := httphelpers.RecordingHandler(httphelpers.SequentialHandler(
		immediateEOF, streamHandler2, streamHandler3, streamHandler4))
	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()

	// The reset interval is long compared to the delays and to scheduling noise under -race.
	baseDelay := 10 * time.Millisecond
	resetInterval := 300 * time.Millisecond
	waitTime := 2 * time.Second
	logger := delayLogger{delays: make(chan time.Duration, 10)}
	stream, err := SubscribeWithURL(httpServer.URL,
		StreamOptionInitialRetry(baseDelay),
		StreamOptionUseBackoff(time.Hour),
		StreamOptionRetryResetInterval(resetInterval),
		StreamOptionLogger(logger),
		StreamOptionErrorHandler(func(error) StreamErrorHandlerResult { return StreamErrorHandlerResult{} }))
	if !assert.NoError(t, err) {
		return
	}
	defer stream.Close()

	awaitRequest := func(name string) {
		t.Helper()
		select {
		case <-requestsCh:
		case <-time.After(waitTime):
			t.Fatalf("timed out waiting for %s", name)
		}
	}
	awaitDelay := func(name string) time.Duration {
		t.Helper()
		select {
		case d := <-logger.delays:
			return d
		case <-time.After(waitTime):
			t.Fatalf("timed out waiting for reconnect delay after %s", name)
			return 0
		}
	}
	sendAndReceive := func(control httphelpers.SSEStreamControl, id string) {
		t.Helper()
		control.Enqueue(httphelpers.SSEEvent{ID: id})
		select {
		case ev := <-stream.Events:
			assert.Equal(t, id, ev.Id())
		case <-time.After(waitTime):
			t.Fatalf("timed out waiting for event %s", id)
		}
	}

	// Connection 1 ends with no event. This is a failure with no healthy period.
	awaitRequest("connection 1")
	assert.Equal(t, baseDelay, awaitDelay("connection 1"))

	// Connection 2 sends events more often than the reset interval, for longer than the reset
	// interval. The drop comes right after the last event. The backoff resets because the
	// healthy period started at the first event.
	awaitRequest("connection 2")
	sendAndReceive(streamControl2, "2-0")
	firstEvent := time.Now()
	for i := 1; time.Since(firstEvent) < resetInterval+resetInterval/2; i++ {
		time.Sleep(resetInterval / 4)
		sendAndReceive(streamControl2, fmt.Sprintf("2-%d", i))
	}
	streamControl2.EndAll()
	assert.Equal(t, baseDelay, awaitDelay("connection 2"), "healthy connection must reset the backoff")

	// Connection 3 sends one event and drops at once. Its healthy period is too short, so the
	// backoff does not reset and the delay doubles.
	awaitRequest("connection 3")
	sendAndReceive(streamControl3, "3-0")
	streamControl3.EndAll()
	assert.Equal(t, baseDelay*2, awaitDelay("connection 3"), "short connection must not reset the backoff")

	awaitRequest("connection 4")
}
