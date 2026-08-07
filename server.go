package eventsource

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
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
	id    uint64
	// closeReason records why the Server closed this subscription. It is written
	// on the Server.run() goroutine before out is closed, and read by the handler
	// only after it observes out being closed. That channel close is the
	// happens-before edge that makes this plain field access race-free.
	closeReason SubscriberRemovedReason
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
	AllowCORS   bool          // Enable all handlers to be accessible from any origin
	ReplayAll   bool          // Replay repository even if there's no Last-Event-Id specified
	BufferSize  int           // How many messages do we let the client get behind before disconnecting
	Gzip        bool          // Enable compression if client can accept it
	MaxConnTime time.Duration // If non-zero, HTTP connections will be automatically closed after this time
	// Logger, when set, receives DEBUG lines for subscriber lifecycle events
	// (add, remove, replay drain), a WARN line when a slow subscriber is
	// dropped, and write errors. Lines identify connections by an opaque
	// subscriber id and never include the channel name, because channel names
	// may contain values (such as credentials) that must not appear in logs.
	Logger Logger
	// Trace, when set, receives callbacks at points in the Server's lifecycle. See
	// ServerTrace for the concurrency contract that callbacks must satisfy.
	//
	// EXPERIMENTAL: this field and the ServerTrace API are subject to change or
	// removal in any future release. See ServerTrace.
	Trace           *ServerTrace
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
	subCounter    atomic.Uint64
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

// writeStreamHeaders writes the standard SSE response headers, negotiating gzip compression if the
// server allows it and the client accepts it, then commits them with a 200 status. It returns
// whether the response body must be gzip-encoded.
func (srv *Server) writeStreamHeaders(w http.ResponseWriter, req *http.Request) bool {
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
	return useGzip
}

// handlerState carries the state one Handler invocation shares between its
// read loop and its deferred teardown, so the teardown can live in methods
// rather than closures. exitReason, closedNormally, readBatchCh, the replay
// accounting, and delayedEvent are written by the read loop and read by the
// teardown; every access is on the single handler goroutine.
type handlerState struct {
	srv       *Server
	sub       *subscription
	ctx       context.Context
	channel   string
	eventCh   chan eventOrComment
	flusher   http.Flusher
	enc       *Encoder
	connStart time.Time

	// exitReason is set at each point the read loop can exit, so that
	// SubscriberRemoved can report why the connection ended. For a close
	// initiated by the Server it is read from the subscription after the
	// event channel is observed closed.
	exitReason     SubscriberRemovedReason
	closedNormally bool
	reportedAdded  bool

	readBatchCh <-chan Event
	replayStart time.Time
	replayCount int
	replayBytes int64

	// delayedEvent is an event parked by a jitter-enabled server until its
	// delay elapses; the teardown accounts for one still parked when the
	// connection ends.
	delayedEvent eventOrComment
}

// unsubscribe tells the Server this handler is going away. After the Server has
// shut down nothing consumes unsubs (and its small buffer may already be full),
// so a handler that exits late must not block forever on the send.
func (hs *handlerState) unsubscribe() {
	select {
	case hs.srv.unsubs <- hs.sub:
	case <-hs.srv.stopped:
	}
}

// reportExit is the exit-time reporting half of the teardown; it runs first
// and holds the code that invokes consumer callbacks. A panic here must not
// leak the subscription in the Server's map, strand a Repository producer, or
// leave a SubscriberAdded without its matching SubscriberRemoved -- which is
// why cleanup runs in a separate, earlier-registered defer.
func (hs *handlerState) reportExit() {
	replayAborted := false
	batchToDrain := hs.readBatchCh
	if hs.readBatchCh != nil {
		replayAborted = true
		if hs.exitReason != ReasonWriteError {
			// A batch that fully drained before the connection ended -- only
			// its end-of-batch sentinel went unobserved, because the disconnect
			// and the batch end raced in the read loop's select -- is a
			// completed drain, not an aborted one. A write error is the
			// exception: the failing write is what ended the drain, so that
			// batch is aborted no matter what the channel state says. (In the
			// rare case that a Server shutdown's drain is concurrently consuming
			// this abandoned batch, the probe can misread the drain's close as
			// completion; Aborted is documented as best-effort for exactly this
			// race.)
			select {
			case _, ok := <-hs.readBatchCh:
				if !ok {
					replayAborted = false
					batchToDrain = nil // fully drained; nothing to hand over
				}
				// A received event was never written to the connection; it
				// belongs to the abandoned batch and is discarded with it.
			default:
			}
		}
	}
	// Producer liveness before consumer-reachable code: an abandoned replay
	// batch is handed to its background drain before the flush and the
	// callbacks below, so slow or panicking consumer code can neither delay
	// nor strand a Repository producer.
	if drainAbandonedBatches(batchToDrain, hs.eventCh) {
		// The Server closed the subscription while the handler was already exiting
		// for a reason of its own. The receive that observed the closed channel
		// orders the read of closeReason -- the same happens-before edge as the
		// closedNormally path -- and the reason the Server recorded is the
		// authoritative one: without this, a subscriber dropped for buffer overflow
		// while parked in a slow write would be reported as client_closed or
		// max_conn_time.
		hs.closedNormally = true
	}
	if hs.readBatchCh != nil && !replayAborted {
		// The completed batch still gets its end-of-batch flush, matching the
		// read loop's sentinel path and the DrainDuration contract.
		hs.flusher.Flush()
	}
	if hs.delayedEvent != nil {
		// An event was still parked awaiting its jitter delay when the
		// connection ended. Report it so that every event a subscriber
		// was sent is accounted for as either sent or discarded.
		hs.srv.traceEventDiscarded(hs.ctx, hs.channel, DiscardReasonConnectionEnded)
	}
	if hs.readBatchCh != nil {
		// A ReplayStarted with no batch-end sentinel observed still gets
		// its matching ReplayFinished, so the drained-so-far totals are
		// not lost.
		hs.srv.traceReplayFinished(hs.ctx, hs.sub, hs.replayCount, hs.replayBytes,
			sinceOrZero(hs.replayStart), replayAborted)
	}
}

