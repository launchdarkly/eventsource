package eventsource

import (
	"context"
	"runtime/debug"
	"time"
)

// ServerTrace is a set of optional callbacks that a Server invokes at points in
// its lifecycle. It is modeled on net/http/httptrace.ClientTrace: a struct of
// optional function fields, where new fields can be added without breaking
// existing users. Set it on the Server's Trace field before serving requests.
//
// Callbacks are invoked synchronously from internal Server goroutines,
// including the single goroutine that dispatches events to every channel. A
// callback that blocks stalls event delivery for all subscribers -- and a
// blocked SubscriberDropped wedges the dispatch goroutine itself, which also
// blocks Close and any handler still starting up -- so each callback must
// return promptly and must not call back into the Server. A nil ServerTrace,
// or a nil field within it, disables the corresponding hook.
//
// Callbacks must not panic. A panic in SubscriberDropped, which fires on the
// dispatch goroutine, is recovered -- and reported through the Logger when one
// is set -- so that it cannot take down the process; a panic in any other
// callback propagates on the subscriber's handler goroutine like a panic in
// any HTTP handler, ending that connection.
//
// Every callback that fires on a subscriber's connection handler goroutine
// receives that subscriber's request context as its first argument. The
// context is provided for telemetry correlation only -- for example attaching
// a child span or span event to the request span -- and must not be used to
// control cancellation. Being the request's own context, it also reaches
// everything a request context carries, such as net/http's ambient values and
// any consumer middleware values; do not use it to recover data the info
// structs deliberately omit. The context may already be canceled by the time a
// late callback such as SubscriberRemoved runs, because the client closing the
// connection is itself what ends the subscription; that is expected and does
// not prevent creating a span from it.
//
// EXPERIMENTAL: this type and every type it references -- the *Info structs, the
// SubscriberRemovedReason and EventDiscardedReason values, and the Server.Trace
// field itself -- are experimental. Callbacks, fields, and reason values may be
// added, renamed, or removed in any future release, and the points at which
// callbacks fire may change. It is for use by
// LaunchDarkly libraries ONLY. No guarantee is made about backwards
// compatibility or future support, and code outside LaunchDarkly's own libraries
// should not depend on it.
type ServerTrace struct {
	// SubscriberAdded is called once for each new subscriber, after the HTTP
	// response has been started and before the subscription is registered with
	// the Server, so it precedes every other callback for that subscriber.
	SubscriberAdded func(context.Context, SubscriberAddedInfo)

	// SubscriberRemoved is called when a subscriber's connection has ended, for
	// any reason. It is called once for each SubscriberAdded.
	SubscriberRemoved func(context.Context, SubscriberRemovedInfo)

	// SubscriberDropped is called when a subscriber is forcibly disconnected
	// because it could not keep up and fell more than BufferSize events behind.
	// SubscriberRemoved is also called for the same subscriber with reason
	// ReasonBufferOverflow, so a consumer that only needs a disconnect signal
	// can implement SubscriberRemoved alone. (The one exception is a drop that
	// lands after the handler has already finished removing the connection for
	// another cause; the removal that paired with it then carries that earlier
	// reason.)
	//
	// Unlike the other callbacks, this one takes no context: it fires on the
	// Server's dispatch goroutine, which has no association with any single
	// subscriber's request, so no request context is available here.
	SubscriberDropped func(SubscriberDroppedInfo)

	// EventSent is called after an event has been written to a subscriber's
	// connection and the write has been flushed, so a client may observe the
	// event before this callback runs. The info reports the event type, payload
	// size, and the measured write duration, but never the payload itself.
	//
	// EventSent fires only for events that are written and flushed
	// individually. Replayed events are encoded in bulk and not reported
	// individually here -- even though gzip compression or a full response
	// buffer may deliver some of their bytes before the batch ends -- so use
	// ReplayFinished for batch-level replay telemetry.
	EventSent func(context.Context, EventSentInfo)

	// CommentSent is called after a comment has been written to a subscriber's
	// connection.
	CommentSent func(context.Context, CommentSentInfo)

	// EventDiscarded is called when an event is dropped before delivery. This
	// currently happens only on jitter-enabled servers, which coalesce events
	// that arrive while an earlier event is still pending and discard a pending
	// event whose connection ends before its delay elapses. The discarded item
	// may be an event or a comment; both are reported here.
	EventDiscarded func(context.Context, EventDiscardedInfo)

	// WriteError is called when encoding or writing an event to a subscriber's
	// connection fails. The subscriber is removed after this callback.
	WriteError func(context.Context, WriteErrorInfo)

	// ReplayStarted is called when a subscriber begins draining a batch of
	// replayed events provided by a Repository. Every ReplayStarted is followed
	// by exactly one ReplayFinished for the same batch.
	ReplayStarted func(context.Context, ReplayInfo)

	// ReplayFinished is called when a subscriber has finished draining a batch
	// of replayed events, or when the connection ends while the batch is still
	// draining; the info's Aborted field distinguishes the two.
	ReplayFinished func(context.Context, ReplayFinishedInfo)
}

