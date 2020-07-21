package eventsource

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

type subscription struct {
	channel     string
	lastEventID string
	out         chan interface{}
	closeOnce   sync.Once
}

type eventOrComment interface{}

type outbound struct {
	channels       []string
	eventOrComment eventOrComment
	ackCh          chan<- struct{}
}

type registration struct {
	channel    string
	repository Repository
}

type unregistration struct {
	channel         string
	forceDisconnect bool
}

type comment struct {
	value string
}

// Server manages any number of event-publishing channels and allows subscribers to consume them.
// To use it within an HTTP server, create a handler for each channel with Handler().
type Server struct {
	AllowCORS       bool          // Enable all handlers to be accessible from any origin
	ReplayAll       bool          // Replay repository even if there's no Last-Event-Id specified
	BufferSize      int           // How many messages do we let the client get behind before disconnecting
	Gzip            bool          // Enable compression if client can accept it
	MaxConnTime     time.Duration // If non-zero, HTTP connections will be automatically closed after this time
	Logger          Logger        // Logger is a logger that, when set, will be used for logging debug messages
	registrations   chan *registration
	unregistrations chan *unregistration
	pub             chan *outbound
	subs            chan *subscription
	unsubs          chan *subscription
	quit            chan bool
	isClosed        bool
	isClosedMutex   sync.RWMutex
}

// NewServer creates a new Server instance.
func NewServer() *Server {
	srv := &Server{
		registrations:   make(chan *registration),
		unregistrations: make(chan *unregistration),
		pub:             make(chan *outbound),
		subs:            make(chan *subscription),
		unsubs:          make(chan *subscription, 2),
		quit:            make(chan bool),
		BufferSize:      128,
	}
	go srv.run()
	return srv
}

// Close permanently shuts down the Server. It will no longer allow new subscriptions.
func (srv *Server) Close() {
	srv.quit <- true
	srv.markServerClosed()
}

// Handler creates a new HTTP handler for serving a specified channel.
//
// The channel does not have to have been previously registered with Register, but if it has been, the
// handler may replay events from the registered Repository depending on the setting of server.ReplayAll
// and the Last-Event-Id header of the request.
func (srv *Server) Handler(channel string) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		h := w.Header()
		h.Set("Content-Type", "text/event-stream; charset=utf-8")
		h.Set("Cache-Control", "no-cache, no-store, must-revalidate")
		h.Set("Connection", "keep-alive")
		if srv.AllowCORS {
			h.Set("Access-Control-Allow-Origin", "*")
		}
		useGzip := srv.Gzip && strings.Contains(req.Header.Get("Accept-Encoding"), "gzip")
		if useGzip {
			h.Set("Content-Encoding", "gzip")
		}
		w.WriteHeader(http.StatusOK)

		// If the Handler is still active even though the server is closed, stop here.
		// Otherwise the Handler will block while publishing to srv.subs indefinitely.
		if srv.isServerClosed() {
			return
		}

		var maxConnTimeCh <-chan time.Time
		if srv.MaxConnTime > 0 {
			t := time.NewTimer(srv.MaxConnTime)
			defer t.Stop()
			maxConnTimeCh = t.C
		}

		sub := &subscription{
			channel:     channel,
			lastEventID: req.Header.Get("Last-Event-ID"),
			out:         make(chan interface{}, srv.BufferSize),
		}
		srv.subs <- sub
		flusher := w.(http.Flusher)
		//nolint: megacheck  // http.CloseNotifier is deprecated, but currently we are retaining compatibility with Go 1.7
		notifier := w.(http.CloseNotifier)
		flusher.Flush()
		enc := NewEncoder(w, useGzip)
		for {
			select {
			case <-notifier.CloseNotify():
				srv.unsubs <- sub
				return
			case <-maxConnTimeCh: // if MaxConnTime was not set, this is a nil channel and has no effect on the select
				srv.unsubs <- sub // we treat this the same as if the client closed the connection
				return
			case ev, ok := <-sub.out:
				if !ok {
					return
				}
				if err := enc.Encode(ev); err != nil {
					srv.unsubs <- sub
					if srv.Logger != nil {
						srv.Logger.Println(err)
					}
					return
				}
				flusher.Flush()
			}
		}
	}
}

