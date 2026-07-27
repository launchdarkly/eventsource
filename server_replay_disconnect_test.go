package eventsource

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests cover the behavior of a registered Repository whose producer goroutine may be
// blocked sending replayed events when a subscriber disconnects. They exercise both the
// unconditional background drain performed by the handler (which unblocks any Repository) and the
// optional RepositoryWithContext extension (which lets a Repository observe cancellation directly).

const replayTestDeadline = 3 * time.Second

// plainReplayRepo implements only the original Repository interface. Its producer sends one event,
// waits for the test to release it, then sends a series of events on an unbuffered channel. This
// lets the test position the producer so that it becomes blocked on a channel send exactly when the
// subscriber disconnects.
type plainReplayRepo struct {
	started  chan struct{}
	release  chan struct{}
	finished chan struct{}
}

func newPlainReplayRepo() *plainReplayRepo {
	return &plainReplayRepo{
		started:  make(chan struct{}),
		release:  make(chan struct{}),
		finished: make(chan struct{}),
	}
}

func (r *plainReplayRepo) Replay(channel, id string) chan Event {
	out := make(chan Event)
	go func() {
		defer close(r.finished)
		defer close(out)
		close(r.started)
		out <- &publication{id: "0", data: "first"}
		<-r.release
		for i := 1; i < 50; i++ {
			out <- &publication{id: strconv.Itoa(i), data: "more"}
		}
	}()
	return out
}

// ctxReplayRepo implements both Replay and ReplayWithContext. It records which method was called
// and (for ReplayWithContext) whether it observed context cancellation.
type ctxReplayRepo struct {
	started         chan struct{}
	release         chan struct{}
	finished        chan struct{}
	ctxObserved     chan struct{}
	replayCalled    int32
	replayCtxCalled int32
	// blockOnSend, when true, makes the producer block on a channel send after the first event;
	// otherwise it waits directly on ctx.Done() after the first event.
	blockOnSend bool
}

func newCtxReplayRepo() *ctxReplayRepo {
	return &ctxReplayRepo{
		started:     make(chan struct{}),
		release:     make(chan struct{}),
		finished:    make(chan struct{}),
		ctxObserved: make(chan struct{}),
	}
}

func (r *ctxReplayRepo) Replay(channel, id string) chan Event {
	atomic.StoreInt32(&r.replayCalled, 1)
	out := make(chan Event)
	go func() {
		defer close(out)
		out <- &publication{id: "0", data: "first"}
	}()
	return out
}

func (r *ctxReplayRepo) ReplayWithContext(ctx context.Context, channel, id string) <-chan Event {
	atomic.StoreInt32(&r.replayCtxCalled, 1)
	out := make(chan Event)
	go func() {
		defer close(r.finished)
		defer close(out)
		close(r.started)
		select {
		case out <- &publication{id: "0", data: "first"}:
		case <-ctx.Done():
			close(r.ctxObserved)
			return
		}
		if r.blockOnSend {
			<-r.release
			for i := 1; i < 50; i++ {
				select {
				case out <- &publication{id: strconv.Itoa(i), data: "more"}:
				case <-ctx.Done():
					close(r.ctxObserved)
					return
				}
			}
			return
		}
		// Wait purely on cancellation so the drain does not race the context observation.
		<-ctx.Done()
		close(r.ctxObserved)
	}()
	return out
}

// rawSSEConn dials the server directly so the test can control exactly how the connection is closed
// (clean FIN vs. abrupt RST). It returns the connection and a reader positioned at the response body.
func rawSSEConn(t *testing.T, url string) *net.TCPConn {
	t.Helper()
	u, err := neturl.Parse(url)
	require.NoError(t, err)
	path := u.Path
	if path == "" {
		path = "/"
	}
	conn, err := net.Dial("tcp", u.Host)
	require.NoError(t, err)
	_, err = fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\n\r\n", path, u.Host)
	require.NoError(t, err)
	return conn.(*net.TCPConn)
}

// sseReader consumes an SSE response body on a single background goroutine, exposing the lines it
// reads via a channel. Using one goroutine per response avoids concurrent reads of the same reader.
type sseReader struct {
	lines chan string
	done  chan struct{}
}