// SubscriberRemovedReason describes why a subscriber's connection ended. The
// set of values may change over time; consult the EXPERIMENTAL note on
// ServerTrace.
type SubscriberRemovedReason string

// Reasons reported through ServerTrace.SubscriberRemoved.
const (
	// ReasonClientClosed means the client closed the connection.
	ReasonClientClosed SubscriberRemovedReason = "client_closed"
	// ReasonMaxConnTime means the connection reached the Server's MaxConnTime.
	ReasonMaxConnTime SubscriberRemovedReason = "max_conn_time"
	// ReasonWriteError means writing to the connection failed.
	ReasonWriteError SubscriberRemovedReason = "write_error"
	// ReasonServerClosed means the Server was closed.
	ReasonServerClosed SubscriberRemovedReason = "server_closed"
	// ReasonUnregistered means the channel was unregistered with forceDisconnect.
	ReasonUnregistered SubscriberRemovedReason = "unregistered"
	// ReasonBufferOverflow means the subscriber fell too far behind and was dropped.
	ReasonBufferOverflow SubscriberRemovedReason = "buffer_overflow"
)

// EventDiscardedReason describes why an event was discarded before delivery.
// The set of values may change over time; consult the EXPERIMENTAL note on
// ServerTrace.
type EventDiscardedReason string

// Reasons reported through ServerTrace.EventDiscarded.
const (
	// DiscardReasonJitterCoalesce means the event was coalesced away by a
	// jitter-enabled server while an earlier event was still pending.
	DiscardReasonJitterCoalesce EventDiscardedReason = "jitter_coalesce"
	// DiscardReasonConnectionEnded means the event was still waiting out its
	// jitter delay when the connection ended.
	DiscardReasonConnectionEnded EventDiscardedReason = "connection_ended"
)

// SubscriberAddedInfo is passed to ServerTrace.SubscriberAdded.
type SubscriberAddedInfo struct {
	// Channel is the channel the subscriber connected to.
	Channel string
	// SubscriberID is an opaque per-connection identifier that can be used to
	// correlate this callback with SubscriberRemoved and SubscriberDropped.
	// Identifiers are unique within one Server only; distinct Servers issue
	// overlapping ids.
	SubscriberID uint64
	// HasLastEventID reports whether the subscriber supplied a Last-Event-ID.
	HasLastEventID bool
}

// SubscriberRemovedInfo is passed to ServerTrace.SubscriberRemoved.
type SubscriberRemovedInfo struct {
	// Channel is the channel the subscriber was connected to.
	Channel string
	// SubscriberID matches the value from the corresponding SubscriberAddedInfo.
	SubscriberID uint64
	// Reason describes why the connection ended. When a Server-initiated close
	// races the handler's own exit, the reasons rank as follows:
	// buffer_overflow outranks everything, since the SubscriberDropped
	// callback that already fired promises a removal with the matching
	// reason; write_error outranks the remaining Server reasons, since the
	// handler has already reported that failure through WriteError; and any
	// Server-recorded reason outranks client_closed and max_conn_time. Reason
	// is empty only if the connection ended because a consumer callback
	// panicked on the handler goroutine, which leaves no recorded cause.
	Reason SubscriberRemovedReason
	// ConnDuration is how long the connection was open.
	ConnDuration time.Duration
}

