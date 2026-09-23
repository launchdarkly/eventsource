package eventsource

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const writeTimeoutTestDeadline = 2 * time.Second

// Larger than net/http's 2 KB pre-chunking buffer, so each event reaches the socket.
const blockingEventSize = 64 * 1024

const blockingEventCount = 200

// unreadSSEConn sends a valid SSE request, consumes the response headers, then never reads
// again. Callers must resetConn it before httptest.Server.Close, which waits on handlers.
func unreadSSEConn(t *testing.T, url string, headers ...string) *net.TCPConn {
	t.Helper()
	u, err := neturl.Parse(url)
	require.NoError(t, err)
	c, err := net.Dial("tcp", u.Host)
	require.NoError(t, err)
	conn, ok := c.(*net.TCPConn)
	require.True(t, ok)
	require.NoError(t, conn.SetReadBuffer(4096))
	var extra string
	for _, h := range headers {
		extra += h + "\r\n"
	}
	_, err = fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: %s\r\nAccept: text/event-stream\r\n%s\r\n", u.Host, extra)
	require.NoError(t, err)

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(writeTimeoutTestDeadline)))
	var head []byte
	buf := make([]byte, 1)
	for !bytes.HasSuffix(head, []byte("\r\n\r\n")) {
		n, err := conn.Read(buf)
		require.NoError(t, err)
		head = append(head, buf[:n]...)
	}
	require.Contains(t, string(head), "200 OK")
	require.NoError(t, conn.SetReadDeadline(time.Time{}))
	return conn
}

// resetConn sends RST so a handler parked in a write to this connection is released.
func resetConn(t *testing.T, conn *net.TCPConn) {
	t.Helper()
	_ = conn.SetLinger(0)
	_ = conn.Close()
}

func publishBlockingEvents(server *Server, channel string) {
	publishEvents(server, channel, strings.Repeat("x", blockingEventSize))
}

// Random bytes survive gzip, so a compressed stream still fills the socket buffers.
func publishIncompressibleEvents(t *testing.T, server *Server, channel string) {
	raw := make([]byte, blockingEventSize)
	_, err := rand.Read(raw)
	require.NoError(t, err)
	publishEvents(server, channel, base64.StdEncoding.EncodeToString(raw))
}

func publishEvents(server *Server, channel, data string) {
	for i := 0; i < blockingEventCount; i++ {
		server.Publish([]string{channel}, &publication{id: strconv.Itoa(i), data: data})
	}
}

// BufferSize holds the whole payload so buffer overflow can never be what drops the subscriber.
func newWriteTimeoutServer(rec *traceRecorder, writeTimeout time.Duration) *Server {
	server := NewServer()
	server.BufferSize = blockingEventCount * 2
	server.WriteTimeout = writeTimeout
	server.Trace = rec.trace()
	return server
}

func waitForAtLeast(t *testing.T, what string, want int, count func() int) {
	t.Helper()
	deadline := time.Now().Add(writeTimeoutTestDeadline)
	for {
		if n := count(); n >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s (want at least %d, have %d)",
				writeTimeoutTestDeadline, what, want, count())
		}
		time.Sleep(time.Millisecond)
	}
}

// waitForStall returns the counter's value once it has held still for stallSamples samples,
// long enough that a descheduled handler is not mistaken for a blocked one.
func waitForStall(t *testing.T, what string, count func() int) int {
	t.Helper()
	const sampleInterval = 100 * time.Millisecond
	const stallSamples = 5
	deadline := time.Now().Add(2 * writeTimeoutTestDeadline)
	previous, unchanged := -1, 0
	for {
		time.Sleep(sampleInterval)
		current := count()
		if current == previous {
			unchanged++
			if unchanged == stallSamples {
				return current
			}
		} else {
			unchanged = 0
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s still advancing after %s (%d then %d)", what, 2*writeTimeoutTestDeadline, previous, current)
		}
		previous = current
	}
}