func newSSEReader(t *testing.T, r interface{ Read([]byte) (int, error) }) *sseReader {
	t.Helper()
	s := &sseReader{lines: make(chan string), done: make(chan struct{})}
	go func() {
		rd := bufio.NewReader(r)
		for {
			line, err := rd.ReadString('\n')
			if line != "" {
				select {
				case s.lines <- line:
				case <-s.done:
					return
				}
			}
			if err != nil {
				close(s.lines)
				return
			}
		}
	}()
	t.Cleanup(func() { close(s.done) })
	return s
}

// waitFor reads lines until one contains the wanted substring, tolerating chunked framing.
func (s *sseReader) waitFor(t *testing.T, want string) {
	t.Helper()
	deadline := time.After(replayTestDeadline)
	for {
		select {
		case line, ok := <-s.lines:
			if !ok {
				t.Fatalf("stream ended before receiving %q", want)
			}
			if strings.Contains(line, want) {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting to receive %q", want)
		}
	}
}

func assertClosedWithin(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(replayTestDeadline):
		t.Fatalf("%s did not happen within %s", what, replayTestDeadline)
	}
}

// TestReplayProducerUnblocksOnCleanDisconnectPlainRepository verifies that a plain Repository whose
// producer is blocked on a channel send is unblocked (via the handler's background drain) when the
// subscriber cleanly disconnects (FIN).
func TestReplayProducerUnblocksOnCleanDisconnectPlainRepository(t *testing.T) {
	channel := "test"
	server := NewServer()
	server.ReplayAll = true
	defer server.Close()
	repo := newPlainReplayRepo()
	server.Register(channel, repo)
	httpServer := httptest.NewServer(server.Handler(channel))
	defer httpServer.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", httpServer.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	<-repo.started

	rd := newSSEReader(t, resp.Body)
	rd.waitFor(t, "data: first")

	cancel()
	_ = resp.Body.Close()
	time.Sleep(100 * time.Millisecond)
	close(repo.release)

	assertClosedWithin(t, repo.finished, "producer goroutine exit")
}

// TestReplayProducerUnblocksOnAbruptResetPlainRepository is the same as above but the subscriber
// aborts the connection with a TCP reset (RST) rather than a clean close.
func TestReplayProducerUnblocksOnAbruptResetPlainRepository(t *testing.T) {
	channel := "test"
	server := NewServer()
	server.ReplayAll = true
	defer server.Close()
	repo := newPlainReplayRepo()
	server.Register(channel, repo)
	httpServer := httptest.NewServer(server.Handler(channel))
	defer httpServer.Close()

	conn := rawSSEConn(t, httpServer.URL)
	<-repo.started
	rd := newSSEReader(t, conn)
	rd.waitFor(t, "data: first")

	// Force an abrupt RST instead of a clean FIN.
	require.NoError(t, conn.SetLinger(0))
	require.NoError(t, conn.Close())
	time.Sleep(100 * time.Millisecond)
	close(repo.release)

	assertClosedWithin(t, repo.finished, "producer goroutine exit")
}

// TestReplayWithContextObservesCancellationOnDisconnect verifies that a RepositoryWithContext is
// given a context that is cancelled when the subscriber disconnects, and that the producer can
// observe that cancellation directly.
func TestReplayWithContextObservesCancellationOnDisconnect(t *testing.T) {
	channel := "test"
	server := NewServer()
	server.ReplayAll = true
	defer server.Close()
	repo := newCtxReplayRepo() // blockOnSend == false: producer waits on ctx.Done() after first event
	server.Register(channel, repo)
	httpServer := httptest.NewServer(server.Handler(channel))
	defer httpServer.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", httpServer.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	<-repo.started

	rd := newSSEReader(t, resp.Body)
	rd.waitFor(t, "data: first")

	cancel()
	_ = resp.Body.Close()

	assertClosedWithin(t, repo.ctxObserved, "producer observing ctx cancellation")
	assertClosedWithin(t, repo.finished, "producer goroutine exit")
	assert.Equal(t, int32(1), atomic.LoadInt32(&repo.replayCtxCalled), "ReplayWithContext should have been called")
	assert.Equal(t, int32(0), atomic.LoadInt32(&repo.replayCalled), "plain Replay should not have been called")
}

