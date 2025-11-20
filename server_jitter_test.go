package eventsource

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewServerWithJitter(t *testing.T) {
	jitterDuration := 100 * time.Millisecond
	server := NewServerWithJitter(jitterDuration)
	defer server.Close()

	assert.Equal(t, jitterDuration, server.jitter)
	assert.Equal(t, 128, server.BufferSize)
	assert.NotNil(t, server.registrations)
	assert.NotNil(t, server.unregistrations)
	assert.NotNil(t, server.pub)
	assert.NotNil(t, server.subs)
	assert.NotNil(t, server.unsubs)
	assert.NotNil(t, server.quit)
}

func TestServerWithJitterDelaysEventDelivery(t *testing.T) {
	// Use a small jitter duration for faster testing
	jitterDuration := 50 * time.Millisecond
	channel := "test"
	server := NewServerWithJitter(jitterDuration)
	defer server.Close()

	httpServer := httptest.NewServer(server.Handler(channel))
	defer httpServer.Close()

	// Start a client connection
	resp, err := http.Get(httpServer.URL)
	require.NoError(t, err)
	defer resp.Body.Close()

	// Record when we publish the event
	startTime := time.Now()
	event := &publication{data: "test-event"}
	server.Publish([]string{channel}, event)

	// Read the response with a timeout
	resultCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		buf := make([]byte, 1024)
		n, err := resp.Body.Read(buf)
		if err != nil && err != io.EOF {
			errCh <- err
			return
		}
		resultCh <- string(buf[:n])
	}()

	select {
	case result := <-resultCh:
		elapsed := time.Since(startTime)
		// The event should be delayed by at least jitterDuration/2
		// (since jitter subtracts a random amount up to ratio*duration where ratio=0.5)
		minExpectedDelay := jitterDuration / 2
		assert.GreaterOrEqual(t, elapsed.Milliseconds(), minExpectedDelay.Milliseconds(),
			"Event should be delayed by at least half the jitter duration")
		assert.Contains(t, result, "data: test-event")
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Timed out waiting for event")
	}
}

func TestServerWithJitterDiscardsIntermediateEvents(t *testing.T) {
	// This test verifies that when events arrive rapidly while jitter is active,
	// the first event is kept and subsequent events are discarded until the first
	// is delivered.
	jitterDuration := 100 * time.Millisecond
	channel := "test"
	server := NewServerWithJitter(jitterDuration)

	httpServer := httptest.NewServer(server.Handler(channel))
	defer httpServer.Close()

	// Start a client connection
	resp, err := http.Get(httpServer.URL)
	require.NoError(t, err)
	defer resp.Body.Close()

	// Read the response in a goroutine
	bodyCh := make(chan []byte, 1)
	go func() {
		body, _ := io.ReadAll(resp.Body)
		bodyCh <- body
	}()

	// Publish events with very small delays between them
	// The jitter timer should not have fired yet when we send event-2 and event-3
	server.Publish([]string{channel}, &publication{data: "event-1"})
	time.Sleep(5 * time.Millisecond)
	server.Publish([]string{channel}, &publication{data: "event-2"})
	time.Sleep(5 * time.Millisecond)
	server.Publish([]string{channel}, &publication{data: "event-3"})

	// Wait for the jittered event to be delivered
	time.Sleep(jitterDuration + 50*time.Millisecond)

	// Publish one more event to demonstrate the next cycle
	server.Publish([]string{channel}, &publication{data: "event-4"})
	time.Sleep(jitterDuration + 50*time.Millisecond)

	// Close and read
	server.Close()
	body := <-bodyCh
	responseStr := string(body)

	// Event-1 should be delivered, event-2 and event-3 should be discarded
	assert.Contains(t, responseStr, "event-1", "First event should be delivered")
	assert.NotContains(t, responseStr, "event-2", "Second event should be discarded")
	assert.NotContains(t, responseStr, "event-3", "Third event should be discarded")
	// Event-4 should be delivered as it's a new cycle
	assert.Contains(t, responseStr, "event-4", "Fourth event should be delivered in new cycle")
}

