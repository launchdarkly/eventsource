package eventsource

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"
)

type subscription struct {
	channel     string
	lastEventID string
	out         chan<- eventOrComment
	// ctx is the subscribing request's context. It is cancelled when the subscriber disconnects,
	// and is passed to a Repository that implements RepositoryWithContext.
	ctx context.Context
	// batch is the replay batch channel that was handed to this subscription, if any. It is
	// recorded so that if the handler exits without ever dequeuing the batch from its buffered
	// event channel, the unsubscribe path can still drain it and unblock the Repository's
	// producer. Accessed only from the Server.run() goroutine.
	batch <-chan Event
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

type eventBatch struct {
	events <-chan Event
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
	// stopped is closed when run() exits, so that handlers which outlive the Server
	// (e.g. a write error detected after Close) do not block forever sending an
	// unsubscription that nothing will ever consume.
	stopped       chan struct{}
	isClosed      bool
	isClosedMutex sync.RWMutex
	jitter        time.Duration
}

// NewServer creates a new Server instance.
func NewServer() *Server {
	var duration time.Duration
	return NewServerWithJitter(duration)
}

// NewServerWithJitter creates a new Server instance with jitter support.
//
// WARNING: Intermediate events sent while another event send is pending WILL
// BE DISCARDED. Loss of data will occur if used incorrectly.
//
// This method is for use by LaunchDarkly libraries ONLY. No guarantee is made
// about backwards compatibility or future support.
func NewServerWithJitter(jitter time.Duration) *Server {
	srv := &Server{
		registrations:   make(chan *registration),
		unregistrations: make(chan *unregistration),
		pub:             make(chan *outbound),
		subs:            make(chan *subscription),
		unsubs:          make(chan *subscription, 2),
		quit:            make(chan bool),
		stopped:         make(chan struct{}),
		BufferSize:      128,
		jitter:          jitter,
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

		eventCh := make(chan eventOrComment, srv.BufferSize)
		sub := &subscription{
			channel:     channel,
			lastEventID: req.Header.Get("Last-Event-ID"),
			out:         eventCh,
			ctx:         req.Context(),
		}
		srv.subs <- sub
		flusher := w.(http.Flusher)
		flusher.Flush()
		enc := NewEncoder(w, useGzip)

		// unsubscribe tells the Server this handler is going away. After the Server has
		// shut down nothing consumes unsubs (and its small buffer may already be full),
		// so a handler that exits late must not block forever on the send.
		unsubscribe := func() {
			select {
			case srv.unsubs <- sub:
			case <-srv.stopped:
			}
		}

		writeEventOrComment := func(ec eventOrComment) bool {
			if err := enc.Encode(ec); err != nil {
				unsubscribe()
				if srv.Logger != nil {
					srv.Logger.Println(err)
				}
				return false // if this happens, we'll end the handler early because something's clearly broken
			}
			flusher.Flush()
			return true
		}

		// The logic below works as follows:
		// - Normally, the handler is reading from eventCh. Server.run() accesses this channel through sub.out
		//   and sends published events to it.
		// - However, if a Repository is being used, the Server might get a whole batch of events that the
		//   Repository provides through its Replay method. The Repository provides these in the form of a
		//   channel that it writes to. Since we don't know how many events there will be or how long it will
		//   take to write them, we do not want to block Server.run() for this.
		// - Previous implementations of sending events from Replay used a separate goroutine. That was unsafe,
		//   due to a race condition where Server.run() might close the channel while the Replay goroutine is
		//   still writing to it.
		// - So, instead, Server.run() now takes the channel from Replay and wraps it in an eventBatch. When
		//   the handler sees an eventBatch, it switches over to reading events from that channel until the
		//   channel is closed. Then it switches back to reading events from the regular channel.
		// - The Server can close eventCh at any time to indicate that the stream is done. The handler exits.
		// - If the client closes the connection, or if MaxConnTime elapses, the handler exits after telling
		//   the Server to stop publishing events to it.

		var readMainCh <-chan eventOrComment = eventCh
		var readBatchCh <-chan Event
		closedNormally := false
		closeNotify := req.Context().Done()

		// The handler consumes events in two different modes -- either as soon as
		// they arrive, or on some jitter-influenced delay.
		//
		// If the jitter value has been provided, the first event received will be
		// delayed for some (delay/2, delay) amount of time. Events received until
		// then are DISCARDED. If this seems excessive, it is!
		//
		// This jitter functionality is only meant to service the ping stream
		// functionality. The ping stream sends identical "ping" events, so
		// discarding intermediate values is a safe operation.

		var delayedEvent eventOrComment
		jitterStrategy := newDefaultJitter(0.5, 0)

		usingJitter := srv.jitter > 0
		var jitterTimer timer
		if usingJitter {
			jitterTimer = &goTimer{timer: time.NewTimer(jitterStrategy.applyJitter(srv.jitter))}
			jitterTimer.Stop()
		} else {
			jitterTimer = &noopTimer{C: make(<-chan time.Time)}
		}

	ReadLoop:
		for {
			select {
			case <-closeNotify:
				break ReadLoop
			case <-maxConnTimeCh: // if MaxConnTime was not set, this is a nil channel and has no effect on the select
				break ReadLoop
			case <-jitterTimer.Channel():
				// If the jitter is 0, we may have an initial event that fired before
				// we could stop the timer. Or maybe the channel is being closed.
				// Whatever the reason, we can safely discard here.
				if !usingJitter || delayedEvent == nil {
					continue
				}

				ok := writeEventOrComment(delayedEvent)
				delayedEvent = nil

				if !ok {
					break ReadLoop
				}
			case ev, ok := <-readMainCh:
				if !ok {
					closedNormally = true
					break ReadLoop
				}

				if batch, ok := ev.(eventBatch); ok {
					// If we receive an event batch, we are meant to switch to this as
					// our input source. But before we can do that, we need to process
					// any event that was pending processing.
					if delayedEvent != nil {
						jitterTimer.Stop()
						ok := writeEventOrComment(delayedEvent)
						delayedEvent = nil

						if !ok {
							break ReadLoop
						}
					}

					readBatchCh = batch.events
					readMainCh = nil
					continue
				}

				// Write immediately if we aren't using the jitter functionality.
				if !usingJitter {
					if !writeEventOrComment(ev) {
						break ReadLoop
					}
					continue
				}

				// If we are using jitter and we have a pending event, then we don't
				// need to do anything. We can swallow this event.
				if delayedEvent != nil {
					continue
				}

				delayedEvent = ev

				// Figure out the jitter and start the timer. Once this trigger, we
				// will write the event and clear the way for a new event to come in.
				delay := jitterStrategy.applyJitter(srv.jitter)
				jitterTimer.Reset(delay)

			case ev, ok := <-readBatchCh:
				if !ok { // end of batch
					readBatchCh = nil
					readMainCh = eventCh
				} else if !writeEventOrComment(ev) {
					break ReadLoop
				}
			}
		}
		if readBatchCh != nil {
			// We are exiting the read loop while still in the middle of consuming a batch of replayed
			// events from a Repository (e.g. the subscriber disconnected, or MaxConnTime elapsed). The
			// Repository's producer goroutine may be blocked trying to send the remaining events on this
			// channel. Since we hold the receiving end -- we can neither close it nor keep reading it on
			// this exiting goroutine -- drain it in the background so the producer can unblock and release
			// its resources promptly rather than leaking until the process exits.
			//
			// A Repository that implements RepositoryWithContext will already have been told to stop via
			// context cancellation, but draining is harmless in that case and remains the safety net for
			// repositories that only implement Replay.
			go drainReplayedEvents(readBatchCh)
		}
		// A replay batch that the Server queued on eventCh but that the loop above never dequeued
		// would strand its producer the same way. Server.run() drains such a batch when it
		// processes our unsubscription (see the unsubs case there); this sweep of the values
		// already buffered additionally covers the case where the Server has shut down and will
		// never process it. Anything the Server enqueues concurrently with this sweep is still
		// handled by the unsubs path.
	SweepPending:
		for {
			select {
			case ev, ok := <-eventCh:
				if !ok {
					break SweepPending
				}
				if batch, isBatch := ev.(eventBatch); isBatch {
					go drainReplayedEvents(batch.events)
				}
			default:
				break SweepPending
			}
		}
		if !closedNormally {
			unsubscribe() // the server didn't tell us to close, so we must tell it that we're closing
		}
	}
}

// drainReplayedEvents consumes and discards the events from a Repository replay batch channel that
// no subscriber will read again, so the goroutine producing those events can complete and release
// its resources instead of blocking forever on a send.
func drainReplayedEvents(ch <-chan Event) {
	for range ch { //nolint:revive // draining until the channel is closed
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

func (srv *Server) run() {
	defer close(srv.stopped)
	// All access to the subs and repos maps is done from the same goroutine, so modifications are safe.
	subs := make(map[string]map[*subscription]struct{})
	repos := make(map[string]Repository)
	trySend := func(sub *subscription, ec eventOrComment) {
		if !sub.send(ec) {
			sub.close()
			delete(subs[sub.channel], sub)
		}
	}
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
					s.close()
					// Unlike the unsubscription and shutdown cases, no batch drain is needed
					// here: the server keeps running. A buffered channel delivers its queued
					// values before reporting closed, so the handler still dequeues and fully
					// consumes an in-flight batch after close(out); if the handler instead
					// exits abnormally, its own exit paths and its unsubscription (which run()
					// is still alive to process) drain the batch.
				}
			}
		case sub := <-srv.unsubs:
			delete(subs[sub.channel], sub)
			if sub.batch != nil {
				// The handler has exited. If it never dequeued the replay batch from its event
				// channel -- or exited partway through consuming it -- the Repository's producer
				// may still be blocked sending on it; drain it so the producer can finish. If the
				// batch was fully consumed, the channel is already closed and this goroutine exits
				// immediately. Draining concurrently with the handler's own exit-time drain is
				// safe: both simply receive until the channel is closed.
				go drainReplayedEvents(sub.batch)
				sub.batch = nil
			}
		case pub := <-srv.pub:
			for _, c := range pub.channels {
				for s := range subs[c] {
					trySend(s, pub.eventOrComment)
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
					// If the repository supports it, pass the subscriber's context so its producer can
					// stop sending promptly when the subscriber disconnects. Otherwise fall back to the
					// original context-less Replay; the handler's background drain (see Handler) still
					// ensures such a producer eventually unblocks.
					var batchCh <-chan Event
					if repoCtx, ok := repo.(RepositoryWithContext); ok {
						batchCh = repoCtx.ReplayWithContext(sub.ctx, sub.channel, sub.lastEventID)
					} else {
						batchCh = repo.Replay(sub.channel, sub.lastEventID)
					}
					if batchCh != nil {
						if sub.send(eventBatch{events: batchCh}) {
							// Remember the batch so that if the subscriber goes away before its
							// handler dequeues it, the unsubs case below can still drain it.
							sub.batch = batchCh
						} else {
							// The send failed because the subscription's buffer was full (send
							// closes the subscription in that case). The batch will never be
							// consumed and its producer would otherwise block forever; drain it
							// in the background.
							delete(subs[sub.channel], sub)
							go drainReplayedEvents(batchCh)
						}
					}
				}
			}
		case <-srv.quit:
			// We are about to stop processing unsubscriptions, so first handle any that are
			// already queued: their handlers have exited, and a handler that swept its event
			// channel before the replay batch was enqueued is relying on this path to drain it.
			// Subscriptions handled here are deliberately not removed from the map -- the loop
			// below revisits them, but with batch already nil and close() being idempotent
			// that revisit is a no-op.
		DrainUnsubs:
			for {
				select {
				case sub := <-srv.unsubs:
					if sub.batch != nil {
						go drainReplayedEvents(sub.batch)
						sub.batch = nil
					}
				default:
					break DrainUnsubs
				}
			}
			for _, sub := range subs {
				for s := range sub {
					s.close()
					// If the subscriber is already gone, its handler can no longer be relied on
					// to consume or drain a batch, and its unsubscription may never be seen
					// (we are exiting); drain the batch here. If the subscriber is still
					// connected, we must NOT drain: its handler keeps delivering the buffered
					// batch to the client even after close(s.out), and draining would steal
					// events from that delivery.
					if s.batch != nil && s.ctx.Err() != nil {
						go drainReplayedEvents(s.batch)
						s.batch = nil
					}
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

// Attempts to send an event or comment to the subscription's channel.
//
// We do not want to block the main Server goroutine, so this is a non-blocking send. If it fails,
// we return false to tell the Server that the subscriber has fallen behind and should be removed;
// we also immediately close the channel in that case. If the send succeeds-- or if we didn't need
// to attempt a send, because the channel was already closed-- we return true.
//
// This should be called only from the Server.run() goroutine.
func (s *subscription) send(e eventOrComment) bool {
	if s.out == nil {
		return true
	}
	select {
	case s.out <- e:
		return true
	default:
		s.close()
		return false
	}
}

// Closes a subscription's channel and sets it to nil.
//
// This should be called only from the Server.run() goroutine.
func (s *subscription) close() {
	if s.out == nil {
		return
	}

	close(s.out)
	s.out = nil
}

type timer interface {
	Channel() <-chan time.Time
	Reset(time.Duration) bool
	Stop() bool
}

type noopTimer struct {
	C <-chan time.Time
}

func (n *noopTimer) Channel() <-chan time.Time {
	return n.C
}

func (n *noopTimer) Reset(_ time.Duration) bool {
	return true
}

func (n *noopTimer) Stop() bool {
	return true
}

type goTimer struct {
	timer *time.Timer
}

func (t *goTimer) Channel() <-chan time.Time {
	return t.timer.C
}

func (t *goTimer) Reset(d time.Duration) bool {
	return t.timer.Reset(d)
}

func (t *goTimer) Stop() bool {
	return t.timer.Stop()
}