// SubscriberDroppedInfo is passed to ServerTrace.SubscriberDropped.
type SubscriberDroppedInfo struct {
	// Channel is the channel the subscriber was connected to.
	Channel string
	// SubscriberID matches the value from the corresponding SubscriberAddedInfo.
	SubscriberID uint64
	// BufferSize is the number of events the subscriber was allowed to fall
	// behind before being dropped.
	BufferSize int
}

// EventSentInfo is passed to ServerTrace.EventSent.
type EventSentInfo struct {
	// Channel is the channel the event was sent on.
	Channel string
	// EventType is the event's type name, which may be empty.
	EventType string
	// DataSize is the size in bytes of the event's data payload only. It does
	// not include the id/event field values or SSE framing, and it measures
	// the payload before any compression, so it does not correspond exactly to
	// the bytes on the wire.
	DataSize int
	// WriteDuration is the wall time spent encoding and flushing this event to
	// the connection. It is measured only when the EventSent callback is set,
	// so a consumer can back-date a span start to WriteDuration before this
	// callback fires.
	WriteDuration time.Duration
}

// CommentSentInfo is passed to ServerTrace.CommentSent.
type CommentSentInfo struct {
	// Channel is the channel the comment was sent on.
	Channel string
	// WriteDuration is the wall time spent encoding and flushing this comment to
	// the connection. It is measured only when the CommentSent callback is set,
	// so a consumer can back-date a span start to WriteDuration before this
	// callback fires.
	WriteDuration time.Duration
}

// EventDiscardedInfo is passed to ServerTrace.EventDiscarded.
type EventDiscardedInfo struct {
	// Channel is the channel the event would have been sent on.
	Channel string
	// Reason describes why the event was discarded.
	Reason EventDiscardedReason
}

// WriteErrorInfo is passed to ServerTrace.WriteError.
type WriteErrorInfo struct {
	// Channel is the channel the write was attempted on.
	Channel string
	// Err is the error returned by encoding or writing the event.
	Err error
}

// ReplayInfo is passed to ServerTrace.ReplayStarted.
type ReplayInfo struct {
	// Channel is the channel being replayed.
	Channel string
}

// ReplayFinishedInfo is passed to ServerTrace.ReplayFinished.
type ReplayFinishedInfo struct {
	// Channel is the channel that was replayed.
	Channel string
	// EventCount is the number of replayed events written to the connection.
	// For a completed batch they have also been flushed, though a flush cannot
	// report failure, so this does not guarantee the client received them; for
	// an aborted batch they may never have been flushed at all (see Aborted).
	EventCount int
	// TotalDataSize is the summed size in bytes of the data payloads of all
	// replayed events in the batch. Like EventSentInfo.DataSize it counts the
	// data payload only -- no id/event field values, no SSE framing, measured
	// before any compression -- so it does not correspond exactly to the bytes
	// on the wire.
	TotalDataSize int64
	// DrainDuration is how long draining the batch took, as observed by the
	// connection handler, including the single flush at the end of the batch.
	// The measurement starts after the ReplayStarted callback returns, so that
	// callback's own cost is not included. A consumer can back-date a span
	// start to DrainDuration before the ReplayFinished callback fires.
	DrainDuration time.Duration
	// Aborted reports that the connection ended -- client disconnect,
	// MaxConnTime, or a write error -- before the end of the batch was
	// observed. (The handler checks the batch once more at exit, so a batch
	// that fully drained just as the connection ended is still reported
	// completed.) EventCount and TotalDataSize then reflect only the events
	// written before the exit, those events may never have been flushed to the
	// connection, and DrainDuration excludes the end-of-batch flush because
	// none occurred.
	//
	// Aborted is best-effort. If Server.Close races a subscriber that is
	// itself disconnecting mid-batch, the shutdown's drain of the abandoned
	// batch can make the batch appear completed even though the drained events
	// never reached the client; treat Aborted as a strong signal, not an exact
	// accounting.
	//
	// Aborted can also only describe what the Server observed. A Repository
	// that itself ends its batch early -- for example one that stops producing
	// when the request context is canceled -- yields a normal, non-aborted
	// ReplayFinished for whatever it produced.
	Aborted bool
}

