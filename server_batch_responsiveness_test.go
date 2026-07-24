package eventsource

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// floodingRepo returns a batch channel that is continuously ready with events and never
// closes, until stop is signalled. It models a Repository that floods the handler during
// replay -- the case that would starve the handler's select loop if the handler drained
// the batch in a tight inner loop instead of returning to select between events.
type floodingRepo struct {
	data string
	stop <-chan struct{}
}

func (r floodingRepo) Replay(channel, id string) chan Event {
	out := make(chan Event)
	go func() {
		defer close(out)
		ev := prerenderedFloodEvent{data: r.data}
		for {
			select {
			case out <- ev:
			case <-r.stop:
				return
			}
		}
	}()
	return out
}

type prerenderedFloodEvent struct{ data string }

func (e prerenderedFloodEvent) Id() string    { return "" } //nolint:revive
func (e prerenderedFloodEvent) Event() string { return "put" }
func (e prerenderedFloodEvent) Data() string  { return e.data }

// TestMaxConnTimeInterruptsFloodingReplay proves that MaxConnTime is honored even while
// a batch replay is actively flooding the handler with events. Because each batch event
// now flows through one iteration of the main select loop, the maxConnTimeCh case is
// evaluated between events and the handler exits promptly -- rather than staying away
// from the select loop until the (here, unbounded) batch drains, which would ignore
// MaxConnTime entirely.
func TestMaxConnTimeInterruptsFloodingReplay(t *testing.T) {
	stop := make(chan struct{})
	defer close(stop)

	server := NewServer()
	server.ReplayAll = true
	server.MaxConnTime = 100 * time.Millisecond
	defer server.Close()
	server.Register("test", floodingRepo{data: "hello", stop: stop})

	httpServer := httptest.NewServer(server.Handler("test"))
	defer httpServer.Close()

	start := time.Now()
	resp, err := http.Get(httpServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Drain the body until the server closes the connection at MaxConnTime. If the
	// handler were stuck draining the unbounded batch, this would never return and the
	// test would time out.
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	events := 0
	for scanner.Scan() {
		if len(scanner.Bytes()) > 0 {
			events++
		}
	}
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Fatalf("handler did not honor MaxConnTime during replay: took %v", elapsed)
	}
	if events == 0 {
		t.Fatal("expected to receive at least some replayed events before MaxConnTime")
	}
	t.Logf("MaxConnTime honored mid-replay: connection closed after %v having delivered ~%d lines", elapsed, events)
}
