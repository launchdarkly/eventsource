package eventsource

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const writeDeadlineTestTimeout = 5 * time.Second

func TestNewThroughputDeadlineValidatesArguments(t *testing.T) {
	cases := []struct {
		name         string
		rate         int
		slack        time.Duration
		maxWriteTime time.Duration
		valid        bool
	}{
		{"no cap", 1024, time.Second, 0, true},
		{"cap equal to slack", 1024, time.Second, time.Second, true},
		{"cap above slack", 1024, time.Second, time.Minute, true},
		{"zero rate", 0, time.Second, 0, false},
		{"negative rate", -1, time.Second, 0, false},
		{"zero slack", 1024, 0, 0, false},
		{"negative slack", 1024, -time.Second, 0, false},
		{"cap below slack", 1024, time.Second, time.Second - 1, false},
		{"negative cap", 1024, time.Second, -time.Second, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			td, err := NewThroughputDeadline(c.rate, c.slack, c.maxWriteTime)
			if c.valid {
				require.NoError(t, err)
				assert.NotNil(t, td)
			} else {
				assert.Error(t, err)
				assert.Nil(t, td)
			}
		})
	}
}

func TestThroughputDeadline(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name         string
		maxWriteTime time.Duration
		size         int
		want         time.Duration
	}{
		{"zero size gets only the slack", 0, 0, time.Second},
		{"size adds time at the floor rate", 0, 2500, 3500 * time.Millisecond},
		{"cap limits a large unit", 2 * time.Second, 2500, 2 * time.Second},
		{"cap does not extend a small unit", 2 * time.Second, 500, 1500 * time.Millisecond},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			td, err := NewThroughputDeadline(1000, time.Second, c.maxWriteTime)
			require.NoError(t, err)
			assert.Equal(t, now.Add(c.want), td.Deadline(now, c.size))
		})
	}

	t.Run("largest size does not overflow", func(t *testing.T) {
		td, err := NewThroughputDeadline(math.MaxInt32, time.Second, 0)
		require.NoError(t, err)
		assert.True(t, td.Deadline(now, math.MaxInt32).After(now))
	})
}

// policyCall is one call to a recordingPolicy.
type policyCall struct {
	start   time.Time
	written int
}

// recordingPolicy returns start + d + written*perByte, or the zero time if d is zero, and
// records each call.
type recordingPolicy struct {
	d       time.Duration
	perByte time.Duration
	mu      sync.Mutex
	calls   []policyCall
}

func (p *recordingPolicy) Deadline(start time.Time, written int) time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, policyCall{start, written})
	if p.d == 0 {
		return time.Time{}
	}
	return start.Add(p.d + time.Duration(written)*p.perByte)
}

func (p *recordingPolicy) snapshotCalls() []policyCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]policyCall(nil), p.calls...)
}

// unitTotals returns the last written count of each unit, in order. A unit's calls share a start
// time and begin with a written count of zero.
func unitTotals(calls []policyCall) []int {
	var totals []int
	for _, c := range calls {
		if c.written == 0 {
			totals = append(totals, 0)
			continue
		}
		totals[len(totals)-1] = c.written
	}
	return totals
}

// deadlineTestWriter adds SetWriteDeadline and FlushError to traceTestWriter, so that
// http.ResponseController can use it. It records each deadline that the handler sets.
//
// If perByte is set, Write takes perByte for each byte, like a client that reads at a steady
// rate, and then fails with os.ErrDeadlineExceeded if the last deadline set has passed.
type deadlineTestWriter struct {
	traceTestWriter
	perByte   time.Duration
	dmu       sync.Mutex
	deadlines []time.Time
	setErr    error
	flushErr  atomic.Pointer[error]
}