// cleanup is the half of the teardown that must never be skipped: resolving
// the exit reason, unsubscribing, and reporting SubscriberRemoved. It is
// registered before reportExit so that it runs last and still runs while a
// panic from reportExit is unwinding. Unsubscribing a subscription the Server
// never registered is a harmless no-op, so both defers safely precede the
// registration send.
func (hs *handlerState) cleanup() {
	if hs.closedNormally {
		// Reason precedence when a Server-initiated close races the handler's
		// own exit: buffer_overflow outranks everything, because the
		// SubscriberDropped callback that already fired promises a removal
		// with the matching reason; a write error outranks the remaining
		// Server reasons, because the handler has already reported that
		// definitive local failure through WriteError; and any Server-recorded
		// reason outranks the read loop's speculative client_closed and
		// max_conn_time.
		if hs.sub.closeReason == ReasonBufferOverflow || hs.exitReason != ReasonWriteError {
			hs.exitReason = hs.sub.closeReason
		}
	} else {
		hs.unsubscribe() // the server didn't tell us to close, so we must tell it that we're closing
	}
	if hs.reportedAdded {
		hs.srv.traceSubscriberRemoved(hs.ctx, hs.sub, hs.exitReason, sinceOrZero(hs.connStart))
	}
}

func (hs *handlerState) writeEventOrComment(ec eventOrComment) bool {
	if err := hs.enc.Encode(ec); err != nil {
		// No unsubscribe here: the deferred cleanup sends it moments later, and
		// an early send would let run()'s unsubscription path start draining a
		// mid-drain replay batch underneath the teardown's completion probe.
		hs.exitReason = ReasonWriteError
		hs.srv.traceWriteError(hs.ctx, hs.channel, err)
		if hs.srv.Logger != nil {
			hs.srv.Logger.Println(err)
		}
		return false // if this happens, we'll end the handler early because something's clearly broken
	}
	return true
}

func (hs *handlerState) writeEventOrCommentAndFlush(ec eventOrComment) bool {
	return hs.srv.writeTraced(hs.ctx, hs.channel, ec, func() bool {
		if !hs.writeEventOrComment(ec) {
			return false
		}
		hs.flusher.Flush()
		return true
	})
}

