package eventsource

import "time"

// RetryProfile is an opaque handle to a retry profile. Construct one via NewRetryProfile,
// install it as the stream's default via StreamOptionDefaultRetryProfile, or register
// it as an additional profile via StreamOptionRegisterRetryProfile, and activate it at
// runtime via Stream.ActivateProfile.
//
// Two different NewRetryProfile calls with identical options yield two different
// profiles (identity is by pointer, not by parameter equality).
//
// All three per-profile properties (base delay, max delay, jitter) may be left unset
// by omitting the corresponding RetryProfileOption. Unset properties inherit from the
// stream's effective default at delay-computation time.
//
// Reset interval (the healthy-operation threshold for returning to the effective
// default) applies per eventsource instance rather than per-profile; configure it
// via StreamOptionRetryResetInterval. Whichever profile is currently active, the
// reset check uses the single reset interval configured for the instance.
//
// A server-directed `retry:` hint on the SSE wire also applies per eventsource
// instance, matching the WHATWG HTML Living Standard's EventSource
// reconnection-time semantics: the hint overrides every registered profile's base
// delay for subsequent attempts (clamped to MaxServerDirectedRetryDelay). The
// profile's declared maxDelay ceiling is untouched — the "always retry within
// maxDelay" property that motivates registering an extended profile is preserved
// even after a hint.
type RetryProfile struct {
	baseDelay *time.Duration
	maxDelay  *time.Duration
	jitter    *float64
}

// RetryProfileOption is a common interface for configuration parameters that can be
// used when creating a RetryProfile via NewRetryProfile. This mirrors the interface-based
// StreamOption pattern used elsewhere in this package.
type RetryProfileOption interface {
	apply(*RetryProfile) error
}

type retryProfileBaseDelayOption struct{ v time.Duration }

func (o retryProfileBaseDelayOption) apply(c *RetryProfile) error {
	c.baseDelay = &o.v
	return nil
}

// RetryProfileBaseDelay returns an option that sets the base delay for a RetryProfile.
// Without this option, base delay is inherited from the stream's effective default
// at delay-computation time.
func RetryProfileBaseDelay(base time.Duration) RetryProfileOption {
	return retryProfileBaseDelayOption{v: base}
}

type retryProfileMaxDelayOption struct{ v time.Duration }

func (o retryProfileMaxDelayOption) apply(c *RetryProfile) error {
	c.maxDelay = &o.v
	return nil
}

// RetryProfileMaxDelay returns an option that sets the maximum delay (backoff ceiling)
// for a RetryProfile. A max delay of zero means "no backoff" — successive retries all
// use the base delay. Without this option, max delay is inherited from the stream's
// effective default at delay-computation time.
func RetryProfileMaxDelay(maxDelay time.Duration) RetryProfileOption {
	return retryProfileMaxDelayOption{v: maxDelay}
}

type retryProfileJitterOption struct{ v float64 }

func (o retryProfileJitterOption) apply(c *RetryProfile) error {
	c.jitter = &o.v
	return nil
}

// RetryProfileJitter returns an option that sets the jitter ratio (range 0.0 - 1.0)
// for a RetryProfile. A jitter ratio of zero means "no jitter." Without this option,
// jitter is inherited from the stream's effective default at delay-computation time.
func RetryProfileJitter(ratio float64) RetryProfileOption {
	return retryProfileJitterOption{v: ratio}
}

// NewRetryProfile constructs a RetryProfile from the provided options. Properties not
// specified via options are inherited from the stream's effective default. The library
// never mutates the fields set here; the returned pointer is a stable, opaque handle.
func NewRetryProfile(options ...RetryProfileOption) *RetryProfile {
	c := &RetryProfile{}
	for _, o := range options {
		_ = o.apply(c)
	}
	return c
}

// DefaultProfile is a package-level sentinel *RetryProfile meaning "revert to
// this stream's effective default." Pass it to Stream.ActivateProfile when you want
// to explicitly revert without holding a reference to the default handle directly
// (for example, when the default was configured via legacy stream options rather
// than via StreamOptionDefaultRetryProfile).
//
// The sentinel has no data — its purpose is pointer identity only. Do not pass it
// to StreamOption wrappers; use NewRetryProfile for that.
var DefaultProfile = &RetryProfile{} //nolint:gochecknoglobals
