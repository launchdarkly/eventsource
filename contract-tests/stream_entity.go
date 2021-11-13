package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/launchdarkly/eventsource"
)

type streamEntity struct {
	sse    *eventsource.Stream
	opts   streamOpts
	logger *log.Logger
	closer chan struct{}
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