func TestServerWriteTimeoutEndsConnectionBlockedInWrite(t *testing.T) {
	channel := "test"
	rec := &traceRecorder{}
	server := newWriteTimeoutServer(rec, 200*time.Millisecond)
	defer server.Close()
	httpServer := httptest.NewServer(server.Handler(channel))
	defer httpServer.Close()

	conn := unreadSSEConn(t, httpServer.URL)
	defer resetConn(t, conn)

	waitForAtLeast(t, "the subscriber to be added", 1, func() int { return len(rec.snapshotAdded()) })
	publishBlockingEvents(server, channel)

	waitForAtLeast(t, "the handler to exit", 1, func() int { return len(rec.snapshotRemoved()) })
	removed := rec.snapshotRemoved()
	assert.Equal(t, ReasonWriteError, removed[0].Reason)

	writeErrors := rec.snapshotWriteErrors()
	require.Len(t, writeErrors, 1)
	assert.True(t, errors.Is(writeErrors[0].Err, os.ErrDeadlineExceeded),
		"expected a deadline error, got %v", writeErrors[0].Err)
}

func TestServerWithoutWriteTimeoutStaysBlockedInWrite(t *testing.T) {
	channel := "test"
	rec := &traceRecorder{}
	server := newWriteTimeoutServer(rec, 0)
	defer server.Close()
	httpServer := httptest.NewServer(server.Handler(channel))
	defer httpServer.Close()

	conn := unreadSSEConn(t, httpServer.URL)
	defer resetConn(t, conn)

	waitForAtLeast(t, "the subscriber to be added", 1, func() int { return len(rec.snapshotAdded()) })
	publishBlockingEvents(server, channel)
	waitForAtLeast(t, "the first event to be sent", 1, func() int { return len(rec.snapshotEventsSent()) })
	stalled := waitForStall(t, "events sent", func() int { return len(rec.snapshotEventsSent()) })
	assert.Less(t, stalled, blockingEventCount, "the whole payload fit in the socket buffers")

	time.Sleep(writeTimeoutTestDeadline / 2)
	assert.Empty(t, rec.snapshotRemoved(), "handler exited even though no write deadline was set")
	assert.Equal(t, stalled, len(rec.snapshotEventsSent()), "handler resumed writing")
}

func TestServerWriteTimeoutDoesNotAffectClientThatKeepsReading(t *testing.T) {
	channel := "test"
	rec := &traceRecorder{}
	const writeTimeout = 100 * time.Millisecond
	server := newWriteTimeoutServer(rec, writeTimeout)
	defer server.Close()
	httpServer := httptest.NewServer(server.Handler(channel))
	defer httpServer.Close()

	resp, err := http.Get(httpServer.URL)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	reader := newSSEReader(t, resp.Body)

	// Gaps exceed WriteTimeout, so a deadline left armed between writes would end this stream.
	for i := 0; i < 4; i++ {
		time.Sleep(writeTimeout + 50*time.Millisecond)
		want := fmt.Sprintf("event-%d", i)
		server.Publish([]string{channel}, &publication{id: strconv.Itoa(i), data: want})
		reader.waitFor(t, want)
	}
	assert.Empty(t, rec.snapshotWriteErrors())
	assert.Empty(t, rec.snapshotRemoved())
}

// A ResponseWriter that ResponseController cannot unwrap must still work with WriteTimeout set.
func TestServerWriteTimeoutIgnoredWhenResponseWriterHasNoDeadline(t *testing.T) {
	channel := "test"
	rec := &traceRecorder{}
	server := NewServer()
	server.WriteTimeout = 10 * time.Millisecond
	server.Trace = rec.trace()
	defer server.Close()

	writer := &traceTestWriter{delay: 50 * time.Millisecond}
	cancel, done := startTraceHandler(server, channel, writer, nil)
	defer func() {
		cancel()
		waitClosed(t, done)
	}()
	publishUntilSent(t, server, channel, rec, &publication{id: "1", data: "hello"})

	assert.Empty(t, rec.snapshotWriteErrors())
	assert.Empty(t, rec.snapshotRemoved())
	assert.True(t, writer.contains("data: hello"))
}

