package eventsource

import (
	"net/http"
	"time"
)

type streamOptions struct {
	initialRetry            time.Duration
	httpClient              *http.Client
	lastEventID             string
	logger                  Logger
	useBackoff              bool
	useJitter               bool
	canRetryFirstConnection bool
	maxRetry                time.Duration
	readTimeout             time.Duration
	retryResetInterval      time.Duration
	initialRetryTimeout     time.Duration
}

// StreamOption is a common interface for optional configuration parameters that can be
// used in creating a stream.
type StreamOption interface {
	apply(s *streamOptions) error
}

type readTimeoutOption struct {
	timeout time.Duration
}

func (o readTimeoutOption) apply(s *streamOptions) error {
	s.readTimeout = o.timeout
	return nil
}

// StreamOptionReadTimeout returns an option that sets the read timeout interval for a
// stream when the stream is created. If the stream does not receive new data within this
// length of time, it will restart the connection.
//
// By default, there is no read timeout.
func StreamOptionReadTimeout(timeout time.Duration) StreamOption {
	return readTimeoutOption{timeout: timeout}
}

type initialRetryOption struct {
	retry time.Duration
}

func (o initialRetryOption) apply(s *streamOptions) error {
	s.initialRetry = o.retry
	return nil
}

// StreamOptionInitialRetry returns an option that sets the initial retry delay for a
// stream when the stream is created.
//
// This delay will be used the first time the stream has to be restarted; the interval will
// increase exponentially on subsequent reconnections. Each time, there will also be a
// pseudo-random jitter so that the actual value may be up to 50% less. So, for instance,
// if you set the initial delay to 1 second, the first reconnection will use a delay between
// 0.5s and 1s inclusive, and subsequent reconnections will be 1s-2s, 2s-4s, etc.
//
// The default value is DefaultInitialRetry. In a future version, this value may change, so
// if you need a specific value it is best to set it explicitly.
func StreamOptionInitialRetry(retry time.Duration) StreamOption {
	return initialRetryOption{retry: retry}
}

type maxRetryOption struct {
	maxRetry time.Duration
}

func (o maxRetryOption) apply(s *streamOptions) error {
	s.maxRetry = o.maxRetry
	return nil
}

type useBackoffOption struct {
	value bool
}

func (o useBackoffOption) apply(s *streamOptions) error {
	s.useBackoff = o.value
	return nil
}

// StreamOptionUseBackoff returns an option that determines whether to use an exponential
// backoff for reconnection delays.
//
// If enabled, the retry delay interval will be doubled (not counting jitter - see
// StreamOptionUseJitter) for consecutive stream reconnections, subject to the limit
// specified by StreamOptionMaxRetry.
//
// For consistency with earlier versions, this is currently disabled (false) by default. In
// a future version it will default to enabled (true), so if you do not want backoff
// behavior you should explicitly set it to false. It is recommended to use both backoff
// and jitter, to avoid "thundering herd" behavior in the case of a server outage.
func StreamOptionUseBackoff(useBackoff bool) StreamOption {
	return useBackoffOption{useBackoff}
}

type canRetryFirstConnectionOption struct {
	initialRetryTimeout time.Duration
}

func (o canRetryFirstConnectionOption) apply(s *streamOptions) error {
	s.initialRetryTimeout = o.initialRetryTimeout
	return nil
}

// StreamOptionCanRetryFirstConnection returns an option that determines whether to apply
// retry behavior to the first connection attempt for the stream.
//
// If the timeout is nonzero, an initial connection failure when subscribing will not cause an
// error result, but will trigger the same retry logic as if an existing connection had failed.
// The stream constructor will not return until a connection has been made, or until the
// specified timeout expires, if the timeout is positive; if the timeout is negative, it
// will continue retrying indefinitely.
//
// The default value is zero: an initial connection failure will not be retried.
func StreamOptionCanRetryFirstConnection(initialRetryTimeout time.Duration) StreamOption {
	return canRetryFirstConnectionOption{initialRetryTimeout}
}

