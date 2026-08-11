package eventsource

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// Stream handles a connection for receiving Server Sent Events.
// It will try and reconnect if the connection is lost, respecting both
// received retry delays and event id's.
type Stream struct {
	c               *http.Client
	req             *http.Request
	queryParamsFunc *func(existing url.Values) url.Values
	lastEventID     string
	readTimeout     time.Duration
	retryDelay      *retryDelayStrategy
	// Events emits the events received by the stream
	Events chan Event
	// Errors emits any errors encountered while reading events from the stream.
	//
	// Errors during initialization of the stream are not pushed to this channel, since until the
	// Subscribe method has returned the caller would not be able to consume the channel. If you have
	// configured the Stream to be able to retry on initialization errors, but you still want to know
	// about those errors or control how they are handled, use StreamOptionErrorHandler.
	//
	// If an error handler has been specified with StreamOptionErrorHandler, the Errors channel is
	// not used and will be nil.
	Errors       chan error
	errorHandler StreamErrorHandler
	// Logger is a logger that, when set, will be used for logging informational messages.
	//
	// This field is exported for backward compatibility, but should not be set directly because
	// it may be used by multiple goroutines. Use SetLogger instead.
	Logger      Logger
	restarter   chan struct{}
	closer      chan struct{}
	closeOnce   sync.Once
	mu          sync.RWMutex
	connections int
}

var (
	// ErrReadTimeout is the error that will be emitted if a stream was closed due to not
	// receiving any data within the configured read timeout interval.
	ErrReadTimeout = errors.New("Read timeout on stream")
)

// SubscriptionError is an error object returned from a stream when there is an HTTP error.
type SubscriptionError struct {
	Code    int
	Message string
	Header  http.Header
}

func (e SubscriptionError) Error() string {
	s := fmt.Sprintf("error %d", e.Code)
	if e.Message != "" {
		s = s + ": " + e.Message
	}
	return s
}

// Subscribe to the Events emitted from the specified url.
// If lastEventId is non-empty it will be sent to the server in case it can replay missed events.
// Deprecated: use SubscribeWithURL instead.
func Subscribe(url, lastEventID string) (*Stream, error) {
	return SubscribeWithURL(url, StreamOptionLastEventID(lastEventID))
}

// SubscribeWithURL subscribes to the Events emitted from the specified URL. The stream can
// be configured by providing any number of StreamOption values.
func SubscribeWithURL(url string, options ...StreamOption) (*Stream, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	return SubscribeWithRequestAndOptions(req, options...)
}

// SubscribeWithRequest will take an http.Request to set up the stream, allowing custom headers
// to be specified, authentication to be configured, etc.
// Deprecated: use SubscribeWithRequestAndOptions instead.
func SubscribeWithRequest(lastEventID string, request *http.Request) (*Stream, error) {
	return SubscribeWithRequestAndOptions(request, StreamOptionLastEventID(lastEventID))
}

// SubscribeWith takes a HTTP client and request providing customization over both headers and
// control over the HTTP client settings (timeouts, tls, etc)
// If request.Body is set, then request.GetBody should also be set so that we can reissue the request
// Deprecated: use SubscribeWithRequestAndOptions instead.
func SubscribeWith(lastEventID string, client *http.Client, request *http.Request) (*Stream, error) {
	return SubscribeWithRequestAndOptions(request, StreamOptionHTTPClient(client),
		StreamOptionLastEventID(lastEventID))
}

// SubscribeWithRequestAndOptions takes an initial http.Request to set up the stream - allowing
// custom headers, authentication, etc. to be configured - and also takes any number of
// StreamOption values to set other properties of the stream, such as timeouts or a specific
// HTTP client to use.
func SubscribeWithRequestAndOptions(request *http.Request, options ...StreamOption) (*Stream, error) {
	defaultClient := *http.DefaultClient

	// The four legacy retry-timing fields (initialRetry, backoffMaxDelay,
	// jitterRatio, retryResetInterval) are intentionally left nil here
	// so we can distinguish between "caller never set" and "caller
	// explicitly requested zero".
	configuredOptions := streamOptions{
		httpClient: &defaultClient,
	}

	for _, o := range options {
		if err := o.apply(&configuredOptions); err != nil {
			return nil, err
		}
	}

	stream := newStream(request, configuredOptions)

	var initialRetryTimeoutCh <-chan time.Time
	var lastError error
	if configuredOptions.initialRetryTimeout > 0 {
		initialRetryTimeoutCh = time.After(configuredOptions.initialRetryTimeout)
	}
	for {
		r, h, err := stream.connect()
		if err == nil {
			go stream.stream(r, h)
			return stream, nil
		}
		lastError = err
		if configuredOptions.initialRetryTimeout == 0 {
			return nil, err
		}
		if configuredOptions.errorHandler != nil {
			result := configuredOptions.errorHandler(err)
			if result.ActivateProfile != nil {
				stream.retryDelay.activateProfile(result.ActivateProfile)
			}
			if result.CloseNow {
				return nil, err
			}
		}
		// We never push errors to the Errors channel during initialization-- the caller would have no way to
		// consume the channel, since we haven't returned a Stream instance.
		delay := stream.retryDelay.NextRetryDelay(time.Now())
		if configuredOptions.logger != nil {
			configuredOptions.logger.Printf("Connection failed (%s), retrying in %0.4f secs\n", err, delay.Seconds())
		}
		nextRetryCh := time.After(delay)
		select {
		case <-initialRetryTimeoutCh:
			if lastError == nil {
				lastError = errors.New("timeout elapsed while waiting to connect")
			}
			return nil, lastError
		case <-nextRetryCh:
			continue
		}
	}
}