func (w *deadlineTestWriter) Write(p []byte) (int, error) {
	if w.perByte > 0 {
		time.Sleep(time.Duration(len(p)) * w.perByte)
		w.dmu.Lock()
		var dl time.Time
		if len(w.deadlines) > 0 {
			dl = w.deadlines[len(w.deadlines)-1]
		}
		w.dmu.Unlock()
		if !dl.IsZero() && time.Now().After(dl) {
			return 0, os.ErrDeadlineExceeded
		}
	}
	return w.traceTestWriter.Write(p)
}

func (w *deadlineTestWriter) SetWriteDeadline(t time.Time) error {
	w.dmu.Lock()
	defer w.dmu.Unlock()
	w.deadlines = append(w.deadlines, t)
	return w.setErr
}

func (w *deadlineTestWriter) FlushError() error {
	if err := w.flushErr.Load(); err != nil {
		return *err
	}
	return nil
}

func (w *deadlineTestWriter) snapshotDeadlines() []time.Time {
	w.dmu.Lock()
	defer w.dmu.Unlock()
	return append([]time.Time(nil), w.deadlines...)
}

// registrationSignal is a Repository with nothing to replay. With ReplayAll set, the Server
// asks it for a replay only after it registers a subscriber, so the call shows that
// published events will reach the subscriber.
type registrationSignal struct {
	once sync.Once
	ch   chan struct{}
}

func newRegistrationSignal() *registrationSignal {
	return &registrationSignal{ch: make(chan struct{})}
}

func (s *registrationSignal) Replay(channel, id string) chan Event {
	s.once.Do(func() { close(s.ch) })
	out := make(chan Event)
	close(out)
	return out
}

func (s *registrationSignal) wait(t *testing.T) {
	t.Helper()
	select {
	case <-s.ch:
	case <-time.After(writeDeadlineTestTimeout):
		require.Fail(t, "timed out waiting for the subscriber to be registered")
	}
}

// countingEvent counts the calls to Data, to show that the Server reads a lazily computed
// Data only once per write.
type countingEvent struct {
	data  string
	calls atomic.Int32
}

func (e *countingEvent) Id() string    { return "1" } //nolint:revive // matches the Event interface
func (e *countingEvent) Event() string { return "put" }
func (e *countingEvent) Data() string {
	e.calls.Add(1)
	return e.data
}

func newDeadlineServer(policy WriteDeadlinePolicy) (*Server, *registrationSignal) {
	server := NewServer()
	server.WriteDeadline = policy
	server.ReplayAll = true
	signal := newRegistrationSignal()
	server.Register("test", signal)
	return server, signal
}

// startDeadlineHandler runs the handler with a request of the given HTTP major version.
func startDeadlineHandler(
	server *Server,
	w http.ResponseWriter,
	protoMajor int,
	headers map[string]string,
) (context.CancelFunc, <-chan struct{}) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.ProtoMajor = protoMajor
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.Handler("test")(w, req)
	}()
	return cancel, done
}

func TestWriteDeadlineIsSetAndClearedForEachUnit(t *testing.T) {
	policy := &recordingPolicy{d: time.Minute}
	server, signal := newDeadlineServer(policy)
	defer server.Close()
	w := &deadlineTestWriter{}
	cancel, done := startDeadlineHandler(server, w, 1, nil)
	defer func() {
		cancel()
		waitClosed(t, done)
	}()
	signal.wait(t)

	ev := &countingEvent{data: "line one\nline two"}
	server.Publish([]string{"test"}, ev)
	server.PublishComment([]string{"test"}, "keepalive")
	require.Eventually(t, func() bool { return w.contains(":keepalive\n") },
		writeDeadlineTestTimeout, 10*time.Millisecond)

	// The units are the header flush, the end of the empty replay batch, the event, and the
	// comment.
	event := "id: 1\nevent: put\ndata: line one\ndata: line two\n\n"
	assert.Equal(t, []int{0, 0, len(event), len(":keepalive\n")}, unitTotals(policy.snapshotCalls()))
	assert.Equal(t, int32(1), ev.calls.Load())

	// The policy gives the same deadline for any number of bytes, so each unit sets it once.
	deadlines := w.snapshotDeadlines()
	require.Len(t, deadlines, 8)
	for i := 0; i < len(deadlines); i += 2 {
		assert.False(t, deadlines[i].IsZero(), "unit %d did not set a deadline", i/2)
		assert.True(t, deadlines[i+1].IsZero(), "unit %d did not clear its deadline", i/2)
	}
}