// TestReplayWithContextProducerUnblocksWhenBlockedOnSend verifies that a RepositoryWithContext
// producer that is blocked on a channel send at disconnect time still unblocks promptly (via either
// context cancellation or the background drain).
func TestReplayWithContextProducerUnblocksWhenBlockedOnSend(t *testing.T) {
	channel := "test"
	server := NewServer()
	server.ReplayAll = true
	defer server.Close()
	repo := newCtxReplayRepo()
	repo.blockOnSend = true
	server.Register(channel, repo)
	httpServer := httptest.NewServer(server.Handler(channel))
	defer httpServer.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", httpServer.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	<-repo.started

	rd := newSSEReader(t, resp.Body)
	rd.waitFor(t, "data: first")

	cancel()
	_ = resp.Body.Close()
	time.Sleep(100 * time.Millisecond)
	close(repo.release)

	assertClosedWithin(t, repo.finished, "producer goroutine exit")
}

// TestReplayWithContextIsPreferredOverReplay verifies that when a Repository implements both methods,
// the server uses ReplayWithContext.
func TestReplayWithContextIsPreferredOverReplay(t *testing.T) {
	channel := "test"
	server := NewServer()
	server.ReplayAll = true
	defer server.Close()
	repo := newCtxReplayRepo()
	server.Register(channel, repo)
	httpServer := httptest.NewServer(server.Handler(channel))
	defer httpServer.Close()

	resp, err := http.Get(httpServer.URL)
	require.NoError(t, err)
	defer resp.Body.Close()
	<-repo.started
	rd := newSSEReader(t, resp.Body)
	rd.waitFor(t, "data: first")

	assert.Equal(t, int32(1), atomic.LoadInt32(&repo.replayCtxCalled))
	assert.Equal(t, int32(0), atomic.LoadInt32(&repo.replayCalled))
}

// TestReplayNormalDeliveryPlainRepository verifies that when the subscriber stays connected, all
// replayed events from a plain Repository are delivered in order.
func TestReplayNormalDeliveryPlainRepository(t *testing.T) {
	channel := "test"
	server := NewServer()
	server.ReplayAll = true
	defer server.Close()
	repo := newPlainReplayRepo()
	server.Register(channel, repo)
	httpServer := httptest.NewServer(server.Handler(channel))
	defer httpServer.Close()

	resp, err := http.Get(httpServer.URL)
	require.NoError(t, err)
	defer resp.Body.Close()
	<-repo.started

	rd := newSSEReader(t, resp.Body)
	rd.waitFor(t, "data: first")
	close(repo.release)
	// Events 1..49 should all arrive; confirm the last one.
	rd.waitFor(t, "id: 49")
	assertClosedWithin(t, repo.finished, "producer goroutine exit")
}

// TestReplayNormalDeliveryContextRepository verifies full ordered delivery for a RepositoryWithContext.
func TestReplayNormalDeliveryContextRepository(t *testing.T) {
	channel := "test"
	server := NewServer()
	server.ReplayAll = true
	defer server.Close()
	repo := newCtxReplayRepo()
	repo.blockOnSend = true
	server.Register(channel, repo)
	httpServer := httptest.NewServer(server.Handler(channel))
	defer httpServer.Close()

	resp, err := http.Get(httpServer.URL)
	require.NoError(t, err)
	defer resp.Body.Close()
	<-repo.started

	rd := newSSEReader(t, resp.Body)
	rd.waitFor(t, "data: first")
	close(repo.release)
	rd.waitFor(t, "id: 49")
	assertClosedWithin(t, repo.finished, "producer goroutine exit")
}

