package eventsource_test

import (
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/launchdarkly/eventsource"
)

type TimeEvent time.Time

func (t TimeEvent) Id() string    { return fmt.Sprint(time.Time(t).UnixNano()) }
func (t TimeEvent) Event() string { return "Tick" }
func (t TimeEvent) Data() string  { return time.Time(t).String() }

const (
	TICK_COUNT = 5
)

func TimePublisher(srv *eventsource.Server) {
	start := time.Date(2013, time.January, 1, 0, 0, 0, 0, time.UTC)
	ticker := time.NewTicker(1 * time.Millisecond)
	for range TICK_COUNT {
		<-ticker.C
		srv.Publish([]string{"time"}, TimeEvent(start))
		start = start.Add(time.Second)
	}
}

// subscribedSignal is a Repository with nothing to replay. The Server asks it for a replay
// only after it registers a subscriber, so the call shows that published events will arrive.
type subscribedSignal struct {
	once sync.Once
	ch   chan struct{}
}

func (s *subscribedSignal) Replay(channel, id string) chan eventsource.Event {
	s.once.Do(func() { close(s.ch) })
	out := make(chan eventsource.Event)
	close(out)
	return out
}

func ExampleEvent() {
	srv := eventsource.NewServer()
	srv.Gzip = true
	srv.ReplayAll = true
	subscribed := &subscribedSignal{ch: make(chan struct{})}
	srv.Register("time", subscribed)
	defer srv.Close()
	l, err := net.Listen("tcp", ":8080")
	if err != nil {
		return
	}
	defer l.Close()
	http.HandleFunc("/time", srv.Handler("time"))
	go http.Serve(l, nil)
	stream, err := eventsource.Subscribe("http://127.0.0.1:8080/time", "")
	if err != nil {
		return
	}
	// Events published before the Server registers the subscriber reach nobody.
	<-subscribed.ch
	go TimePublisher(srv)
	for range TICK_COUNT {
		ev := <-stream.Events
		fmt.Println(ev.Id(), ev.Event(), ev.Data())
	}

	// Output:
	// 1356998400000000000 Tick 2013-01-01 00:00:00 +0000 UTC
	// 1356998401000000000 Tick 2013-01-01 00:00:01 +0000 UTC
	// 1356998402000000000 Tick 2013-01-01 00:00:02 +0000 UTC
	// 1356998403000000000 Tick 2013-01-01 00:00:03 +0000 UTC
	// 1356998404000000000 Tick 2013-01-01 00:00:04 +0000 UTC
}