func TestWriteDeadlineCountsBytesWritten(t *testing.T) {
	cases := []struct {
		name    string
		gzip    bool
		headers map[string]string
	}{
		{"uncompressed", false, nil},
		{"gzip", true, map[string]string{"Accept-Encoding": "gzip"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			policy := &recordingPolicy{d: time.Minute}
			server, signal := newDeadlineServer(policy)
			server.Gzip = c.gzip
			defer server.Close()
			w := &deadlineTestWriter{}
			cancel, done := startDeadlineHandler(server, w, 1, c.headers)
			signal.wait(t)

			data := strings.Repeat("compressible ", 10000)
			server.Publish([]string{"test"}, &publication{data: data})
			require.Eventually(t, func() bool { return len(unitTotals(policy.snapshotCalls())) == 3 },
				writeDeadlineTestTimeout, 10*time.Millisecond)
			cancel()
			waitClosed(t, done)

			// Every byte that reached the ResponseWriter was counted in some unit, after any
			// compression.
			var counted int
			for _, total := range unitTotals(policy.snapshotCalls()) {
				counted += total
			}
			assert.Equal(t, w.bytesWritten(), counted)
			if c.gzip {
				assert.Less(t, counted, len(data)/10)
			} else {
				assert.Greater(t, counted, len(data))
			}
		})
	}
}

func TestWriteDeadlineExtendsAsBytesAreWritten(t *testing.T) {
	// Each byte adds a millisecond, so a 1000-byte value moves the deadline by a second.
	policy := &recordingPolicy{d: time.Second, perByte: time.Millisecond}
	server, signal := newDeadlineServer(policy)
	defer server.Close()
	w := &deadlineTestWriter{}
	cancel, done := startDeadlineHandler(server, w, 1, nil)
	defer func() {
		cancel()
		waitClosed(t, done)
	}()
	signal.wait(t)

	server.Publish([]string{"test"}, &publication{data: strings.Repeat("x", 1000)})
	require.Eventually(t, func() bool { return w.contains("\n\n") },
		writeDeadlineTestTimeout, 10*time.Millisecond)

	// The Encoder writes "data: ", the value, and two newlines. The prefix and the newlines
	// each move the deadline by less than the granularity, so only the value extends it. The
	// event's unit is the last one, so its calls end the lists.
	deadlines := w.snapshotDeadlines()
	require.GreaterOrEqual(t, len(deadlines), 3)
	unit := deadlines[len(deadlines)-3:]
	calls := policy.snapshotCalls()
	start := calls[len(calls)-1].start
	assert.Equal(t, start.Add(time.Second+deadlineGranularity), unit[0])
	assert.Equal(t, start.Add(time.Second+1006*time.Millisecond+deadlineGranularity), unit[1])
	assert.True(t, unit[2].IsZero())
}

func TestWriteDeadlineOnHandlerExit(t *testing.T) {
	t.Run("HTTP/1 sets a deadline for the final flush", func(t *testing.T) {
		server, signal := newDeadlineServer(&recordingPolicy{d: time.Minute})
		defer server.Close()
		w := &deadlineTestWriter{}
		cancel, done := startDeadlineHandler(server, w, 1, nil)
		signal.wait(t)
		cancel()
		waitClosed(t, done)

		deadlines := w.snapshotDeadlines()
		require.NotEmpty(t, deadlines)
		assert.False(t, deadlines[len(deadlines)-1].IsZero())
	})

	t.Run("HTTP/2 leaves the deadline clear", func(t *testing.T) {
		server, signal := newDeadlineServer(&recordingPolicy{d: time.Minute})
		defer server.Close()
		w := &deadlineTestWriter{}
		cancel, done := startDeadlineHandler(server, w, 2, nil)
		signal.wait(t)
		cancel()
		waitClosed(t, done)

		deadlines := w.snapshotDeadlines()
		require.NotEmpty(t, deadlines)
		assert.True(t, deadlines[len(deadlines)-1].IsZero())
	})
}

