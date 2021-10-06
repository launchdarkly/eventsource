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
	"time"

	"github.com/launchdarkly/eventsource"
)

type jsonObject map[string]interface{}

type createStreamOpts struct {
	URL            string            `json:"url"`
	Tag            string            `json:"tag"`
	InitialDelayMS *int              `json:"initialDelayMs"`
	LastEventID    string            `json:"lastEventId"`
	Method         string            `json:"method"`
	Body           string            `json:"body"`
	Headers        map[string]string `json:"headers"`
	ReadTimeoutMS  *int              `json:"readTimeoutMs"`
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		switch req.Method {
		case "GET":
			getServiceStatus(w)
		case "POST":
			postCreateStream(w, req)
		default:
			w.WriteHeader(405)
		}
	})
	server := &http.Server{Handler: mux, Addr: ":8000"}
	_ = server.ListenAndServe()
}

func getServiceStatus(w http.ResponseWriter) {
	resp := jsonObject{
		"capabilities": []string{
			"cr-only",
			"headers",
			"last-event-id",
			"post",
			"read-timeout",
			"report",
		},
	}
	data, _ := json.Marshal(resp)
	w.Header().Add("Content-Type", "application/json")
	w.Header().Add("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(200)
	_, _ = w.Write(data)
}

func postCreateStream(w http.ResponseWriter, req *http.Request) {
	var opts createStreamOpts
	err := json.NewDecoder(req.Body).Decode(&opts)
	if err != nil {
		writeError(w, err)
		return
	}

	log := log.New(os.Stdout, fmt.Sprintf("[%s]: ", opts.Tag),
		log.Ldate|log.Ltime|log.Lmicroseconds|log.Lmsgprefix)
	log.Printf("Starting stream to %s", opts.URL)

	method := "GET"
	if opts.Method != "" {
		method = opts.Method
	}
	var body io.Reader
	if opts.Body != "" {
		body = bytes.NewBufferString(opts.Body)
	}
	streamReq, _ := http.NewRequest(method, opts.URL, body)
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

	es, err := eventsource.SubscribeWithRequestAndOptions(streamReq, streamOpts...)

	if err != nil {
		log.Printf("Failed to start stream: %s", err)
		writeError(w, err)
		return
	}

	w.WriteHeader(200)
	flusher := w.(http.Flusher)
	flusher.Flush()

	sendMessage(w, map[string]interface{}{
		"kind": "hello",
	})

	closeNotifyCh := req.Context().Done()

	for {
		select {
		case <-closeNotifyCh:
			log.Println("Test ended")
			es.Close()
			return

		case ev := <-es.Events:
			evProps := jsonObject{
				"type": ev.Event(),
				"data": ev.Data(),
				"id":   ev.Id(),
			}
			log.Printf("Received event from stream (%s)", ev.Event())
			sendMessage(w, jsonObject{"kind": "event", "event": evProps})

		case err := <-es.Errors:
			log.Printf("Received error from stream: %s", err.Error())
			sendMessage(w, jsonObject{"kind": "error", "error": err.Error()})
		}
	}
}

func writeError(w http.ResponseWriter, err error) {
	w.WriteHeader(400)
	_, _ = w.Write([]byte(err.Error()))
}

func sendMessage(w io.Writer, message jsonObject) {
	bytes, _ := json.Marshal(message)
	_, _ = w.Write(bytes)
	lf := []byte{'\n'}
	_, _ = w.Write(lf)
	w.(http.Flusher).Flush()
}