// Handler creates a new HTTP handler for serving a specified channel.
//
// The channel does not have to have been previously registered with Register, but if it has been, the
// handler may replay events from the registered Repository depending on the setting of server.ReplayAll
// and the Last-Event-Id header of the request.
func (srv *Server) Handler(channel string) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		useGzip := srv.writeStreamHeaders(w, req)

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

		// ctx is the subscriber's request context. It is threaded into the trace
		// callbacks that fire on this handler goroutine, so a consumer can correlate
		// telemetry with the request span, and it is handed to the subscription for
		// the benefit of a Repository that implements RepositoryWithContext.
		ctx := req.Context()

		eventCh := make(chan eventOrComment, srv.BufferSize)
		sub := &subscription{
			channel:     channel,
			lastEventID: req.Header.Get("Last-Event-ID"),
			out:         eventCh,
			ctx:         ctx,
		}

		// measuringReplay gates the replay accounting -- the clock reads and the
		// extra per-event Data() call -- on something that will actually consume
		// it: the ReplayFinished callback or the Logger's replay line. This
		// mirrors shouldMeasureWrite's per-callback gating.
		measuringReplay := (srv.Trace != nil && srv.Trace.ReplayFinished != nil) || srv.Logger != nil

		hs := &handlerState{
			srv:     srv,
			sub:     sub,
			ctx:     ctx,
			channel: channel,
			eventCh: eventCh,
			flusher: w.(http.Flusher),
			// connStart is meaningful only when something is observing the
			// connection; beginSubscription returns the zero time otherwise,
			// which sinceOrZero maps to a zero duration.
			connStart: srv.beginSubscription(sub),
		}

		defer hs.cleanup()
		defer hs.reportExit()

		hs.flusher.Flush()
		// reportedAdded is set before the callback so that a panic inside
		// SubscriberAdded itself still produces the balancing SubscriberRemoved.
		hs.reportedAdded = true
		// SubscriberAdded fires before the subscription is registered with the
		// Server, so no other callback -- in particular SubscriberDropped, which
		// can fire on the dispatch goroutine as soon as the Server knows the
		// subscription -- can precede it. The HTTP response was already started
		// by writeStreamHeaders above.
		srv.traceSubscriberAdded(ctx, sub)

		// If the Server closed while this handler was starting up, nothing will
		// ever receive the registration; without the escape the handler would
		// park on this send forever, leaking the goroutine and pinning the
		// connection open.
		select {
		case srv.subs <- sub:
		case <-srv.stopped:
			hs.exitReason = ReasonServerClosed
			return
		}
		hs.enc = NewEncoder(w, useGzip)

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
		closeNotify := ctx.Done()

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
				hs.exitReason = ReasonClientClosed
				break ReadLoop
			case <-maxConnTimeCh: // if MaxConnTime was not set, this is a nil channel and has no effect on the select
				hs.exitReason = ReasonMaxConnTime
				break ReadLoop
			case <-jitterTimer.Channel():
				// If the jitter is 0, we may have an initial event that fired before
				// we could stop the timer. Or maybe the channel is being closed.
				// Whatever the reason, we can safely discard here.
				if !usingJitter || hs.delayedEvent == nil {
					continue
				}

				// Cleared before the write so that a panicking EventSent cannot
				// leave the event to also be reported as discarded by the
				// teardown.
				delayed := hs.delayedEvent
				hs.delayedEvent = nil

				if !hs.writeEventOrCommentAndFlush(delayed) {
					break ReadLoop
				}
			case ev, ok := <-readMainCh:
				if !ok {
					hs.closedNormally = true
					break ReadLoop
				}

				if batch, ok := ev.(eventBatch); ok {
					// If we receive an event batch, we are meant to switch to this as
					// our input source. But before we can do that, we need to process
					// any event that was pending processing.
					if hs.delayedEvent != nil {
						jitterTimer.Stop()
						// Cleared before the write, as in the timer case above.
						delayed := hs.delayedEvent
						hs.delayedEvent = nil

						if !hs.writeEventOrCommentAndFlush(delayed) {
							break ReadLoop
						}
					}

					hs.readBatchCh = batch.events
					readMainCh = nil
					hs.replayCount = 0
					hs.replayBytes = 0
					// replayStart resets with the counts so that, if ReplayStarted
					// panics below, the teardown's abort report cannot measure from
					// a previous batch's start.
					hs.replayStart = time.Time{}
					srv.traceReplayStarted(ctx, channel)
					// The drain clock starts after ReplayStarted returns, so the
					// consumer's own callback cost is not billed to DrainDuration.
					if measuringReplay {
						hs.replayStart = time.Now()
					}
					continue
				}

				// Write immediately if we aren't using the jitter functionality.
				if !usingJitter {
					if !hs.writeEventOrCommentAndFlush(ev) {
						break ReadLoop
					}
					continue
				}

				// If we are using jitter and we have a pending event, then we don't
				// need to do anything. We can swallow this event.
				if hs.delayedEvent != nil {
					srv.traceEventDiscarded(ctx, channel, DiscardReasonJitterCoalesce)
					continue
				}

				hs.delayedEvent = ev

				// Figure out the jitter and start the timer. Once this trigger, we
				// will write the event and clear the way for a new event to come in.
				delay := jitterStrategy.applyJitter(srv.jitter)
				jitterTimer.Reset(delay)

			case ev, ok := <-hs.readBatchCh:
				if !ok { // end of batch
					hs.flusher.Flush()
					hs.readBatchCh = nil
					readMainCh = eventCh
					// DrainDuration is measured after the flush above, so it accounts
					// for the batch's single flush rather than excluding it.
					srv.traceReplayFinished(ctx, sub, hs.replayCount, hs.replayBytes, sinceOrZero(hs.replayStart), false)
					continue
				}

				// Replayed events are not flushed individually, so they report no
				// EventSent; replay is observed at batch level via ReplayFinished.
				if !hs.writeEventOrComment(ev) {
					break ReadLoop
				}
				hs.replayCount++
				if measuringReplay {
					hs.replayBytes += int64(len(ev.Data()))
				}
			}
		}
	}
}

