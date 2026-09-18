package eventsource

import (
	"errors"
	"io"
	"net/http"
	"time"
)

// deadlineWriter arms Server.WriteTimeout only for the duration of each Write and Flush call,
// so a stream that is idle or waiting on a Repository never carries an armed deadline (on
// HTTP/2 an expired write deadline resets the stream even with no write in flight).
type deadlineWriter struct {
	w           io.Writer
	flusher     http.Flusher
	rc          *http.ResponseController
	timeout     time.Duration
	unsupported bool
}

func newDeadlineWriter(w http.ResponseWriter, timeout time.Duration) *deadlineWriter {
	return &deadlineWriter{w: w, flusher: w.(http.Flusher), rc: http.NewResponseController(w), timeout: timeout}
}

func (d *deadlineWriter) arm() bool {
	if d.timeout <= 0 || d.unsupported {
		return false
	}
	if err := d.rc.SetWriteDeadline(time.Now().Add(d.timeout)); err != nil {
		d.unsupported = errors.Is(err, http.ErrNotSupported)
		return false
	}
	return true
}

// The deadline is left armed on error so net/http's trailing writes fail fast too.
func (d *deadlineWriter) Write(p []byte) (int, error) {
	armed := d.arm()
	n, err := d.w.Write(p)
	if armed && err == nil {
		_ = d.rc.SetWriteDeadline(time.Time{})
	}
	return n, err
}

// Flush reports a flush error only while a deadline is armed; http.Flusher discards it.
func (d *deadlineWriter) Flush() error {
	if !d.arm() {
		d.flusher.Flush()
		return nil
	}
	if err := d.rc.Flush(); err != nil {
		return err
	}
	_ = d.rc.SetWriteDeadline(time.Time{})
	return nil
}