// Register registers a Repository to be used for the specified channel. The Repository will be used to
// determine whether new subscribers should receive data that was generated before they subscribed.
//
// Channels do not have to be registered unless you want to specify a Repository. An unregistered channel can
// still be subscribed to with Handler, and published to with Publish.
func (srv *Server) Register(channel string, repo Repository) {
	srv.registrations <- &registration{
		channel:    channel,
		repository: repo,
	}
}

// Unregister removes a channel registration that was created by Register. If forceDisconnect is true, it also
// causes all currently active handlers for that channel to close their connections. If forceDisconnect is false,
// those connections will remain open until closed by their clients but will not receive any more events.
//
// This will not prevent creating new channel subscriptions for the same channel with Handler, or publishing
// events to that channel with Publish. It is the caller's responsibility to avoid using channels that are no
// longer supposed to be used.
func (srv *Server) Unregister(channel string, forceDisconnect bool) {
	srv.unregistrations <- &unregistration{
		channel:         channel,
		forceDisconnect: forceDisconnect,
	}
}

// Publish publishes an event to one or more channels.
func (srv *Server) Publish(channels []string, ev Event) {
	srv.pub <- &outbound{
		channels:       channels,
		eventOrComment: ev,
	}
}

// PublishWithAcknowledgment publishes an event to one or more channels, returning a channel that will receive
// a value after the event has been processed by the server.
//
// This can be used to ensure a well-defined ordering of operations. Since each Server method is handled
// asynchronously via a separate channel, if you call server.Publish and then immediately call server.Close,
// there is no guarantee that the server execute the Close operation only after the event has been published.
// If you instead call PublishWithAcknowledgement, and then read from the returned channel before calling
// Close, you can be sure that the event was published before the server was closed.
func (srv *Server) PublishWithAcknowledgment(channels []string, ev Event) <-chan struct{} {
	ackCh := make(chan struct{}, 1)
	srv.pub <- &outbound{
		channels:       channels,
		eventOrComment: ev,
		ackCh:          ackCh,
	}
	return ackCh
}

// PublishComment publishes a comment to one or more channels.
func (srv *Server) PublishComment(channels []string, text string) {
	srv.pub <- &outbound{
		channels:       channels,
		eventOrComment: comment{value: text},
	}
}

func replay(repo Repository, sub *subscription) {
	for ev := range repo.Replay(sub.channel, sub.lastEventID) {
		sub.out <- ev
	}
}

func (srv *Server) run() {
	subs := make(map[string]map[*subscription]struct{})
	repos := make(map[string]Repository)
	for {
		select {
		case reg := <-srv.registrations:
			repos[reg.channel] = reg.repository
		case unreg := <-srv.unregistrations:
			delete(repos, unreg.channel)
			previousSubs := subs[unreg.channel]
			delete(subs, unreg.channel)
			if unreg.forceDisconnect {
				for s := range previousSubs {
					s.Close()
				}
			}
		case sub := <-srv.unsubs:
			delete(subs[sub.channel], sub)
		case pub := <-srv.pub:
			for _, c := range pub.channels {
				for s := range subs[c] {
					select {
					case s.out <- pub.eventOrComment:
					default:
						srv.unsubs <- s
						close(s.out)
					}
				}
			}
			if pub.ackCh != nil {
				select {
				// It shouldn't be possible for this channel to block since it is created for a single use, but
				// we'll do a non-blocking push just to be safe
				case pub.ackCh <- struct{}{}:
				default:
				}
			}
		case sub := <-srv.subs:
			if _, ok := subs[sub.channel]; !ok {
				subs[sub.channel] = make(map[*subscription]struct{})
			}
			subs[sub.channel][sub] = struct{}{}
			if srv.ReplayAll || len(sub.lastEventID) > 0 {
				repo, ok := repos[sub.channel]
				if ok {
					go replay(repo, sub)
				}
			}
		case <-srv.quit:
			for _, sub := range subs {
				for s := range sub {
					close(s.out)
				}
			}
			return
		}
	}
}

func (srv *Server) isServerClosed() bool {
	srv.isClosedMutex.RLock()
	defer srv.isClosedMutex.RUnlock()
	return srv.isClosed
}

func (srv *Server) markServerClosed() {
	srv.isClosedMutex.Lock()
	defer srv.isClosedMutex.Unlock()
	srv.isClosed = true
}

func (s *subscription) Close() {
	s.closeOnce.Do(func() {
		close(s.out)
	})
}