// observing reports whether anything consumes what the handler measures, so that
// timings are computed only when a Trace callback or the Logger will use them.
func (srv *Server) observing() bool {
	return srv.Trace != nil || srv.Logger != nil
}

// beginSubscription records tracing bookkeeping for a new subscription and
// returns the time the connection was established. The returned time is the
// zero value when nothing is observing the connection; derive durations from
// it with sinceOrZero, which maps that sentinel to a zero duration.
func (srv *Server) beginSubscription(sub *subscription) time.Time {
	if !srv.observing() {
		return time.Time{}
	}
	// The id is assigned whenever anything is observing -- a Logger-only
	// server needs it too, since log lines identify connections by id.
	sub.id = srv.subCounter.Add(1)
	return time.Now()
}

// sinceOrZero is time.Since for measurements that may not have been started:
// it reports zero for the zero time instead of a nonsense duration measured
// from the epoch.
func sinceOrZero(start time.Time) time.Duration {
	if start.IsZero() {
		return 0
	}
	return time.Since(start)
}

// The trace helpers below also feed srv.Logger. Log lines identify
// connections by their opaque subscriber id and never include the channel
// name: consumers may use sensitive values (such as credentials) as channel
// names, and logs must not contain them.

func (srv *Server) traceSubscriberAdded(ctx context.Context, sub *subscription) {
	hasLastEventID := sub.lastEventID != ""
	if t := srv.Trace; t != nil && t.SubscriberAdded != nil {
		t.SubscriberAdded(ctx, SubscriberAddedInfo{
			Channel:        sub.channel,
			SubscriberID:   sub.id,
			HasLastEventID: hasLastEventID,
		})
	}
	if srv.Logger != nil {
		srv.Logger.Printf("[DEBUG] eventsource: subscriber added (id=%d, has_last_event_id=%t)",
			sub.id, hasLastEventID)
	}
}

func (srv *Server) traceSubscriberRemoved(
	ctx context.Context,
	sub *subscription,
	reason SubscriberRemovedReason,
	dur time.Duration,
) {
	if t := srv.Trace; t != nil && t.SubscriberRemoved != nil {
		t.SubscriberRemoved(ctx, SubscriberRemovedInfo{
			Channel:      sub.channel,
			SubscriberID: sub.id,
			Reason:       reason,
			ConnDuration: dur,
		})
	}
	if srv.Logger != nil {
		srv.Logger.Printf("[DEBUG] eventsource: subscriber removed (id=%d, reason=%s, duration=%s)",
			sub.id, reason, dur)
	}
}

// traceSubscriberDropped runs on the Server.run() dispatch goroutine, where an
// unrecovered panic would take down the process rather than a single
// connection, so it contains panics from the consumer-supplied callback and
// logger instead of letting them unwind run().
func (srv *Server) traceSubscriberDropped(sub *subscription) {
	defer func() {
		if r := recover(); r != nil && srv.Logger != nil {
			// The report itself runs consumer code -- the Logger, which may be
			// exactly what just panicked -- and a second panic here would unwind
			// run() after all, so it is swallowed.
			defer func() { _ = recover() }()
			// %T, not %v: the panic value is consumer-controlled and may carry
			// data, such as the info struct with its channel, that must never be
			// logged. The stack identifies the panic site.
			srv.Logger.Printf("[ERROR] eventsource: panic in ServerTrace callback (%T)\n%s", r, debug.Stack())
		}
	}()
	if t := srv.Trace; t != nil && t.SubscriberDropped != nil {
		t.SubscriberDropped(SubscriberDroppedInfo{
			Channel:      sub.channel,
			SubscriberID: sub.id,
			BufferSize:   srv.BufferSize,
		})
	}
	if srv.Logger != nil {
		srv.Logger.Printf("[WARN] eventsource: dropped subscriber (id=%d, fell behind buffer size %d)",
			sub.id, srv.BufferSize)
	}
}

