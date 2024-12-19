package eventsource

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/launchdarkly/go-test-helpers/v2/httphelpers"
)

func TestStreamCanUseCustomClient(t *testing.T) {
	streamHandler, streamControl := httphelpers.SSEHandler(nil)
	defer streamControl.Close()
	handler, requestsCh := httphelpers.RecordingHandler(streamHandler)
	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()

	client := *http.DefaultClient
	client.Transport = urlSuffixingRoundTripper{http.DefaultTransport, "path"}

	stream := mustSubscribe(t, httpServer.URL, StreamOptionHTTPClient(&client))
	defer stream.Close()

	r := <-requestsCh
	assert.Equal(t, "/path", r.Request.URL.Path)
}

func TestStreamSendsLastEventID(t *testing.T) {
	streamHandler, streamControl := httphelpers.SSEHandler(nil)
	defer streamControl.Close()
	handler, requestsCh := httphelpers.RecordingHandler(streamHandler)

	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()

	lastID := "xyz"
	stream := mustSubscribe(t, httpServer.URL, StreamOptionLastEventID(lastID))
	defer stream.Close()

	r0 := <-requestsCh
	assert.Equal(t, lastID, r0.Request.Header.Get("Last-Event-ID"))
}

func TestCanReplaceStreamQueryParameters(t *testing.T) {
	streamHandler, streamControl := httphelpers.SSEHandler(nil)
	defer streamControl.Close()
	handler, requestsCh := httphelpers.RecordingHandler(streamHandler)

	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()

	option := StreamOptionDynamicQueryParams(func(existing url.Values) url.Values {
		return url.Values{
			"filter": []string{"my-custom-filter"},
			"basis":  []string{"last-known-basis"},
		}
	})

	stream := mustSubscribe(t, httpServer.URL, option)
	defer stream.Close()

	r0 := <-requestsCh
	assert.Equal(t, "my-custom-filter", r0.Request.URL.Query().Get("filter"))
	assert.Equal(t, "last-known-basis", r0.Request.URL.Query().Get("basis"))
}

func TestCanUpdateStreamQueryParameters(t *testing.T) {
	streamHandler, streamControl := httphelpers.SSEHandler(nil)
	defer streamControl.Close()
	handler, requestsCh := httphelpers.RecordingHandler(streamHandler)

	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()

	option := StreamOptionDynamicQueryParams(func(existing url.Values) url.Values {
		if existing.Has("count") {
			count, _ := strconv.Atoi(existing.Get("count"))

			if count == 1 {
				existing.Set("count", strconv.Itoa(count+1))
				return existing
			}

			return url.Values{}
		}

		return url.Values{
			"initial": []string{"payload is set"},
			"count":   []string{"1"},
		}
	})

	stream := mustSubscribe(t, httpServer.URL, option, StreamOptionInitialRetry(time.Millisecond))
	defer stream.Close()

	r0 := <-requestsCh
	assert.Equal(t, "payload is set", r0.Request.URL.Query().Get("initial"))
	assert.Equal(t, "1", r0.Request.URL.Query().Get("count"))

	streamControl.EndAll()
	<-stream.Errors // Accept the error to unblock the retry handler

	r1 := <-requestsCh
	assert.Equal(t, "payload is set", r1.Request.URL.Query().Get("initial"))
	assert.Equal(t, "2", r1.Request.URL.Query().Get("count"))

	streamControl.EndAll()
	<-stream.Errors // Accept the error to unblock the retry handler

	r2 := <-requestsCh
	assert.False(t, r2.Request.URL.Query().Has("initial"))
	assert.False(t, r2.Request.URL.Query().Has("count"))
}

func TestStreamReconnectWithRequestBodySendsBodyTwice(t *testing.T) {
	body := []byte("my-body")

	streamHandler, streamControl := httphelpers.SSEHandler(nil)
	defer streamControl.Close()
	handler, requestsCh := httphelpers.RecordingHandler(streamHandler)

	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()

	req, _ := http.NewRequest("REPORT", httpServer.URL, bytes.NewBuffer(body))
	if req.GetBody == nil {
		t.Fatalf("Expected get body to be set")
	}
	stream, err := SubscribeWithRequestAndOptions(req, StreamOptionInitialRetry(time.Millisecond))
	if err != nil {
		t.Fatalf("Failed to subscribe: %s", err)
		return
	}
	defer stream.Close()

	// Wait for the first request
	r0 := <-requestsCh

	// Allow the stream to reconnect once; get the second request
	streamControl.EndAll()
	<-stream.Errors // Accept the error to unblock the retry handler
	r1 := <-requestsCh

	stream.Close()

	assert.Equal(t, body, r0.Body)
	assert.Equal(t, body, r1.Body)
}