// TestReplayEmptyBatchNilChannel verifies that a Repository returning nil (no events) does not hang,
// and that normal publishing still works afterward.
func TestReplayEmptyBatchNilChannel(t *testing.T) {
	channel := "test"
	server := NewServer()
	server.ReplayAll = true
	defer server.Close()
	server.Register(channel, nilRepo{})
	httpServer := httptest.NewServer(server.Handler(channel))
	defer httpServer.Close()

	resp, err := http.Get(httpServer.URL)
	require.NoError(t, err)
	defer resp.Body.Close()

	// After the (empty) replay, a normally published event should still be delivered.
	go func() {
		time.Sleep(50 * time.Millisecond)
		server.Publish([]string{channel}, &publication{id: "live", data: "published"})
	}()
	rd := newSSEReader(t, resp.Body)
	rd.waitFor(t, "data: published")
}

// TestReplayEmptyBatchClosedChannel verifies that a Repository returning an already-closed channel
// does not hang and normal publishing still works afterward.
func TestReplayEmptyBatchClosedChannel(t *testing.T) {
	channel := "test"
	server := NewServer()
	server.ReplayAll = true
	defer server.Close()
	server.Register(channel, closedRepo{})
	httpServer := httptest.NewServer(server.Handler(channel))
	defer httpServer.Close()

	resp, err := http.Get(httpServer.URL)
	require.NoError(t, err)
	defer resp.Body.Close()

	go func() {
		time.Sleep(50 * time.Millisecond)
		server.Publish([]string{channel}, &publication{id: "live", data: "published"})
	}()
	rd := newSSEReader(t, resp.Body)
	rd.waitFor(t, "data: published")
}

type nilRepo struct{}

func (nilRepo) Replay(channel, id string) chan Event { return nil }

type closedRepo struct{}

func (closedRepo) Replay(channel, id string) chan Event {
	out := make(chan Event)
	close(out)
	return out
}

// TestReplayMultipleConcurrentSubscriptionsUnblock verifies that several concurrent subscribers that
// each disconnect mid-replay all have their producer goroutines unblocked.
func TestReplayMultipleConcurrentSubscriptionsUnblock(t *testing.T) {
	const n = 5
	mux := http.NewServeMux()
	server := NewServer()
	server.ReplayAll = true
	defer server.Close()

	repos := make([]*plainReplayRepo, n)
	for i := 0; i < n; i++ {
		channel := "chan-" + strconv.Itoa(i)
		repos[i] = newPlainReplayRepo()
		server.Register(channel, repos[i])
		mux.HandleFunc("/"+channel, server.Handler(channel))
	}
	httpServer := httptest.NewServer(mux)
	defer httpServer.Close()

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithCancel(context.Background())
			req, _ := http.NewRequestWithContext(ctx, "GET", httpServer.URL+"/chan-"+strconv.Itoa(i), nil)
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			<-repos[i].started
			rd := newSSEReader(t, resp.Body)
			rd.waitFor(t, "data: first")
			cancel()
			_ = resp.Body.Close()
			time.Sleep(50 * time.Millisecond)
			close(repos[i].release)
		}(i)
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		assertClosedWithin(t, repos[i].finished, fmt.Sprintf("producer %d exit", i))
	}
}

// TestReplayProducerUnblocksOnImmediateDisconnectPlainRepository verifies that a plain Repository
// producer is unblocked even when the subscriber disconnects immediately, before reading any of the
// stream -- i.e. before the handler has dequeued the replay batch from its buffered event channel.
// This exercises a different window than the tests above, which all wait for "data: first" (and
// therefore guarantee the handler is already consuming the batch) before disconnecting. Because the
// disconnect races the handler's dequeue, a single trial can land on either side of the window, so
// the test runs many trials and requires every producer to finish; on code without the
// unsubscribe-path drain, most trials strand the producer.
func TestReplayProducerUnblocksOnImmediateDisconnectPlainRepository(t *testing.T) {
	const trials = 20
	for i := 0; i < trials; i++ {
		func() {
			channel := "test"
			server := NewServer()
			server.ReplayAll = true
			defer server.Close()
			repo := newPlainReplayRepo()
			server.Register(channel, repo)
			httpServer := httptest.NewServer(server.Handler(channel))
			defer httpServer.Close()

			conn := rawSSEConn(t, httpServer.URL)
			// Wait only until the producer is running (which happens as the subscription is
			// registered), NOT until any event has been received, then disconnect at once --
			// alternating between an abrupt RST and a clean FIN.
			<-repo.started
			if i%2 == 0 {
				require.NoError(t, conn.SetLinger(0))
			}
			require.NoError(t, conn.Close())
			close(repo.release)

			assertClosedWithin(t, repo.finished,
				fmt.Sprintf("producer goroutine exit (trial %d)", i))
		}()
	}
}