// shouldMeasureWrite reports whether the trace callback that would receive this
// event or comment is set, so the handler measures write duration only when a
// consumer will actually read it. This preserves the zero-overhead nil path:
// no clock is read when the corresponding callback is nil.
func (srv *Server) shouldMeasureWrite(ec eventOrComment) bool {
	t := srv.Trace
	if t == nil {
		return false
	}
	switch ec.(type) {
	case Event:
		return t.EventSent != nil
	case comment:
		return t.CommentSent != nil
	default:
		return false
	}
}

// writeTraced performs a write via write, then reports it through the EventSent or
// CommentSent callback along with the wall time the write took. The clock is read
// only when that callback is set, so the nil path stays allocation- and
// syscall-free. It returns whether the write succeeded.
func (srv *Server) writeTraced(
	ctx context.Context,
	channel string,
	ec eventOrComment,
	write func() bool,
) bool {
	measure := srv.shouldMeasureWrite(ec)
	var writeStart time.Time
	if measure {
		writeStart = time.Now()
	}
	if !write() {
		return false
	}
	var writeDuration time.Duration
	if measure {
		writeDuration = time.Since(writeStart)
	}
	srv.traceSentEventOrComment(ctx, channel, ec, writeDuration)
	return true
}

func (srv *Server) traceSentEventOrComment(
	ctx context.Context,
	channel string,
	ec eventOrComment,
	writeDuration time.Duration,
) {
	t := srv.Trace
	if t == nil {
		return
	}
	switch item := ec.(type) {
	case Event:
		if t.EventSent != nil {
			t.EventSent(ctx, EventSentInfo{
				Channel:       channel,
				EventType:     item.Event(),
				DataSize:      len(item.Data()),
				WriteDuration: writeDuration,
			})
		}
	case comment:
		if t.CommentSent != nil {
			t.CommentSent(ctx, CommentSentInfo{Channel: channel, WriteDuration: writeDuration})
		}
	}
}

func (srv *Server) traceEventDiscarded(ctx context.Context, channel string, reason EventDiscardedReason) {
	if t := srv.Trace; t != nil && t.EventDiscarded != nil {
		t.EventDiscarded(ctx, EventDiscardedInfo{
			Channel: channel,
			Reason:  reason,
		})
	}
}

func (srv *Server) traceWriteError(ctx context.Context, channel string, err error) {
	if t := srv.Trace; t != nil && t.WriteError != nil {
		t.WriteError(ctx, WriteErrorInfo{Channel: channel, Err: err})
	}
}

func (srv *Server) traceReplayStarted(ctx context.Context, channel string) {
	if t := srv.Trace; t != nil && t.ReplayStarted != nil {
		t.ReplayStarted(ctx, ReplayInfo{Channel: channel})
	}
}

func (srv *Server) traceReplayFinished(
	ctx context.Context,
	sub *subscription,
	count int,
	totalBytes int64,
	dur time.Duration,
	aborted bool,
) {
	if t := srv.Trace; t != nil && t.ReplayFinished != nil {
		t.ReplayFinished(ctx, ReplayFinishedInfo{
			Channel:       sub.channel,
			EventCount:    count,
			TotalDataSize: totalBytes,
			DrainDuration: dur,
			Aborted:       aborted,
		})
	}
	if srv.Logger != nil {
		// The duration logs in Go's own formatting (like the removed line)
		// rather than integer milliseconds, which rounded the common
		// sub-millisecond drain down to 0.
		srv.Logger.Printf(
			"[DEBUG] eventsource: replay drained (id=%d, event_count=%d, total_bytes=%d, duration=%s, aborted=%t)",
			sub.id, count, totalBytes, dur, aborted)
	}
}