type useJitterOption struct {
	value bool
}

func (o useJitterOption) apply(s *streamOptions) error {
	s.useJitter = o.value
	return nil
}

// StreamOptionUseJitter returns an option that determines whether to use a randomized
// jitter for reconnection delays.
//
// If enabled, then whatever retry delay interval would otherwise be used is randomly
// decreased by up to 50%.
//
// For consistency with earlier versions, this is currently disabled (false) by default. In
// a future version it will default to enabled (true), so if you do not want jitter you
// should explicitly set it to false. It is recommended to use both backoff and jitter, to
// avoid "thundering herd" behavior in the case of a server outage.
func StreamOptionUseJitter(useJitter bool) StreamOption {
	return useJitterOption{useJitter}
}

// StreamOptionMaxRetry returns an option that sets the maximum retry delay for a
// stream when the stream is created. This is only relevant if backoff is enabled (see
// StreamOptionUseBackoff).
//
// If the stream has to be restarted multiple times, the retry delay interval will increase
// using an exponential backoff interval but will never be longer than this maximum.
//
// The default value is DefaultMaxRetry.
func StreamOptionMaxRetry(maxRetry time.Duration) StreamOption {
	return maxRetryOption{maxRetry: maxRetry}
}

type retryResetIntervalOption struct {
	retryResetInterval time.Duration
}

func (o retryResetIntervalOption) apply(s *streamOptions) error {
	s.retryResetInterval = o.retryResetInterval
	return nil
}

// StreamOptionRetryResetInterval returns an option that sets the minimum amount of time that a
// connection must stay open before the Stream resets its backoff delay. This is only relevant if
// backoff is enabled (see StreamOptionUseBackoff).
//
// If a connection fails before the threshold has elapsed, the delay before reconnecting will be
// greater than the last delay; if it fails after the threshold, the delay will start over at the
// the initial minimum value. This prevents long delays from occurring on connections that are only
// rarely restarted.
//
// The default value is DefaultRetryResetInterval.
func StreamOptionRetryResetInterval(retryResetInterval time.Duration) StreamOption {
	return retryResetIntervalOption{retryResetInterval: retryResetInterval}
}

type lastEventIDOption struct {
	lastEventID string
}

func (o lastEventIDOption) apply(s *streamOptions) error {
	s.lastEventID = o.lastEventID
	return nil
}

// StreamOptionLastEventID returns an option that sets the initial last event ID for a
// stream when the stream is created. If specified, this value will be sent to the server
// in case it can replay missed events.
func StreamOptionLastEventID(lastEventID string) StreamOption {
	return lastEventIDOption{lastEventID: lastEventID}
}

type httpClientOption struct {
	client *http.Client
}

func (o httpClientOption) apply(s *streamOptions) error {
	if o.client != nil {
		s.httpClient = o.client
	}
	return nil
}

// StreamOptionHTTPClient returns an option that overrides the default HTTP client used by
// a stream when the stream is created.
func StreamOptionHTTPClient(client *http.Client) StreamOption {
	return httpClientOption{client: client}
}

type loggerOption struct {
	logger Logger
}

func (o loggerOption) apply(s *streamOptions) error {
	s.logger = o.logger
	return nil
}

// StreamOptionLogger returns an option that sets the logger for a stream when the stream
// is created (to change it later, you can use SetLogger). By default, there is no logger.
func StreamOptionLogger(logger Logger) StreamOption {
	return loggerOption{logger: logger}
}

const (
	// DefaultInitialRetry is the default value for StreamOptionalInitialRetry.
	DefaultInitialRetry = time.Second * 3
	// DefaultMaxRetry is the default value for StreamOptionMaxRetry.
	DefaultMaxRetry = time.Second * 30
	// DefaultRetryResetInterval is the default value for StreamOptionRetryResetInterval.
	DefaultRetryResetInterval = time.Second * 60
)
