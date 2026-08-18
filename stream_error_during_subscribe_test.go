package eventsource

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/launchdarkly/go-test-helpers/v3/httphelpers"
)

func handlerCausingNetworkError() http.Handler {
	return httphelpers.BrokenConnectionHandler()
}

func handlerCausingHTTPError(status int, header *http.Header) http.Handler {
	if header == nil {
		return httphelpers.HandlerWithStatus(status)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for key, values := range *header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		httphelpers.HandlerWithStatus(status).ServeHTTP(w, r)
	})
}

func shouldBeNetworkError(t *testing.T) func(error) {
	return func(err error) {
		if !strings.HasSuffix(err.Error(), "EOF") {
			t.Errorf("expected EOF error, got %v", err)
		}
	}
}

func shouldBeHTTPError(t *testing.T, status int, header *http.Header) func(error) {
	return func(err error) {
		switch e := err.(type) {
		case SubscriptionError:
			assert.Equal(t, status, e.Code)
			if header != nil {
				for key, value := range *header {
					if v, ok := e.Header[key]; ok {
						assert.Equal(t, v, value)
					} else {
						assert.Fail(t, "header not found", "header %s not found in error headers", key)
					}
				}
			}
		default:
			t.Errorf("expected SubscriptionError, got %T", e)
			return
		}
	}
}

func TestStreamDoesNotRetryInitialConnectionByDefaultAfterNetworkError(t *testing.T) {
	testStreamDoesNotRetryInitialConnectionByDefault(t, handlerCausingNetworkError(), shouldBeNetworkError(t))
}

func TestStreamDoesNotRetryInitialConnectionByDefaultAfterHTTPError(t *testing.T) {
	header := http.Header{
		"X-My-Header": []string{"my-value"},
	}
	testStreamDoesNotRetryInitialConnectionByDefault(t, handlerCausingHTTPError(401, &header), shouldBeHTTPError(t, 401, &header))
}

func testStreamDoesNotRetryInitialConnectionByDefault(t *testing.T, errorHandler http.Handler, checkError func(error)) {
	streamHandler, streamControl := httphelpers.SSEHandler(nil)
	defer streamControl.Close()
	handler, requestsCh := httphelpers.RecordingHandler(httphelpers.SequentialHandler(errorHandler, streamHandler))
	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()

	stream, err := SubscribeWithURL(httpServer.URL)
	defer func() {
		if stream != nil {
			stream.Close()
		}
	}()
	assert.Error(t, err)
	assert.Nil(t, stream)

	assert.Equal(t, 1, len(requestsCh))
}

func TestStreamCanRetryInitialConnection(t *testing.T) {
	streamHandler, streamControl := httphelpers.SSEHandler(nil)
	defer streamControl.Close()
	handler, requestsCh := httphelpers.RecordingHandler(httphelpers.SequentialHandler(
		handlerCausingNetworkError(),
		handlerCausingNetworkError(),
		streamHandler))
	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()

	stream, err := SubscribeWithURL(httpServer.URL,
		StreamOptionInitialRetry(time.Millisecond),
		StreamOptionCanRetryFirstConnection(time.Second*2))
	defer func() {
		if stream != nil {
			stream.Close()
		}
	}()
	assert.NoError(t, err)

	assert.Equal(t, 3, len(requestsCh))
}

func TestStreamCanRetryInitialConnectionWithIndefiniteTimeout(t *testing.T) {
	streamHandler, streamControl := httphelpers.SSEHandler(nil)
	defer streamControl.Close()
	handler, requestsCh := httphelpers.RecordingHandler(httphelpers.SequentialHandler(
		handlerCausingNetworkError(),
		handlerCausingNetworkError(),
		streamHandler))
	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()

	stream, err := SubscribeWithURL(httpServer.URL,
		StreamOptionInitialRetry(time.Millisecond),
		StreamOptionCanRetryFirstConnection(-1))
	defer func() {
		if stream != nil {
			stream.Close()
		}
	}()
	assert.NoError(t, err)

	assert.Equal(t, 3, len(requestsCh))
}

func TestStreamCanRetryInitialConnectionUntilFiniteTimeout(t *testing.T) {
	streamHandler, streamControl := httphelpers.SSEHandler(nil)
	defer streamControl.Close()
	handler, requestsCh := httphelpers.RecordingHandler(httphelpers.SequentialHandler(
		handlerCausingNetworkError(),
		handlerCausingNetworkError(),
		streamHandler))
	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()

	stream, err := SubscribeWithURL(httpServer.URL,
		StreamOptionInitialRetry(100*time.Millisecond),
		StreamOptionCanRetryFirstConnection(150*time.Millisecond))
	defer func() {
		if stream != nil {
			stream.Close()
		}
	}()
	assert.Error(t, err)

	assert.Equal(t, 2, len(requestsCh))
}

