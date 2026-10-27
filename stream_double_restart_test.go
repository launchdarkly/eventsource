package eventsource

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestStreamRestartWhileRetryPendingDoesNotLeakConnection(t *testing.T) {
	var open int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&open, 1)
		defer atomic.AddInt32(&open, -1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()
	defer httpServer.CloseClientConnections()

	stream := mustSubscribe(t, httpServer.URL, StreamOptionInitialRetry(50*time.Millisecond))
	defer stream.Close()

	stream.Restart()
	time.Sleep(10 * time.Millisecond) // the first restart is processed and a retry is pending
	stream.Restart()

	time.Sleep(500 * time.Millisecond)
	assert.Equal(t, int32(1), atomic.LoadInt32(&open))
}
