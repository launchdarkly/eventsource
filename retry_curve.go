package eventsource

import "time"

// RetryCurve is an opaque handle to a retry curve. Construct one via NewRetryCurve,
// install it as the stream's default via StreamOptionDefaultRetryCurve, or register
// it as an additional curve via StreamOptionRegisterRetryCurve, and activate it at
// runtime via Stream.ActivateCurve.
//
// Two different NewRetryCurve calls with identical options yield two different
// curves (identity is by pointer, not by parameter equality).
//
// All three per-curve properties (base delay, max delay, jitter) may be left unset
// by omitting the corresponding RetryCurveOption. Unset properties inherit from the
// stream's effective default at delay-computation time.
//
// Reset interval (the healthy-operation threshold for returning to the effective
// default) is a stream-level concept rather than a per-curve one; configure it via
// StreamOptionRetryResetInterval. Whichever curve is currently active, the reset
// check uses the stream's single reset interval.
//
// A server-directed `retry:` hint on the SSE wire is also stream-wide per HTML5
// semantics: it overrides every registered curve's base delay for subsequent
// attempts (clamped to MaxServerDirectedRetryDelay). The curve's declared maxDelay
// ceiling is untouched — the "always retry within maxDelay" property that motivates
// registering an extended curve is preserved even after a hint.
type RetryCurve struct {
	baseDelay *time.Duration
	maxDelay  *time.Duration
	jitter    *float64
}

// RetryCurveOption is a common interface for configuration parameters that can be
// used when creating a RetryCurve via NewRetryCurve. This mirrors the interface-based
// StreamOption pattern used elsewhere in this package.
type RetryCurveOption interface {
	apply(*RetryCurve) error
}

type retryCurveBaseDelayOption struct{ v time.Duration }

func (o retryCurveBaseDelayOption) apply(c *RetryCurve) error {
	c.baseDelay = &o.v
	return nil
}

// RetryCurveBaseDelay returns an option that sets the base delay for a RetryCurve.
// Without this option, base delay is inherited from the stream's effective default
// at delay-computation time.
func RetryCurveBaseDelay(base time.Duration) RetryCurveOption {
	return retryCurveBaseDelayOption{v: base}
}

type retryCurveMaxDelayOption struct{ v time.Duration }

func (o retryCurveMaxDelayOption) apply(c *RetryCurve) error {
	c.maxDelay = &o.v
	return nil
}

// RetryCurveMaxDelay returns an option that sets the maximum delay (backoff ceiling)
// for a RetryCurve. A max delay of zero means "no backoff" — successive retries all
// use the base delay. Without this option, max delay is inherited from the stream's
// effective default at delay-computation time.
func RetryCurveMaxDelay(maxDelay time.Duration) RetryCurveOption {
	return retryCurveMaxDelayOption{v: maxDelay}
}

type retryCurveJitterOption struct{ v float64 }

func (o retryCurveJitterOption) apply(c *RetryCurve) error {
	c.jitter = &o.v
	return nil
}

// RetryCurveJitter returns an option that sets the jitter ratio (range 0.0 - 1.0)
// for a RetryCurve. A jitter ratio of zero means "no jitter." Without this option,
// jitter is inherited from the stream's effective default at delay-computation time.
func RetryCurveJitter(ratio float64) RetryCurveOption {
	return retryCurveJitterOption{v: ratio}
}

// NewRetryCurve constructs a RetryCurve from the provided options. Properties not
// specified via options are inherited from the stream's effective default. The library
// never mutates the fields set here; the returned pointer is a stable, opaque handle.
func NewRetryCurve(options ...RetryCurveOption) *RetryCurve {
	c := &RetryCurve{}
	for _, o := range options {
		_ = o.apply(c)
	}
	return c
}

// DefaultCurve is a package-level sentinel *RetryCurve meaning "revert to
// this stream's effective default." Pass it to Stream.ActivateCurve when you want
// to explicitly revert without holding a reference to the default handle directly
// (for example, when the default was configured via legacy stream options rather
// than via StreamOptionDefaultRetryCurve).
//
// The sentinel has no data — its purpose is pointer identity only. Do not pass it
// to StreamOption wrappers; use NewRetryCurve for that.
var DefaultCurve = &RetryCurve{} //nolint:gochecknoglobals