func TestWriteDeadlineZeroTimeSetsNoDeadline(t *testing.T) {
	policy := &recordingPolicy{}
	server, signal := newDeadlineServer(policy)
	defer server.Close()
	w := &deadlineTestWriter{}
	cancel, done := startDeadlineHandler(server, w, 1, nil)
	signal.wait(t)
	server.Publish([]string{"test"}, &publication{data: "hello"})
	require.Eventually(t, func() bool { return w.contains("data: hello\n") },
		writeDeadlineTestTimeout, 10*time.Millisecond)
	cancel()
	waitClosed(t, done)

	// Each unit asks the policy once, at its start.
	calls := policy.snapshotCalls()
	assert.NotEmpty(t, calls)
	for _, c := range calls {
		assert.Zero(t, c.written)
	}
	assert.Empty(t, w.snapshotDeadlines())
}

func TestWriteDeadlineWithoutResponseControllerSupport(t *testing.T) {
	logger := &captureLogger{}
	policy := &recordingPolicy{d: time.Minute}
	server, signal := newDeadlineServer(policy)
	server.Logger = logger
	defer server.Close()
	// traceTestWriter has no SetWriteDeadline, so http.ResponseController reports
	// http.ErrNotSupported.
	w := &traceTestWriter{}
	cancel, done := startDeadlineHandler(server, w, 1, nil)
	defer func() {
		cancel()
		waitClosed(t, done)
	}()
	signal.wait(t)

	server.Publish([]string{"test"}, &publication{data: "hello"})
	require.Eventually(t, func() bool { return w.contains("data: hello\n") },
		writeDeadlineTestTimeout, 10*time.Millisecond)

	logger.mu.Lock()
	defer logger.mu.Unlock()
	var warnings int
	for _, line := range logger.lines {
		if strings.Contains(line, "[WARN] eventsource: write deadlines are not supported") {
			warnings++
		}
	}
	assert.Equal(t, 1, warnings)
	// After the first failure, the handler stops asking the policy.
	assert.Len(t, policy.snapshotCalls(), 1)
}

func TestWriteDeadlineSetFailureEndsHandlerBeforeSubscriberIsAdded(t *testing.T) {
	rec := &traceRecorder{}
	server, _ := newDeadlineServer(&recordingPolicy{d: time.Minute})
	server.Trace = rec.trace()
	defer server.Close()
	w := &deadlineTestWriter{setErr: errors.New("sabotaged")}
	_, done := startDeadlineHandler(server, w, 1, nil)
	waitClosed(t, done)

	writeErrors := rec.snapshotWriteErrors()
	require.Len(t, writeErrors, 1)
	assert.True(t, errors.Is(writeErrors[0].Err, w.setErr), "got %v", writeErrors[0].Err)
	assert.Empty(t, rec.snapshotAdded())
	assert.Empty(t, rec.snapshotRemoved())
}