// slowReplayRepo delays returning from Replay. This lets a test arrange for the subscriber's
// handler to have exited (and swept its still-empty event channel) before the batch is ever
// enqueued, so that only the Server's unsubscription and shutdown paths can dispose of the batch.
type slowReplayRepo struct {
	started  chan struct{}
	finished chan struct{}
	delay    time.Duration
}

func newSlowReplayRepo(delay time.Duration) *slowReplayRepo {
	return &slowReplayRepo{
		started:  make(chan struct{}),
		finished: make(chan struct{}),
		delay:    delay,
	}
}

func (r *slowReplayRepo) Replay(channel, id string) chan Event {
	close(r.started)
	time.Sleep(r.delay)
	out := make(chan Event)
	go func() {
		defer close(r.finished)
		defer close(out)
		for i := 0; i < 50; i++ {
			out <- &publication{id: strconv.Itoa(i), data: "replayed"}
		}
	}()
	return out
}

// TestReplayProducerUnblocksWhenServerClosesDuringImmediateDisconnect verifies that a producer is
// not stranded when Server.Close races a subscriber that disconnected before its replay batch was
// even produced. In that ordering the handler has already exited (its exit-time sweep found
// nothing, because the batch had not been enqueued yet), so the batch can only be disposed of by
// the Server -- and the Server may select the quit case over the handler's queued unsubscription,
// so the shutdown path itself must drain it. Whether quit or the unsubscription is selected first
// is a coin flip per trial, so the test runs many trials and requires every producer to finish.
func TestReplayProducerUnblocksWhenServerClosesDuringImmediateDisconnect(t *testing.T) {
	const trials = 20
	for i := 0; i < trials; i++ {
		func() {
			channel := "test"
			server := NewServer()
			server.ReplayAll = true
			repo := newSlowReplayRepo(80 * time.Millisecond)
			server.Register(channel, repo)
			httpServer := httptest.NewServer(server.Handler(channel))
			defer httpServer.Close()

			conn := rawSSEConn(t, httpServer.URL)
			// Replay is now sleeping inside the Server's goroutine. Disconnect and give the
			// handler time to observe it, exit, and queue its unsubscription -- all before
			// Replay returns and the batch is enqueued.
			<-repo.started
			require.NoError(t, conn.SetLinger(0))
			require.NoError(t, conn.Close())
			time.Sleep(40 * time.Millisecond)
			// Close the server; the quit signal and the queued unsubscription are now both
			// ready when the Server finishes the registration it is still processing.
			server.Close()

			assertClosedWithin(t, repo.finished,
				fmt.Sprintf("producer goroutine exit (trial %d)", i))
		}()
	}
}

// TestReplayServerCloseDrainsBatchOfDepartedSubscriber pins the narrowest shutdown window
// directly: a subscriber whose context is already cancelled (the client is gone) and whose
// handler will never act again -- it has already swept its then-empty event channel, and its
// unsubscription will never be processed. It builds that state explicitly, registering a
// subscription with a cancelled context and no handler behind it, so the only thing that can
// dispose of the replay batch is the shutdown path's drain of departed subscribers still in the
// subscription map. This is a white-box test by necessity; from outside the process the window
// between a handler's sweep and its unsubscription send is microseconds wide.
func TestReplayServerCloseDrainsBatchOfDepartedSubscriber(t *testing.T) {
	channel := "test"
	server := NewServer()
	server.ReplayAll = true
	repo := newPlainReplayRepo()
	server.Register(channel, repo)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the subscriber is already gone
	eventCh := make(chan eventOrComment, server.BufferSize)
	sub := &subscription{
		channel: channel,
		out:     eventCh,
		ctx:     ctx,
	}
	server.subs <- sub // registration enqueues the batch; no handler will ever consume it
	<-repo.started
	close(repo.release)

	server.Close()
	assertClosedWithin(t, repo.finished, "producer goroutine exit")
}