func newStream(request *http.Request, configuredOptions streamOptions) *Stream {
	retryDelay := newRetryDelayStrategyFromOptions(&configuredOptions, 0)

	stream := &Stream{
		c:            configuredOptions.httpClient,
		lastEventID:  configuredOptions.lastEventID,
		readTimeout:  configuredOptions.readTimeout,
		req:          request,
		retryDelay:   retryDelay,
		Events:       make(chan Event),
		errorHandler: configuredOptions.errorHandler,
		Logger:       configuredOptions.logger,
		restarter:    make(chan struct{}, 1),
		closer:       make(chan struct{}),
	}

	if configuredOptions.queryParamsFunc != nil {
		stream.queryParamsFunc = configuredOptions.queryParamsFunc
	}

	if configuredOptions.errorHandler == nil {
		// The Errors channel is only used if there is no error handler.
		stream.Errors = make(chan error)
	}

	return stream
}

// Restart forces the stream to drop the currently active connection and attempt to connect again, in the
// same way it would if the connection had failed. There will be a delay before reconnection, as defined
// by the Stream configuration (StreamOptionInitialRetry, StreamOptionUseBackoff, etc.).
//
// This method is safe for concurrent access. Its behavior is asynchronous: Restart returns immediately
// and the connection is restarted as soon as possible from another goroutine after that. It is possible
// for additional events from the original connection to be delivered during that interval.ssible.
//
// If the stream has already been closed with Close, Restart has no effect.
func (stream *Stream) Restart() {
	// Note the non-blocking send: if there's already been a Restart call that hasn't been processed yet,
	// we'll just leave that one in the channel.
	select {
	case stream.restarter <- struct{}{}:
		break
	default:
		break
	}
}

// Close closes the stream permanently. It is safe for concurrent access and can be called multiple times.
func (stream *Stream) Close() {
	stream.closeOnce.Do(func() {
		close(stream.closer)
	})
}

func (stream *Stream) connect() (io.ReadCloser, http.Header, error) {
	var err error
	var resp *http.Response
	stream.req.Header.Set("Cache-Control", "no-cache")
	stream.req.Header.Set("Accept", "text/event-stream")
	if len(stream.lastEventID) > 0 {
		stream.req.Header.Set("Last-Event-ID", stream.lastEventID)
	}
	req := *stream.req
	if stream.queryParamsFunc != nil {
		req.URL.RawQuery = (*stream.queryParamsFunc)(req.URL.Query()).Encode()
	}

	// All but the initial connection will need to regenerate the body
	if stream.connections > 0 && req.GetBody != nil {
		if req.Body, err = req.GetBody(); err != nil {
			return nil, nil, err
		}
	}

	if resp, err = stream.c.Do(&req); err != nil {
		return nil, nil, err
	}
	stream.connections++
	if resp.StatusCode != 200 {
		message, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		err = SubscriptionError{
			Code:    resp.StatusCode,
			Message: string(message),
			Header:  resp.Header,
		}
		return nil, nil, err
	}
	return resp.Body, resp.Header, nil
}