func TestWriteDeadlineFlushErrorIsWriteError(t *testing.T) {
	flushErr := errors.New("flush failed")

	t.Run("with a policy the connection ends", func(t *testing.T) {
		rec := &traceRecorder{}
		server, signal := newDeadlineServer(&recordingPolicy{d: time.Minute})
		server.Trace = rec.trace()
		defer server.Close()
		w := &deadlineTestWriter{}
		_, done := startDeadlineHandler(server, w, 1, nil)
		signal.wait(t)

		w.flushErr.Store(&flushErr)
		server.Publish([]string{"test"}, &publication{data: "hello"})
		waitClosed(t, done)

		writeErrors := rec.snapshotWriteErrors()
		require.Len(t, writeErrors, 1)
		assert.True(t, errors.Is(writeErrors[0].Err, flushErr), "got %v", writeErrors[0].Err)
		removed := rec.snapshotRemoved()
		require.Len(t, removed, 1)
		assert.Equal(t, ReasonWriteError, removed[0].Reason)
	})

	t.Run("without a policy the error is ignored", func(t *testing.T) {
		rec := &traceRecorder{}
		server, signal := newDeadlineServer(nil)
		server.Trace = rec.trace()
		defer server.Close()
		w := &deadlineTestWriter{}
		w.flushErr.Store(&flushErr)
		cancel, done := startDeadlineHandler(server, w, 1, nil)
		signal.wait(t)

		server.Publish([]string{"test"}, &publication{data: "hello"})
		require.Eventually(t, func() bool { return len(rec.snapshotEventsSent()) == 1 },
			writeDeadlineTestTimeout, 10*time.Millisecond)
		cancel()
		waitClosed(t, done)

		assert.Empty(t, rec.snapshotWriteErrors())
		assert.Empty(t, w.snapshotDeadlines())
	})
}

func TestWriteDeadlineEndsSteadyClientBelowTheFloor(t *testing.T) {
	rec := &traceRecorder{}
	// The slack is much longer than any single write in the test takes.
	policy, err := NewThroughputDeadline(64*1024, 200*time.Millisecond, 0)
	require.NoError(t, err)
	server, signal := newDeadlineServer(policy)
	server.Trace = rec.trace()
	defer server.Close()
	// The client accepts about 32 KiB per second, half the floor, and never stalls.
	w := &deadlineTestWriter{perByte: 30 * time.Microsecond}
	_, done := startDeadlineHandler(server, w, 1, nil)
	signal.wait(t)

	// The event has 256 lines of 1 KiB, so it reaches the connection in many small writes. No
	// single write comes near its own deadline. Only the unit's total shows that the client is
	// below the floor: the event needs about 8 seconds, and the deadline falls behind after
	// less than 1 second.
	line := strings.Repeat("x", 1024)
	lines := make([]string, 256)
	for i := range lines {
		lines[i] = line
	}
	server.Publish([]string{"test"}, &publication{data: strings.Join(lines, "\n")})

	select {
	case <-done:
	case <-time.After(writeDeadlineTestTimeout):
		require.Fail(t, "the client below the floor was not cut")
	}
	writeErrors := rec.snapshotWriteErrors()
	require.Len(t, writeErrors, 1)
	assert.Contains(t, writeErrors[0].Err.Error(), os.ErrDeadlineExceeded.Error())
}

// The tests below use real sockets, because the behavior under test is the interaction between
// the deadline and the connection.

// Larger than the socket buffers, so a client that stops reading blocks the handler.
const stallEventSize = 64 * 1024

const stallEventCount = 200

func newStallPolicy(t *testing.T) WriteDeadlinePolicy {
	t.Helper()
	policy, err := NewThroughputDeadline(stallEventSize, 200*time.Millisecond, 0)
	require.NoError(t, err)
	return policy
}

// newSmallSendBufferServer shrinks each connection's send buffer, so a client that stops
// reading blocks the handler after little output.
func newSmallSendBufferServer(handler http.Handler) *httptest.Server {
	ts := httptest.NewUnstartedServer(handler)
	ts.Config.ConnState = func(c net.Conn, state http.ConnState) {
		if tc, ok := c.(*net.TCPConn); ok && state == http.StateNew {
			_ = tc.SetWriteBuffer(4096)
		}
	}
	ts.Start()
	return ts
}