func TestStreamErrorHandlerCanAllowRetryOfInitialConnectionAfterNetworkError(t *testing.T) {
	testStreamErrorHandlerCanAllowRetryOfInitialConnection(t, handlerCausingNetworkError(), shouldBeNetworkError(t))
}

func TestStreamErrorHandlerCanAllowRetryOfInitialConnectionAfterHTTPError(t *testing.T) {
	testStreamErrorHandlerCanAllowRetryOfInitialConnection(t, handlerCausingHTTPError(401, nil), shouldBeHTTPError(t, 401, nil))
}

func testStreamErrorHandlerCanAllowRetryOfInitialConnection(t *testing.T, errorHandler http.Handler, checkError func(error)) {
	streamHandler, streamControl := httphelpers.SSEHandler(nil)
	defer streamControl.Close()
	handler, requestsCh := httphelpers.RecordingHandler(httphelpers.SequentialHandler(
		errorHandler,
		streamHandler))
	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()

	myErrChannel := make(chan error, 1)

	stream, err := SubscribeWithURL(httpServer.URL,
		StreamOptionInitialRetry(100*time.Millisecond),
		StreamOptionCanRetryFirstConnection(150*time.Millisecond),
		StreamOptionErrorHandler(func(err error) StreamErrorHandlerResult {
			myErrChannel <- err
			return StreamErrorHandlerResult{}
		}))
	defer func() {
		if stream != nil {
			stream.Close()
		}
	}()
	assert.NoError(t, err)

	assert.Equal(t, 1, len(myErrChannel))
	e := <-myErrChannel
	checkError(e)

	assert.Equal(t, 2, len(requestsCh))
}

func TestStreamErrorHandlerCanPreventRetryOfInitialConnection(t *testing.T) {
	streamHandler, streamControl := httphelpers.SSEHandler(nil)
	defer streamControl.Close()
	handler, requestsCh := httphelpers.RecordingHandler(httphelpers.SequentialHandler(
		handlerCausingNetworkError(),
		streamHandler))
	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()

	stream, err := SubscribeWithURL(httpServer.URL,
		StreamOptionInitialRetry(100*time.Millisecond),
		StreamOptionCanRetryFirstConnection(150*time.Millisecond),
		StreamOptionErrorHandler(func(err error) StreamErrorHandlerResult {
			return StreamErrorHandlerResult{CloseNow: true}
		}))
	defer func() {
		if stream != nil {
			stream.Close()
		}
	}()
	assert.Error(t, err)

	assert.Equal(t, 1, len(requestsCh))
}

// Cancelling the request's context during a retry sleep aborts the retry loop
// promptly with the context's error, rather than waiting for the sleep to elapse.
// Uses a large retry delay so a passing test cannot be masked by the sleep
// happening to finish before the cancel takes effect.
func TestStreamSubscribeIsInterruptedByRequestContextDuringRetrySleep(t *testing.T) {
	httpServer := httptest.NewServer(handlerCausingHTTPError(401, nil))
	defer httpServer.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", httpServer.URL, nil)
	assert.NoError(t, err)

	// Cancel the context shortly after Subscribe enters its retry sleep. The
	// retry delay is 10s so the test would time out if the sleep were still
	// uninterruptible.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	stream, err := SubscribeWithRequestAndOptions(req,
		StreamOptionHTTPClient(newIsolatedClient(t)),
		StreamOptionInitialRetry(10*time.Second),
		StreamOptionCanRetryFirstConnection(-1))
	elapsed := time.Since(start)
	wg.Wait()

	assert.Nil(t, stream)
	assert.True(t, errors.Is(err, context.Canceled), "expected context.Canceled, got %v", err)
	assert.True(t, elapsed < 2*time.Second, "Subscribe should have returned promptly after ctx cancel, took %v", elapsed)
}