func (stream *Stream) stream(r io.ReadCloser, h http.Header) {
	retryChan := make(chan struct{}, 1)

	scheduleRetry := func() {
		logger := stream.getLogger()
		delay := stream.retryDelay.NextRetryDelay(time.Now())
		if logger != nil {
			logger.Printf("Reconnecting in %0.4f secs", delay.Seconds())
		}
		time.AfterFunc(delay, func() {
			retryChan <- struct{}{}
		})
	}

	reportErrorAndMaybeContinue := func(err error) bool {
		if stream.errorHandler != nil {
			result := stream.errorHandler(err)
			if result.ActivateProfile != nil {
				stream.retryDelay.activateProfile(result.ActivateProfile)
			}
			if result.CloseNow {
				stream.Close()
				return false
			}
		} else if stream.Errors != nil {
			stream.Errors <- err
		}
		return true
	}

NewStream:
	for {
		events := make(chan Event)
		errs := make(chan error)

		if r != nil {
			dec := NewDecoderWithOptions(r,
				DecoderOptionReadTimeout(stream.readTimeout),
				DecoderOptionLastEventID(stream.lastEventID),
				DecoderOptionHeaders(h),
			)
			go func() {
				for {
					ev, err := dec.Decode()

					if err != nil {
						errs <- err
						close(errs)
						close(events)
						return
					}
					events <- ev
				}
			}()
		}

		discardCurrentStream := func() {
			if r != nil {
				_ = r.Close()
				r = nil
				// Allow the decoding goroutine to terminate. After r.Close it will either return an
				// error from Decode (sending on errs, then closing both channels) or, if it had
				// already decoded an event, still be blocked sending that event on events. We must
				// therefore drain both channels concurrently: draining them sequentially deadlocks
				// if the goroutine is blocked sending on the one we are not yet reading (e.g. we
				// wait on errs while it is stuck on events), and neither side can make progress.
				var wg sync.WaitGroup
				wg.Add(2)
				go func() {
					defer wg.Done()
					for range errs { //nolint:revive // draining until the channel is closed
					}
				}()
				go func() {
					defer wg.Done()
					for range events { //nolint:revive // draining until the channel is closed
					}
				}()
				wg.Wait()
			}
		}

		for {
			select {
			case <-stream.restarter:
				discardCurrentStream()
				scheduleRetry()
				continue NewStream
			case err := <-errs:
				if !reportErrorAndMaybeContinue(err) {
					break NewStream
				}
				discardCurrentStream()
				scheduleRetry()
				continue NewStream
			case ev := <-events:
				pub := ev.(*publication)
				if pub.Retry() > 0 {
					stream.retryDelay.ApplyRetryTime(clampServerDirectedRetry(pub.Retry()))
				}
				stream.lastEventID = pub.lastEventID
				stream.retryDelay.SetGoodSince(time.Now())
				stream.Events <- ev
			case <-stream.closer:
				discardCurrentStream()
				break NewStream
			case <-retryChan:
				var err error
				r, h, err = stream.connect()
				if err != nil {
					r = nil
					h = nil
					if !reportErrorAndMaybeContinue(err) {
						break NewStream
					}
					scheduleRetry()
				}
				continue NewStream
			}
		}
	}

	if stream.Errors != nil {
		close(stream.Errors)
	}
	close(stream.Events)
}

func (stream *Stream) getRetryDelayStrategy() *retryDelayStrategy { //nolint:unused // unused except by tests
	return stream.retryDelay
}

// ActivateProfile switches the currently-active retry profile on this stream. The
// change takes effect immediately.
//
// The profile argument must be one of: (a) a profile registered on this stream via
// StreamOptionRegisterRetryProfile, (b) the stream's effective default profile
// (installed via StreamOptionDefaultRetryProfile, or otherwise synthesized), or
// (c) the package-level DefaultProfile sentinel — treated as a symbolic marker
// meaning "revert to the effective default." Any other *RetryProfile is a silent no-op.
//
// If a healthy-operation reset fires on the next NextRetryDelay call, that reset
// trumps this activation: the active profile will be reverted to the effective
// default. This preserves the intuitive property that a single failure after a long
// healthy period does not push the stream into the newly-activated regime.
//
// Safe to call from any goroutine, including the stream's error handler.
func (stream *Stream) ActivateProfile(profile *RetryProfile) {
	stream.retryDelay.activateProfile(profile)
}

// SetLogger sets the Logger field in a thread-safe manner.
func (stream *Stream) SetLogger(logger Logger) {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	stream.Logger = logger
}

func (stream *Stream) getLogger() Logger {
	stream.mu.RLock()
	defer stream.mu.RUnlock()
	return stream.Logger
}

func clampServerDirectedRetry(hintMs int64) time.Duration {
	maxMs := int64(MaxServerDirectedRetryDelay / time.Millisecond)
	if hintMs > maxMs {
		hintMs = maxMs
	}
	return time.Duration(hintMs) * time.Millisecond
}