// drainAbandonedBatches unblocks Repository producers whose replay batches an exiting handler
// will never consume: the batch it was reading when it exited, and any batch still queued
// unread on its buffered event channel.
//
// current is the batch the handler was mid-way through consuming, or nil. Its producer may be
// blocked sending the remaining events; since the handler holds the receiving end -- it can
// neither close the channel nor keep reading it -- the batch is drained in the background so
// the producer can unblock and release its resources promptly. A Repository that implements
// RepositoryWithContext will already have been told to stop via context cancellation, but
// draining is harmless in that case and remains the safety net for repositories that only
// implement Replay.
//
// A batch the Server queued on the buffered event channel that the handler never dequeued
// would strand its producer the same way. Server.run() drains such a batch when it processes
// the handler's unsubscription; the sweep of the already-buffered values here additionally
// covers the case where the Server has shut down and will never process it. Anything the
// Server enqueues concurrently with this sweep is still handled by the unsubscription path.
//
// The return value reports whether the sweep observed eventCh closed. A closed channel means
// the Server ended this subscription and recorded a closeReason before closing, so the caller
// can treat the exit as Server-initiated even if its read loop left for another reason first.
func drainAbandonedBatches(current <-chan Event, eventCh <-chan eventOrComment) bool {
	if current != nil {
		go drainReplayedEvents(current)
	}
	for {
		select {
		case ev, ok := <-eventCh:
			if !ok {
				return true
			}
			if batch, isBatch := ev.(eventBatch); isBatch {
				go drainReplayedEvents(batch.events)
			}
		default:
			return false
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

// replay obtains the replay channel for a new subscription. If the repository supports it, the
// subscriber's context is passed so the repository's producer can stop sending promptly when the
// subscriber disconnects. Otherwise this falls back to the original context-less Replay; the
// handler's background drain (see Handler) still ensures such a producer eventually unblocks.
func replay(repo Repository, sub *subscription) <-chan Event {
	if repoCtx, ok := repo.(RepositoryWithContext); ok {
		return repoCtx.ReplayWithContext(sub.ctx, sub.channel, sub.lastEventID)
	}
	return repo.Replay(sub.channel, sub.lastEventID)
}

// enqueueReplay hands a newly registered subscription its replay batch, when
// the channel has a Repository and the request calls for a replay. Runs on the
// Server.run() goroutine; subs is run()'s subscription map.
func (srv *Server) enqueueReplay(
	subs map[string]map[*subscription]struct{},
	repos map[string]Repository,
	sub *subscription,
) {
	if !srv.ReplayAll && len(sub.lastEventID) == 0 {
		return
	}
	repo, ok := repos[sub.channel]
	if !ok {
		return
	}
	batchCh := replay(repo, sub)
	if batchCh == nil {
		return
	}
	if sub.send(eventBatch{events: batchCh}) {
		// Remember the batch so that if the subscriber goes away before its
		// handler dequeues it, the unsubs path can still drain it.
		sub.batch = batchCh
	} else {
		// The send failed because the subscription's buffer was full (send
		// closes the subscription in that case). The batch will never be
		// consumed and its producer would otherwise block forever; drain it
		// in the background. This is a drop like any other, so it reports
		// SubscriberDropped just as trySend does.
		delete(subs[sub.channel], sub)
		srv.traceSubscriberDropped(sub)
		go drainReplayedEvents(batchCh)
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
			srv.traceSubscriberDropped(sub)
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
					s.closeReason = ReasonUnregistered
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
				// The handler has exited. If it never dequeued the replay batch from its
				// event channel -- or exited partway through consuming it -- the
				// Repository's producer may still be blocked sending on it; drain it so
				// the producer can finish. Draining concurrently with the handler's own
				// exit-time drain is safe: both simply receive until the channel is
				// closed. This unsubscription arrives only after the handler's teardown
				// has finished its exit-time reporting, so the drain cannot race the
				// teardown's completion probe.
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
			srv.enqueueReplay(subs, repos, sub)
		case <-srv.quit:
			// We are about to stop processing unsubscriptions, so first handle any that are
			// already queued: their handlers have exited, and a handler that swept its event
			// channel before the replay batch was enqueued is relying on this path to drain it.
			// Subscriptions handled here are deliberately not removed from the map -- the loop
			// below revisits them, recording a close reason and closing their channel. That is
			// harmless: their handlers have already exited and read neither again.
		DrainUnsubs:
			for {
				select {
				case sub := <-srv.unsubs:
					if sub.batch != nil {
						go drainReplayedEvents(sub.batch)
					}
					sub.batch = nil
				default:
					break DrainUnsubs
				}
			}
			for _, sub := range subs {
				for s := range sub {
					s.closeReason = ReasonServerClosed
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
		s.closeReason = ReasonBufferOverflow
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