// pacedReplayRepo produces a fixed series of events with small gaps between them, so a test can
// reliably interleave another action (such as Server.Close) with an in-progress delivery.
type pacedReplayRepo struct {
	started  chan struct{}
	finished chan struct{}
}

func newPacedReplayRepo() *pacedReplayRepo {
	return &pacedReplayRepo{started: make(chan struct{}), finished: make(chan struct{})}
}

func (r *pacedReplayRepo) Replay(channel, id string) chan Event {
	out := make(chan Event)
	go func() {
		defer close(r.finished)
		defer close(out)
		close(r.started)
		for i := 0; i < 30; i++ {
			out <- &publication{id: strconv.Itoa(i), data: "replayed"}
			time.Sleep(time.Millisecond)
		}
	}()
	return out
}

// TestReplayCloseDoesNotTruncateDeliveryToConnectedSubscriber verifies that closing the server
// while a still-connected subscriber is consuming a replay batch does not steal any of the
// batch's events from that subscriber: the handler must be left to deliver every remaining
// event, so the client's Last-Event-ID resume point stays accurate. This is the guard rail for
// the shutdown path's decision NOT to drain the batch of a subscriber whose request context is
// still live. The interleaving is racy, so the test repeats the scenario several times.
func TestReplayCloseDoesNotTruncateDeliveryToConnectedSubscriber(t *testing.T) {
	const trials = 8
	for i := 0; i < trials; i++ {
		func() {
			channel := "test"
			server := NewServer()
			server.ReplayAll = true
			repo := newPacedReplayRepo()
			server.Register(channel, repo)
			httpServer := httptest.NewServer(server.Handler(channel))
			defer httpServer.Close()

			resp, err := http.Get(httpServer.URL)
			require.NoError(t, err)
			defer resp.Body.Close()
			<-repo.started

			// Collect every id until the stream ends (the handler exits normally once the
			// batch is done and its channel has been closed by the shutdown), closing the
			// server as soon as the first event is seen so the shutdown lands mid-batch.
			// Every one of the 30 events must reach the subscriber; a shutdown path that
			// drained this batch would swallow a subset of them.
			rd := newSSEReader(t, resp.Body)
			seen := make(map[string]bool)
			closed := false
			deadline := time.After(replayTestDeadline)
		Collect:
			for {
				select {
				case line, ok := <-rd.lines:
					if !ok {
						break Collect
					}
					if strings.HasPrefix(line, "id: ") {
						seen[strings.TrimSpace(strings.TrimPrefix(line, "id: "))] = true
					}
					if !closed && strings.Contains(line, "data: replayed") {
						server.Close() // close mid-batch, subscriber still connected
						closed = true
					}
				case <-deadline:
					t.Fatalf("timed out waiting for the stream to end (trial %d)", i)
				}
			}
			require.True(t, closed, "stream ended before delivering any event (trial %d)", i)
			for id := 0; id < 30; id++ {
				assert.True(t, seen[strconv.Itoa(id)],
					"event %d was not delivered to the connected subscriber (trial %d)", id, i)
			}
			assertClosedWithin(t, repo.finished,
				fmt.Sprintf("producer goroutine exit (trial %d)", i))
		}()
	}
}

// TestReplayProducerUnblocksWhenServerCloses verifies that closing the server does not strand a
// Repository producer that is mid-replay.
func TestReplayProducerUnblocksWhenServerCloses(t *testing.T) {
	channel := "test"
	server := NewServer()
	server.ReplayAll = true
	repo := newPlainReplayRepo()
	server.Register(channel, repo)
	httpServer := httptest.NewServer(server.Handler(channel))
	defer httpServer.Close()

	resp, err := http.Get(httpServer.URL)
	require.NoError(t, err)
	<-repo.started
	rd := newSSEReader(t, resp.Body)
	rd.waitFor(t, "data: first")

	// Release the producer so it is actively delivering, then close the server and the client.
	close(repo.release)
	_ = resp.Body.Close()
	server.Close()

	assertClosedWithin(t, repo.finished, "producer goroutine exit after server close")
}