// Cancelling the request's context before Subscribe is called causes Subscribe
// to abort at the first opportunity - its first connect() call is short-circuited
// by http.Client honoring the already-cancelled context.
func TestStreamSubscribeReturnsImmediatelyWhenRequestContextAlreadyCancelled(t *testing.T) {
	httpServer := httptest.NewServer(handlerCausingHTTPError(401, nil))
	defer httpServer.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled

	req, err := http.NewRequestWithContext(ctx, "GET", httpServer.URL, nil)
	assert.NoError(t, err)

	handlerCalls := 0
	stream, err := SubscribeWithRequestAndOptions(req,
		StreamOptionHTTPClient(newIsolatedClient(t)),
		StreamOptionInitialRetry(10*time.Second),
		StreamOptionCanRetryFirstConnection(-1),
		StreamOptionErrorHandler(func(err error) StreamErrorHandlerResult {
			handlerCalls++
			return StreamErrorHandlerResult{}
		}))

	assert.Nil(t, stream)
	assert.True(t, errors.Is(err, context.Canceled), "expected context.Canceled, got %v", err)
	// The error handler must not be invoked when the failure was caused by the
	// caller cancelling the context - the caller has already decided to abandon.
	assert.Equal(t, 0, handlerCalls, "error handler should not run on context cancellation")
}

// A request whose context has a deadline that expires during a retry sleep
// causes Subscribe to return context.DeadlineExceeded, mirroring the
// context.Canceled path. Uses a 1h retry delay so a passing test cannot be
// explained by the sleep happening to complete before the deadline fires.
func TestStreamSubscribeReturnsContextDeadlineExceededWhenDeadlinePasses(t *testing.T) {
	httpServer := httptest.NewServer(handlerCausingHTTPError(401, nil))
	defer httpServer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", httpServer.URL, nil)
	assert.NoError(t, err)

	start := time.Now()
	stream, err := SubscribeWithRequestAndOptions(req,
		StreamOptionHTTPClient(newIsolatedClient(t)),
		StreamOptionInitialRetry(time.Hour),
		StreamOptionCanRetryFirstConnection(-1))
	elapsed := time.Since(start)

	assert.Nil(t, stream)
	assert.True(t, errors.Is(err, context.DeadlineExceeded),
		"expected context.DeadlineExceeded, got %v", err)
	assert.True(t, elapsed < 2*time.Second,
		"Subscribe should return promptly after deadline expires, took %v", elapsed)
}

// A server that accepts the TCP connection but never sends a response blocks
// stream.c.Do indefinitely. Cancelling the request context while Do is stuck
// aborts the HTTP call and returns from Subscribe promptly. This is the
// worst-case pre-connect scenario the RETRY conformance work made visible:
// without ctx observation, Subscribe would never return.
func TestStreamSubscribeIsInterruptedByRequestContextDuringInFlightDo(t *testing.T) {
	blockCh := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-blockCh
	})
	httpServer := httptest.NewServer(handler)
	// Defer order matters: close(blockCh) must run before httpServer.Close()
	// so the blocked handler goroutine can exit and let the server shut down.
	defer httpServer.Close()
	defer close(blockCh)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", httpServer.URL, nil)
	assert.NoError(t, err)

	// Cancel after Subscribe has entered Do and is blocked waiting for the server.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	stream, err := SubscribeWithRequestAndOptions(req,
		StreamOptionHTTPClient(newIsolatedClient(t)),
		StreamOptionCanRetryFirstConnection(-1))
	elapsed := time.Since(start)
	wg.Wait()

	assert.Nil(t, stream)
	assert.True(t, errors.Is(err, context.Canceled),
		"expected context.Canceled, got %v", err)
	assert.True(t, elapsed < 1*time.Second,
		"Do should abort promptly after ctx cancel, took %v", elapsed)
}

// Backward compatibility: requests built via http.NewRequest carry
// context.Background, whose Done channel never fires. The retry loop must
// behave identically to pre-context behavior - no premature return.
func TestStreamSubscribeIsUnaffectedByBackgroundContextRequest(t *testing.T) {
	streamHandler, streamControl := httphelpers.SSEHandler(nil)
	defer streamControl.Close()
	handler, requestsCh := httphelpers.RecordingHandler(httphelpers.SequentialHandler(
		handlerCausingNetworkError(),
		handlerCausingNetworkError(),
		streamHandler))
	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()

	req, err := http.NewRequest("GET", httpServer.URL, nil) // no context
	assert.NoError(t, err)

	stream, err := SubscribeWithRequestAndOptions(req,
		StreamOptionHTTPClient(newIsolatedClient(t)),
		StreamOptionInitialRetry(time.Millisecond),
		StreamOptionCanRetryFirstConnection(-1))
	defer func() {
		if stream != nil {
			stream.Close()
			// Drain until Events closes so the stream goroutine has fully
			// exited before returning - leaving it running loads subsequent
			// tests' scheduling.
			for range stream.Events { //nolint:revive
			}
		}
	}()
	assert.NoError(t, err)
	assert.Equal(t, 3, len(requestsCh))
}
