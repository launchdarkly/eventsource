package eventsource

import (
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

	"github.com/launchdarkly/go-test-helpers/v3/httphelpers"
)

// TestStreamCloseDuringEventDeliveryDoesNotDeadlock is a regression test for a deadlock in the
// stream goroutine's shutdown path (discardCurrentStream). When Close raced with an event that had
// just been decoded, discardCurrentStream drained the internal errs channel before events while the
// decoder goroutine was blocked sending on events, so neither side progressed and stream.Events was
// never closed. A consumer ranging over stream.Events to drain it on shutdown would then hang
// forever. See discardCurrentStream in stream.go.
//
// The race is timing-dependent, so this floods the stream and closes mid-delivery, repeated many
// times to make the interleaving overwhelmingly likely. On regression the consumer's drain never
// finishes and the test times out, printing all goroutines to pinpoint the block.
func TestStreamCloseDuringEventDeliveryDoesNotDeadlock(t *testing.T) {
	for i := 0; i < 100; i++ {
		func() {
			streamHandler, streamControl := httphelpers.SSEHandler(nil)
			defer streamControl.Close()
			httpServer := httptest.NewServer(streamHandler)
			defer httpServer.Close()

			stream := mustSubscribe(t, httpServer.URL)

			// Flood the stream so the decoder is very likely mid-delivery of an event when we Close.
			go func() {
				for j := 0; j < 500; j++ {
					streamControl.Enqueue(httphelpers.SSEEvent{ID: "id", Data: "data"})
				}
			}()

			// A real consumer drains Events (and Errors) until they close on shutdown; that only
			// happens once the stream goroutine exits cleanly, so this is what deadlocked.
			drained := make(chan struct{})
			go func() {
				for range stream.Events {
				}
				for range stream.Errors {
				}
				close(drained)
			}()

			time.Sleep(2 * time.Millisecond)
			stream.Close()

			select {
			case <-drained:
			case <-time.After(5 * time.Second):
				buf := make([]byte, 1<<20)
				n := runtime.Stack(buf, true)
				t.Fatalf("stream.Events was never closed after Close (deadlock) on iteration %d\n%s", i, buf[:n])
			}
		}()
	}
}
