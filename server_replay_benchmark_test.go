package eventsource

import (
	"bufio"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type prerenderedEvent struct {
	name string
	data string
}

func (e prerenderedEvent) Id() string    { return "" } //nolint:revive
func (e prerenderedEvent) Event() string { return e.name }
func (e prerenderedEvent) Data() string  { return e.data }

// batchRepository replays a fixed set of events through a buffered channel, so every
// event is immediately available to the handler. This models a server-side proxy
// replaying a large pre-rendered data set to a newly connected client.
type batchRepository struct {
	events []Event
}

func (r batchRepository) Replay(channel, id string) chan Event {
	out := make(chan Event, len(r.events))
	for _, e := range r.events {
		out <- e
	}
	close(out)
	return out
}

// unbufferedRepository replays the same events through an unbuffered channel fed by a
// goroutine, one rendezvous per event. This models callers (like ld-relay today) that
// stream replay events instead of preloading them.
type unbufferedRepository struct {
	events []Event
}

func (r unbufferedRepository) Replay(channel, id string) chan Event {
	out := make(chan Event)
	go func() {
		defer close(out)
		for _, e := range r.events {
			out <- e
		}
	}()
	return out
}

func benchmarkReplayBatch(b *testing.B, numEvents, dataSize int) {
	benchmarkReplay(b, numEvents, dataSize, func(events []Event) Repository {
		return batchRepository{events: events}
	})
}

func benchmarkReplayUnbuffered(b *testing.B, numEvents, dataSize int) {
	benchmarkReplay(b, numEvents, dataSize, func(events []Event) Repository {
		return unbufferedRepository{events: events}
	})
}

func benchmarkReplay(b *testing.B, numEvents, dataSize int, makeRepo func([]Event) Repository) {
	events := make([]Event, 0, numEvents)
	data := strings.Repeat("x", dataSize)
	for i := 0; i < numEvents; i++ {
		events = append(events, prerenderedEvent{name: "put-object", data: data})
	}

	server := NewServer()
	server.ReplayAll = true
	defer server.Close()
	server.Register("test", makeRepo(events))

	httpServer := httptest.NewServer(server.Handler("test"))
	defer httpServer.Close()

	b.SetBytes(int64(numEvents * dataSize))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		resp, err := http.Get(httpServer.URL)
		if err != nil {
			b.Fatal(err)
		}
		scanner := bufio.NewScanner(resp.Body)
		count := 0
		for scanner.Scan() {
			if strings.HasPrefix(scanner.Text(), "data: ") {
				count++
				if count == numEvents {
					break
				}
			}
		}
		_ = resp.Body.Close()
		if count != numEvents {
			b.Fatalf("expected %d events, got %d", numEvents, count)
		}
	}
}

func BenchmarkReplayBatch(b *testing.B) {
	for _, bc := range []struct{ numEvents, dataSize int }{
		{100, 300},
		{1000, 300},
		{5000, 300},
		{5000, 2000},
	} {
		b.Run(fmt.Sprintf("%devents_%dB", bc.numEvents, bc.dataSize), func(b *testing.B) {
			benchmarkReplayBatch(b, bc.numEvents, bc.dataSize)
		})
	}
}

func BenchmarkReplayUnbuffered(b *testing.B) {
	for _, bc := range []struct{ numEvents, dataSize int }{
		{5000, 300},
	} {
		b.Run(fmt.Sprintf("%devents_%dB", bc.numEvents, bc.dataSize), func(b *testing.B) {
			benchmarkReplayUnbuffered(b, bc.numEvents, bc.dataSize)
		})
	}
}