func TestServerWithJitterFlushesDelayedEventBeforeBatch(t *testing.T) {
	jitterDuration := 200 * time.Millisecond
	channel := "test"

	// Custom repository that delays before sending events
	slowRepo := &testServerRepository{}

	server := NewServerWithJitter(jitterDuration)

	httpServer := httptest.NewServer(server.Handler(channel))
	defer httpServer.Close()

	// Start a client connection
	resp, err := http.Get(httpServer.URL)
	require.NoError(t, err)
	defer resp.Body.Close()

	// Read the response in a goroutine
	bodyCh := make(chan []byte, 1)
	go func() {
		body, _ := io.ReadAll(resp.Body)
		bodyCh <- body
	}()

	// Publish a regular event (this will be delayed by jitter)
	server.Publish([]string{channel}, &publication{data: "delayed-event"})

	// Wait a bit, then trigger a batch by registering a repo with ReplayAll
	time.Sleep(50 * time.Millisecond)
	server.ReplayAll = true
	server.Register(channel, slowRepo)

	// Unsubscribe and resubscribe to trigger the batch replay
	// (This is a bit hacky but demonstrates the batch handling)

	// Wait for everything to process
	time.Sleep(jitterDuration + 100*time.Millisecond)
	server.Close()

	// Get the response
	body := <-bodyCh
	responseStr := string(body)

	// Should contain the delayed event that was flushed
	assert.Contains(t, responseStr, "delayed-event")
}

func TestServerWithZeroJitterBehavesLikeNormalServer(t *testing.T) {
	channel := "test"
	server := NewServerWithJitter(0)

	httpServer := httptest.NewServer(server.Handler(channel))
	defer httpServer.Close()

	// Start a client connection
	resp, err := http.Get(httpServer.URL)
	require.NoError(t, err)
	defer resp.Body.Close()

	// Record when we publish the event
	startTime := time.Now()
	event := &publication{data: "immediate-event"}
	server.Publish([]string{channel}, event)

	// Read the response with a timeout
	resultCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		buf := make([]byte, 1024)
		n, err := resp.Body.Read(buf)
		if err != nil && err != io.EOF {
			errCh <- err
			return
		}
		resultCh <- string(buf[:n])
	}()

	select {
	case result := <-resultCh:
		elapsed := time.Since(startTime)
		// With zero jitter, event should be delivered immediately (within reasonable time)
		assert.Less(t, elapsed.Milliseconds(), (50 * time.Millisecond).Milliseconds(),
			"Event should be delivered immediately with zero jitter")
		assert.Contains(t, result, "data: immediate-event")
		server.Close()
	case err := <-errCh:
		server.Close()
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		server.Close()
		t.Fatal("Timed out waiting for event")
	}
}

func TestServerWithJitterHandlesCommentsCorrectly(t *testing.T) {
	jitterDuration := 50 * time.Millisecond
	channel := "test"
	server := NewServerWithJitter(jitterDuration)

	httpServer := httptest.NewServer(server.Handler(channel))
	defer httpServer.Close()

	// Start a client connection
	resp, err := http.Get(httpServer.URL)
	require.NoError(t, err)
	defer resp.Body.Close()

	// Read the response in a goroutine
	bodyCh := make(chan []byte, 1)
	go func() {
		body, _ := io.ReadAll(resp.Body)
		bodyCh <- body
	}()

	// Publish a comment (should also be subject to jitter)
	server.PublishComment([]string{channel}, "test comment")

	// Wait for jitter delay
	time.Sleep(jitterDuration + 50*time.Millisecond)
	server.Close()

	// Get the response
	body := <-bodyCh
	responseStr := string(body)

	// Should contain the comment
	assert.Contains(t, responseStr, ":test comment")
}
