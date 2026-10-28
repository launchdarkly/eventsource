package eventsource

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// deadlineGranularity is how much later than the policy's deadline the Server can set the
// connection's deadline. A unit's deadline is updated only when the policy extends it past the
// current one, so this limits how often a large unit calls SetWriteDeadline.
const deadlineGranularity = 100 * time.Millisecond

// unitWriter writes a handler's output one unit at a time. See WriteDeadlinePolicy for what a
// unit is. Only the handler goroutine uses a unitWriter, because the connection can be reused
// or gone after the handler returns.
type unitWriter interface {
	// write encodes ec, if it is not nil, and then flushes, if flush is true.
	write(ec eventOrComment, flush bool) error
	// exit runs after the rest of the handler's teardown.
	exit()
}

func newUnitWriter(srv *Server, sub *subscription, w http.ResponseWriter, req *http.Request, useGzip bool) unitWriter {
	if srv.WriteDeadline == nil {
		return &plainUnitWriter{enc: NewEncoder(w, useGzip), flusher: w.(http.Flusher)}
	}
	dw := &deadlineUnitWriter{
		w:      w,
		rc:     http.NewResponseController(w),
		policy: srv.WriteDeadline,
		http1:  req.ProtoMajor == 1,
		warnUnsupported: func() {
			if srv.Logger != nil {
				srv.Logger.Printf("[WARN] eventsource: write deadlines are not supported on this connection (id=%d)", sub.id)
			}
		},
	}
	// The Encoder writes through dw, so dw counts the bytes after gzip compression.
	dw.enc = NewEncoder(dw, useGzip)
	return dw
}

// plainUnitWriter is the unitWriter for a Server without a WriteDeadline policy. It keeps the
// behavior from before WriteDeadline existed: flush errors are ignored.
type plainUnitWriter struct {
	enc     *Encoder
	flusher http.Flusher
}

func (p *plainUnitWriter) write(ec eventOrComment, flush bool) error {
	if ec != nil {
		if err := p.enc.Encode(ec); err != nil {
			return err
		}
	}
	if flush {
		p.flusher.Flush()
	}
	return nil
}

func (p *plainUnitWriter) exit() {}

// deadlineUnitWriter is the unitWriter for a Server with a WriteDeadline policy. It is also the
// io.Writer under the Encoder, so it sees each write before it reaches the connection.
type deadlineUnitWriter struct {
	w               http.ResponseWriter
	rc              *http.ResponseController
	enc             *Encoder
	policy          WriteDeadlinePolicy
	http1           bool
	warnUnsupported func()

	// unsupported records that the ResponseWriter cannot set a write deadline, so the writer
	// stops trying.
	unsupported bool
	// armed records that the current unit has a deadline.
	armed    bool
	start    time.Time
	written  int
	deadline time.Time
}

func (d *deadlineUnitWriter) write(ec eventOrComment, flush bool) error {
	if ev, ok := ec.(Event); ok {
		// The fields are read before the unit starts, so a lazily computed Data() costs the
		// client none of its time.
		ec = &publication{id: ev.Id(), event: ev.Event(), data: ev.Data()}
	}
	if err := d.begin(); err != nil {
		return err
	}
	if ec != nil {
		if err := d.enc.Encode(ec); err != nil {
			return err
		}
	}
	if flush {
		if err := d.rc.Flush(); err != nil {
			return fmt.Errorf("eventsource flush: %w", err)
		}
	}
	if d.armed {
		d.armed = false
		// A deadline that stays set would end an idle stream, so a failure to clear it is
		// a write error.
		if err := d.rc.SetWriteDeadline(time.Time{}); err != nil {
			return fmt.Errorf("eventsource clear write deadline: %w", err)
		}
	}
	return nil
}

// exit bounds the final flush that net/http does after the handler returns. On HTTP/1,
// net/http clears the deadline after that flush. HTTP/2 is skipped: a deadline on a stream that
// the client already closed can later send the client a stray stream reset.
func (d *deadlineUnitWriter) exit() {
	if d.http1 {
		_ = d.begin()
	}
}

// begin starts a unit and sets the deadline that the policy gives for zero bytes written.
func (d *deadlineUnitWriter) begin() error {
	d.start = time.Now()
	d.written = 0
	d.armed = false
	if d.unsupported {
		return nil
	}
	dl := d.policy.Deadline(d.start, 0)
	if dl.IsZero() {
		return nil
	}
	if err := d.setDeadline(dl); err != nil {
		if errors.Is(err, http.ErrNotSupported) {
			d.unsupported = true
			d.warnUnsupported()
			return nil
		}
		return err
	}
	d.armed = true
	return nil
}

// extend counts n more bytes for the current unit before they are written, and moves the
// deadline later if the policy gives more time for them.
func (d *deadlineUnitWriter) extend(n int) error {
	if !d.armed {
		return nil
	}
	d.written += n
	if dl := d.policy.Deadline(d.start, d.written); dl.After(d.deadline) {
		return d.setDeadline(dl)
	}
	return nil
}

func (d *deadlineUnitWriter) setDeadline(dl time.Time) error {
	dl = dl.Add(deadlineGranularity)
	if err := d.rc.SetWriteDeadline(dl); err != nil {
		return fmt.Errorf("eventsource set write deadline: %w", err)
	}
	d.deadline = dl
	return nil
}

// Write implements io.Writer for the Encoder.
func (d *deadlineUnitWriter) Write(p []byte) (int, error) {
	if err := d.extend(len(p)); err != nil {
		return 0, err
	}
	return d.w.Write(p)
}

// WriteString implements io.StringWriter, so the Encoder's io.WriteString calls do not copy
// each string into a new []byte.
func (d *deadlineUnitWriter) WriteString(s string) (int, error) {
	if err := d.extend(len(s)); err != nil {
		return 0, err
	}
	return io.WriteString(d.w, s)
}