// stalledSSEConn sends an SSE request, reads the response headers, and then stops reading.
// Reset it with resetConn before httptest.Server.Close, which waits for the handlers.
func stalledSSEConn(t *testing.T, url string, headers ...string) *net.TCPConn {
	t.Helper()
	u, err := neturl.Parse(url)
	require.NoError(t, err)
	c, err := net.Dial("tcp", u.Host)
	require.NoError(t, err)
	conn := c.(*net.TCPConn)
	require.NoError(t, conn.SetReadBuffer(4096))
	var extra string
	for _, h := range headers {
		extra += h + "\r\n"
	}
	_, err = fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: %s\r\nAccept: text/event-stream\r\n%s\r\n", u.Host, extra)
	require.NoError(t, err)

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(writeDeadlineTestTimeout)))
	var head []byte
	buf := make([]byte, 1)
	for !bytes.HasSuffix(head, []byte("\r\n\r\n")) {
		n, err := conn.Read(buf)
		require.NoError(t, err)
		head = append(head, buf[:n]...)
	}
	require.Contains(t, string(head), "200 OK")
	return conn
}

// resetConn sends a TCP reset, which releases a handler that is blocked in a write.
func resetConn(conn *net.TCPConn) {
	_ = conn.SetLinger(0)
	_ = conn.Close()
}

func stallEvents(data string) []Event {
	events := make([]Event, stallEventCount)
	for i := range events {
		events[i] = &publication{id: strconv.Itoa(i), data: data}
	}
	return events
}

// incompressibleData survives gzip, so a compressed stream still fills the socket buffers.
func incompressibleData(t *testing.T) string {
	t.Helper()
	raw := make([]byte, stallEventSize)
	_, err := rand.Read(raw)
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(raw)
}

func waitForRemoval(t *testing.T, rec *traceRecorder) SubscriberRemovedInfo {
	t.Helper()
	require.Eventually(t, func() bool { return len(rec.snapshotRemoved()) == 1 },
		writeDeadlineTestTimeout, 10*time.Millisecond, "the stalled subscriber was not removed")
	return rec.snapshotRemoved()[0]
}

func TestWriteDeadlineEndsStalledHTTP1Stream(t *testing.T) {
	cases := []struct {
		name    string
		gzip    bool
		headers []string
	}{
		{"uncompressed", false, nil},
		{"gzip", true, []string{"Accept-Encoding: gzip"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := &traceRecorder{}
			server, signal := newDeadlineServer(newStallPolicy(t))
			server.Trace = rec.trace()
			server.Gzip = c.gzip
			// The buffer holds every event, so a buffer overflow cannot be what removes the
			// subscriber.
			server.BufferSize = stallEventCount
			defer server.Close()
			ts := newSmallSendBufferServer(server.Handler("test"))
			defer ts.Close()
			conn := stalledSSEConn(t, ts.URL, c.headers...)
			defer resetConn(conn)
			signal.wait(t)

			data := strings.Repeat("x", stallEventSize)
			if c.gzip {
				data = incompressibleData(t)
			}
			for _, ev := range stallEvents(data) {
				server.Publish([]string{"test"}, ev)
			}

			removed := waitForRemoval(t, rec)
			assert.Equal(t, ReasonWriteError, removed.Reason)
			writeErrors := rec.snapshotWriteErrors()
			require.Len(t, writeErrors, 1)
			// The Encoder formats the error with %v, so only the message shows the timeout.
			assert.Contains(t, writeErrors[0].Err.Error(), "i/o timeout")
		})
	}
}

// replayRepository replays a fixed set of events.
type replayRepository struct{ events []Event }

func (r replayRepository) Replay(channel, id string) chan Event {
	out := make(chan Event, len(r.events))
	for _, ev := range r.events {
		out <- ev
	}
	close(out)
	return out
}

