package eventsource

import (
	"net/http"
	"net/http/httptest"
	"strings"
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
