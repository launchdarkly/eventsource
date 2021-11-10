package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/launchdarkly/eventsource"
)

var supportedCapabilities = []string{
	"headers",
	"last-event-id",
	"post",
	"read-timeout",
	"report",
	"restart",
}
var streams = make(map[string]*streamEntity)
var streamCounter = 0
var lock sync.Mutex

type jsonObject map[string]interface{}

type streamOpts struct {
	StreamURL      string            `json:"streamUrl"`
	CallbackURL    string            `json:"callbackURL"`
	Tag            string            `json:"tag"`
	InitialDelayMS *int              `json:"initialDelayMs"`
	LastEventID    string            `json:"lastEventId"`
	Method         string            `json:"method"`
	Body           string            `json:"body"`
	Headers        map[string]string `json:"headers"`
	ReadTimeoutMS  *int              `json:"readTimeoutMs"`
}

type streamEntity struct {
	sse    *eventsource.Stream
	opts   streamOpts
	logger *log.Logger
	closer chan struct{}
}

type commandParams struct {
	Command string `json:"command"`
}

func newStreamEntity(opts streamOpts) *streamEntity {
	e := &streamEntity{
		opts:   opts,
		closer: make(chan struct{}),
	}
	e.logger = log.New(os.Stdout, fmt.Sprintf("[%s]: ", opts.Tag),
		log.Ldate|log.Ltime|log.Lmicroseconds|log.Lmsgprefix)
	e.logger.Printf("Starting stream from %s", opts.StreamURL)

	method := "GET"
	if opts.Method != "" {
		method = opts.Method
	}
	var body io.Reader
	if opts.Body != "" {
		body = bytes.NewBufferString(opts.Body)
	}
	streamReq, _ := http.NewRequest(method, opts.StreamURL, body)
	for k, v := range opts.Headers {
		streamReq.Header.Set(k, v)
	}
	var streamOpts []eventsource.StreamOption
	if opts.InitialDelayMS != nil {
		streamOpts = append(streamOpts,
			eventsource.StreamOptionInitialRetry(time.Duration(*opts.InitialDelayMS)*time.Millisecond))
	}
	if opts.LastEventID != "" {
		streamOpts = append(streamOpts, eventsource.StreamOptionLastEventID(opts.LastEventID))
	}
	if opts.ReadTimeoutMS != nil {
		streamOpts = append(streamOpts,
			eventsource.StreamOptionReadTimeout(time.Duration(*opts.ReadTimeoutMS)*time.Millisecond))
	}

	sse, err := eventsource.SubscribeWithRequestAndOptions(streamReq, streamOpts...)

	if err != nil {
		e.logger.Printf("Failed to start stream: %s", err)
		e.sendMessage(jsonObject{"kind": "error", "error": err.Error()})
		return e
	}
	e.sse = sse

	go func() {
		for {
			select {
			case <-e.closer:
				return

			case ev := <-sse.Events:
				if ev == nil {
					return
				}
				evProps := jsonObject{
					"type": ev.Event(),
					"data": ev.Data(),
					"id":   ev.Id(),
				}
				e.logger.Printf("Received event from stream (%s)", ev.Event())
				e.sendMessage(jsonObject{"kind": "event", "event": evProps})

			case err := <-sse.Errors:
				if err != nil {
					e.logger.Printf("Received error from stream: %s", err.Error())
					e.sendMessage(jsonObject{"kind": "error", "error": err.Error()})
				}
			}
		}
	}()

	return e
}

func (e *streamEntity) doCommand(command string) bool {
	e.logger.Printf("Received command %q", command)
	if command == "restart" {
		e.sse.Restart()
		return true
	}
	return false
}

func (e *streamEntity) close() {
	e.logger.Println("Test ended")
	close(e.closer)
	if e.sse != nil {
		e.sse.Close()
	}
}

func (e *streamEntity) sendMessage(message jsonObject) {
	data, _ := json.Marshal(message)
	resp, err := http.DefaultClient.Post(e.opts.CallbackURL, "application/json", bytes.NewBuffer(data))
	if err != nil {
		e.logger.Printf("Error sending callback message: %s", err)
		return
	}
	if resp.Body != nil {
		_ = resp.Body.Close()
	}
	if resp.StatusCode >= 300 {
		e.logger.Printf("Callback endpoint returned HTTP %d", resp.StatusCode)
	}
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET":
			getServiceStatus(w)
		case "POST":
			postCreateStream(w, r)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/streams/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/streams/")
		lock.Lock()
		stream := streams[id]
		lock.Unlock()
		if stream == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch r.Method {
		case "POST":
			postStreamCommand(stream, w, r)
		case "DELETE":
			deleteStream(stream, id, w)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	server := &http.Server{Handler: mux, Addr: ":8000"}
	_ = server.ListenAndServe()
}

func getServiceStatus(w http.ResponseWriter) {
	resp := jsonObject{
		"capabilities": supportedCapabilities,
	}
	data, _ := json.Marshal(resp)
	w.Header().Add("Content-Type", "application/json")
	w.Header().Add("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(200)
	_, _ = w.Write(data)
}

func postCreateStream(w http.ResponseWriter, req *http.Request) {
	var opts streamOpts
	if err := json.NewDecoder(req.Body).Decode(&opts); err != nil {
		sendError(w, err)
		return
	}

	e := newStreamEntity(opts)
	lock.Lock()
	streamCounter++
	streamID := strconv.Itoa(streamCounter)
	streams[streamID] = e
	lock.Unlock()

	w.Header().Add("Location", fmt.Sprintf("/streams/%s", streamID))
	w.WriteHeader(http.StatusCreated)

	e.sendMessage(jsonObject{"kind": "hello"})
}

func postStreamCommand(stream *streamEntity, w http.ResponseWriter, req *http.Request) {
	var params commandParams
	if err := json.NewDecoder(req.Body).Decode(&params); err != nil {
		sendError(w, err)
		return
	}

	if !stream.doCommand(params.Command) {
		sendError(w, fmt.Errorf("unrecognized command %q", params.Command))
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func deleteStream(stream *streamEntity, id string, w http.ResponseWriter) {
	stream.close()
	lock.Lock()
	delete(streams, id)
	lock.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func sendError(w http.ResponseWriter, err error) {
	w.WriteHeader(400)
	_, _ = w.Write([]byte(err.Error()))
}