func TestWriteDeadlineEndsStalledReplay(t *testing.T) {
	rec := &traceRecorder{}
	server := NewServer()
	server.WriteDeadline = newStallPolicy(t)
	server.Trace = rec.trace()
	server.ReplayAll = true
	server.Register("test", replayRepository{events: stallEvents(strings.Repeat("x", stallEventSize))})
	defer server.Close()
	ts := newSmallSendBufferServer(server.Handler("test"))
	defer ts.Close()
	conn := stalledSSEConn(t, ts.URL)
	defer resetConn(conn)

	removed := waitForRemoval(t, rec)
	assert.Equal(t, ReasonWriteError, removed.Reason)
	finished := rec.snapshotReplayFinished()
	require.Len(t, finished, 1)
	assert.True(t, finished[0].Aborted)
	assert.Less(t, finished[0].EventCount, stallEventCount)
}

// newHTTP2Server starts a TLS server that negotiates HTTP/2.
func newHTTP2Server(handler http.Handler) *httptest.Server {
	ts := httptest.NewUnstartedServer(handler)
	ts.EnableHTTP2 = true
	ts.StartTLS()
	return ts
}

// openStream opens a stream. The caller must close the body before httptest.Server.Close,
// which waits for the handler.
func openStream(t *testing.T, ts *httptest.Server) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, ts.URL, nil)
	require.NoError(t, err)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	return resp
}

func TestWriteDeadlineEndsStalledHTTP2Stream(t *testing.T) {
	rec := &traceRecorder{}
	server, signal := newDeadlineServer(newStallPolicy(t))
	server.Trace = rec.trace()
	server.BufferSize = stallEventCount
	defer server.Close()
	ts := newHTTP2Server(server.Handler("test"))
	defer ts.Close()
	resp := openStream(t, ts)
	defer resp.Body.Close()
	require.Equal(t, 2, resp.ProtoMajor)
	signal.wait(t)

	// The events exceed the client's per-stream flow-control window, and the test never
	// reads the body, so the handler blocks.
	for _, ev := range stallEvents(strings.Repeat("x", stallEventSize)) {
		server.Publish([]string{"test"}, ev)
	}

	removed := waitForRemoval(t, rec)
	assert.Equal(t, ReasonWriteError, removed.Reason)
}

func TestWriteDeadlineKeepsIdleStreamOpen(t *testing.T) {
	const slack = 100 * time.Millisecond
	cases := []struct {
		name      string
		newServer func(http.Handler) *httptest.Server
		major     int
	}{
		{"HTTP/1", httptest.NewServer, 1},
		{"HTTP/2", newHTTP2Server, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := &traceRecorder{}
			policy, err := NewThroughputDeadline(1024, slack, 0)
			require.NoError(t, err)
			server, signal := newDeadlineServer(policy)
			server.Trace = rec.trace()
			defer server.Close()
			ts := c.newServer(server.Handler("test"))
			defer ts.Close()
			resp := openStream(t, ts)
			defer resp.Body.Close()
			require.Equal(t, c.major, resp.ProtoMajor)
			signal.wait(t)

			lines := make(chan string)
			done := make(chan struct{})
			defer close(done)
			go func() {
				defer close(lines)
				scanner := bufio.NewScanner(resp.Body)
				for scanner.Scan() {
					select {
					case lines <- scanner.Text():
					case <-done:
						return
					}
				}
			}()
			waitForLine := func(want string) {
				t.Helper()
				timeout := time.After(writeDeadlineTestTimeout)
				for {
					select {
					case line, ok := <-lines:
						require.True(t, ok, "stream ended before %q", want)
						if line == want {
							return
						}
					case <-timeout:
						require.Fail(t, "timed out waiting for "+want)
					}
				}
			}

			// Each gap is several times the slack. A deadline left set after a unit would
			// end the stream during the gap.
			for i := 0; i < 3; i++ {
				data := "event-" + strconv.Itoa(i)
				server.Publish([]string{"test"}, &publication{data: data})
				waitForLine("data: " + data)
				time.Sleep(5 * slack)
			}
			server.Publish([]string{"test"}, &publication{data: "last"})
			waitForLine("data: last")

			assert.Empty(t, rec.snapshotWriteErrors())
			assert.Empty(t, rec.snapshotRemoved())
		})
	}
}