// ld-relay serves SSE gzipped; the deadline error must survive the gzip writer.
func TestServerWriteTimeoutEndsGzipConnectionBlockedInWrite(t *testing.T) {
	channel := "test"
	rec := &traceRecorder{}
	server := newWriteTimeoutServer(rec, 200*time.Millisecond)
	server.Gzip = true
	defer server.Close()
	httpServer := httptest.NewServer(server.Handler(channel))
	defer httpServer.Close()

	conn := unreadSSEConn(t, httpServer.URL, "Accept-Encoding: gzip")
	defer resetConn(t, conn)

	waitForAtLeast(t, "the subscriber to be added", 1, func() int { return len(rec.snapshotAdded()) })
	publishIncompressibleEvents(t, server, channel)

	waitForAtLeast(t, "the handler to exit", 1, func() int { return len(rec.snapshotRemoved()) })
	assert.Equal(t, ReasonWriteError, rec.snapshotRemoved()[0].Reason)
	writeErrors := rec.snapshotWriteErrors()
	require.Len(t, writeErrors, 1)
	assert.True(t, errors.Is(writeErrors[0].Err, os.ErrDeadlineExceeded),
		"expected a deadline error, got %v", writeErrors[0].Err)
}

func newHTTP2Server(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	httpServer := httptest.NewUnstartedServer(handler)
	httpServer.EnableHTTP2 = true
	httpServer.StartTLS()
	return httpServer
}

// On HTTP/2 an expired deadline resets the stream rather than failing the write with
// os.ErrDeadlineExceeded, so only the exit reason is asserted.
func TestServerWriteTimeoutEndsHTTP2StreamBlockedInWrite(t *testing.T) {
	channel := "test"
	rec := &traceRecorder{}
	server := newWriteTimeoutServer(rec, 200*time.Millisecond)
	defer server.Close()
	httpServer := newHTTP2Server(t, server.Handler(channel))
	defer httpServer.Close()

	resp, err := httpServer.Client().Get(httpServer.URL)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, 2, resp.ProtoMajor)

	// The body is never read, so the client's flow-control window fills and the handler blocks.
	waitForAtLeast(t, "the subscriber to be added", 1, func() int { return len(rec.snapshotAdded()) })
	publishBlockingEvents(server, channel)

	waitForAtLeast(t, "the handler to exit", 1, func() int { return len(rec.snapshotRemoved()) })
	assert.Equal(t, ReasonWriteError, rec.snapshotRemoved()[0].Reason)
	require.Len(t, rec.snapshotWriteErrors(), 1)
}

// A deadline left armed between events would reset an idle HTTP/2 stream.
func TestServerWriteTimeoutDoesNotAffectHTTP2ClientThatKeepsReading(t *testing.T) {
	channel := "test"
	rec := &traceRecorder{}
	const writeTimeout = 100 * time.Millisecond
	server := newWriteTimeoutServer(rec, writeTimeout)
	defer server.Close()
	httpServer := newHTTP2Server(t, server.Handler(channel))
	defer httpServer.Close()

	resp, err := httpServer.Client().Get(httpServer.URL)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, 2, resp.ProtoMajor)
	reader := newSSEReader(t, resp.Body)

	for i := 0; i < 4; i++ {
		time.Sleep(writeTimeout + 50*time.Millisecond)
		want := fmt.Sprintf("event-%d", i)
		server.Publish([]string{channel}, &publication{id: strconv.Itoa(i), data: want})
		reader.waitFor(t, want)
	}
	assert.Empty(t, rec.snapshotWriteErrors())
	assert.Empty(t, rec.snapshotRemoved())
}
