package eventsource

import (
	"errors"
	"fmt"
	"net/http"
	"time"
)

// writeDeadline arms Server.WriteTimeout around one unit of output and clears it afterwards,
// so a stream that is idle or waiting on a Repository never carries an armed deadline (on
// HTTP/2 an expired write deadline resets the stream even with no write in flight).
type writeDeadline struct {
	rc          *http.ResponseController
	timeout     time.Duration
	unsupported bool
	armed       bool
}

func newWriteDeadline(w http.ResponseWriter, timeout time.Duration) *writeDeadline {
	return &writeDeadline{rc: http.NewResponseController(w), timeout: timeout}
}

// arm fails only when a ResponseWriter that supports deadlines refuses one, since carrying
// on would leave the write unbounded.
func (d *writeDeadline) arm() error {
	if d.timeout <= 0 || d.unsupported {
		return nil
	}
	if err := d.rc.SetWriteDeadline(time.Now().Add(d.timeout)); err != nil {
		if errors.Is(err, http.ErrNotSupported) {
			d.unsupported = true
			return nil
		}
		return fmt.Errorf("eventsource: set write deadline: %w", err)
	}
	d.armed = true
	return nil
}

// clear fails when a stale deadline would otherwise stay armed on an idle stream.
func (d *writeDeadline) clear() error {
	if !d.armed {
		return nil
	}
	d.armed = false
	if err := d.rc.SetWriteDeadline(time.Time{}); err != nil {
		return fmt.Errorf("eventsource: clear write deadline: %w", err)
	}
	return nil
}

// flush reports the errors that http.Flusher discards.
func (d *writeDeadline) flush() error {
	if err := d.rc.Flush(); err != nil {
		return fmt.Errorf("eventsource flush: %w", err)
	}
	return nil
}
