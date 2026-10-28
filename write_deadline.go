package eventsource

import (
	"errors"
	"time"
)

// WriteDeadlinePolicy decides how long the Server gives a subscriber to accept one unit of
// output. Set it on Server.WriteDeadline to enable write deadlines.
//
// A unit is one of these:
//   - the flush of the response headers;
//   - one event or comment and its flush;
//   - one replayed event;
//   - the flush at the end of a replay batch.
//
// Deadline returns the write deadline for a unit that started at start and has passed written
// bytes to the connection, including the write that is about to happen. written counts the
// bytes after gzip compression. The Server calls Deadline with written zero when the unit
// starts, and again before each write in the unit. If the first call returns the zero time,
// the unit has no deadline and the Server does not call Deadline again for that unit.
//
// The Server sets the connection's write deadline from the result and clears it when the unit
// is complete. It never moves the deadline earlier within a unit. It can set the deadline up
// to 100ms later than Deadline returns, so that a large unit does not update it on every write.
//
// The Server calls Deadline on the subscriber's handler goroutine. Implementations must be safe
// for concurrent use, because each subscriber has its own handler goroutine.
type WriteDeadlinePolicy interface {
	Deadline(start time.Time, written int) time.Time
}

// ThroughputDeadline is a WriteDeadlinePolicy that requires a minimum throughput. The deadline
// for a unit is start + written/minBytesPerSecond + slack, so the client must accept the unit's
// bytes at the floor rate on average. The slack lets a client stall briefly, and it is the full
// allowance for a unit with few bytes. If maxWriteTime is not zero, no deadline is more than
// maxWriteTime after the unit started.
//
// Create a ThroughputDeadline with NewThroughputDeadline.
type ThroughputDeadline struct {
	minBytesPerSecond int
	slack             time.Duration
	maxWriteTime      time.Duration
}

// NewThroughputDeadline returns a ThroughputDeadline. minBytesPerSecond and slack must be
// positive. maxWriteTime must be zero, which disables the cap, or at least slack, because a
// smaller cap expires before even a small write can finish.
func NewThroughputDeadline(minBytesPerSecond int, slack, maxWriteTime time.Duration) (*ThroughputDeadline, error) {
	if minBytesPerSecond <= 0 {
		return nil, errors.New("eventsource: minBytesPerSecond must be positive")
	}
	if slack <= 0 {
		return nil, errors.New("eventsource: slack must be positive")
	}
	if maxWriteTime != 0 && maxWriteTime < slack {
		return nil, errors.New("eventsource: maxWriteTime must be zero or at least slack")
	}
	return &ThroughputDeadline{
		minBytesPerSecond: minBytesPerSecond,
		slack:             slack,
		maxWriteTime:      maxWriteTime,
	}, nil
}

// Deadline implements WriteDeadlinePolicy.
func (t *ThroughputDeadline) Deadline(start time.Time, written int) time.Time {
	// The whole seconds and the remainder are converted separately, so a large count cannot
	// overflow time.Duration.
	rate := time.Duration(t.minBytesPerSecond)
	n := time.Duration(written)
	budget := (n/rate)*time.Second + (n%rate)*time.Second/rate + t.slack
	if t.maxWriteTime > 0 && budget > t.maxWriteTime {
		budget = t.maxWriteTime
	}
	return start.Add(budget)
}
