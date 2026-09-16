package eventsource

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSingleLine(t *testing.T) {
	assert.Equal(t, "plain text", singleLine("plain text"))

	// A newline would let the text end a log line and start one of its own.
	assert.Equal(t, "denied FAKE Error: forged line", singleLine("denied\nFAKE Error: forged line"))
	assert.Equal(t, "a b c", singleLine("a\rb\x00c"))
	assert.Equal(t, "a b", singleLine("a\tb"))
	assert.Equal(t, "trimmed", singleLine("\n  trimmed  \n"))
	assert.Equal(t, "", singleLine("\n\n"))
}

func TestReadErrorBody(t *testing.T) {
	assert.Equal(t, "short", readErrorBody(strings.NewReader("short")))
	assert.Equal(t, "", readErrorBody(strings.NewReader("")))

	atLimit := strings.Repeat("x", maxErrorBodyLength)
	assert.Equal(t, atLimit, readErrorBody(strings.NewReader(atLimit)))

	overLimit := readErrorBody(strings.NewReader(strings.Repeat("x", maxErrorBodyLength+1)))
	assert.Equal(t, strings.Repeat("x", maxErrorBodyLength)+"... (truncated)", overLimit)
}

// An error response body is diagnostic, and it comes from whatever answered the request. Keeping
// all of it would let a large error page sit in memory and reach a caller's log in full.
func TestSubscriptionErrorBodyIsBounded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(strings.Repeat("padding ", 10000)))
	}))
	defer server.Close()

	_, err := SubscribeWithURL(server.URL)

	se, ok := err.(SubscriptionError)
	require.True(t, ok, "expected a SubscriptionError, got %T", err)
	assert.Equal(t, http.StatusInternalServerError, se.Code)
	assert.Len(t, se.Message, maxErrorBodyLength+len("... (truncated)"))
	assert.True(t, strings.HasSuffix(se.Message, "... (truncated)"))
}

// The body must not be able to forge a log entry in a caller that logs the error.
func TestSubscriptionErrorStringCannotSpanLines(t *testing.T) {
	const canary = "CANARY-BODY"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(canary + "\nFAKE Error: forged log line\n"))
	}))
	defer server.Close()

	_, err := SubscribeWithURL(server.URL)

	se, ok := err.(SubscriptionError)
	require.True(t, ok, "expected a SubscriptionError, got %T", err)
	// The field keeps the body as it was sent, so a caller that wants it still has it.
	assert.Contains(t, se.Message, "\n")

	line := se.Error()
	assert.NotContains(t, line, "\n", "the body must not be able to end the line")
	assert.Equal(t, "error 401: "+canary+" FAKE Error: forged log line", line)
}

// The library logs the error text itself when it retries, so the same protection has to hold
// for the line it writes.
func TestRetryLogLineCannotSpanLines(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("denied\nFAKE Error: forged log line\n"))
	}))
	defer server.Close()

	logger := &capturingLogger{}
	_, err := SubscribeWithURL(server.URL,
		StreamOptionCanRetryFirstConnection(50*time.Millisecond),
		StreamOptionInitialRetry(time.Millisecond),
		StreamOptionLogger(logger),
	)
	require.Error(t, err)

	lines := logger.lines()
	require.NotEmpty(t, lines, "expected the library to log its retry line")
	for _, line := range lines {
		assert.NotContains(t, strings.TrimSuffix(line, "\n"), "\n",
			"the body must not be able to end the line")
	}
}

type capturingLogger struct {
	mu  sync.Mutex
	out []string
}

func (l *capturingLogger) Println(values ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.out = append(l.out, fmt.Sprint(values...))
}

func (l *capturingLogger) Printf(format string, values ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.out = append(l.out, fmt.Sprintf(format, values...))
}

func (l *capturingLogger) lines() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.out...)
}
