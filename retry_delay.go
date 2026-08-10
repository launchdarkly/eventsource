package eventsource

import (
	"math"
	"math/rand"
	"sync"
	"time"
)

// Encapsulation of the streaming retry-timing behavior. Supports one or more
// RetryCurves registered on a stream at subscribe time, with a single
// currently-active curve driving delay computation. See retry_curve.go for the
// user-facing type; this file holds the internal machinery.
//
// The library uses lazy resolution at delay-computation time, using the active curve's spec,
// falling through to the effective default's spec and then to hard-coded
// fallbacks.
//
// Per-curve runtime state carries only two things: `retryCount` (the backoff
// formula counter n, per RETRY spec) and a nullable `baseDelayOverride` that
// captures server-directed `retry:` hints.
//
// `baseDelayOverride` is NOT cleared on healthy-op reset — it persists until the
// server sends another `retry:` hint or the Stream is closed, matching the HTML5
// SSE spec's "reconnection time is set until updated" semantic.
type retryDelayStrategy struct {
	curves           map[*RetryCurve]*perCurveState
	effectiveDefault *RetryCurve
	active           *RetryCurve
	resetInterval    time.Duration
	goodSince        time.Time // nonzero only if the state is currently "good"
	backoff          backoffStrategy
	jitter           jitterStrategy
	lock             sync.Mutex
}

// perCurveState carries the per-stream mutable runtime state for one registered
// curve: its backoff-formula counter and any server-directed base-delay override.
// The curve's own spec fields (baseDelay/maxDelay/jitter) live on the *RetryCurve
// key itself and are read only.
type perCurveState struct {
	retryCount        int
	baseDelayOverride *time.Duration
}

// Abstraction for backoff delay behavior. The per-attempt effective maxDelay is
// passed in per call so a single strategy instance can serve multiple retry curves
// with different ceilings.
type backoffStrategy interface {
	applyBackoff(baseDelay time.Duration, retryCount int, maxDelay time.Duration) time.Duration
}

// Abstraction for delay jitter behavior. The per-attempt effective ratio is passed
// in per call so a single strategy instance can serve multiple retry curves with
// different jitter ratios.
type jitterStrategy interface {
	applyJitter(computedDelay time.Duration, ratio float64) time.Duration
}

type defaultBackoffStrategy struct{}

// Creates the default implementation of exponential backoff, which doubles the delay each time up to
// the specified maximum.
//
// If a resetInterval was specified for the retryDelayStrategy, and the system has been in a "good"
// state for at least that long, the delay is reset back to the base. This avoids perpetually increasing
// delays in a situation where failures are rare).
func newDefaultBackoff() backoffStrategy {
	return defaultBackoffStrategy{}
}

func (s defaultBackoffStrategy) applyBackoff(
	baseDelay time.Duration, retryCount int, maxDelay time.Duration,
) time.Duration {
	d := math.Min(float64(baseDelay)*math.Pow(2, float64(retryCount)), float64(maxDelay))
	return time.Duration(d)
}

type defaultJitterStrategy struct {
	random *rand.Rand
}

// Creates the default implementation of jitter, which subtracts a pseudo-random amount from each delay.
func newDefaultJitter(randSeed int64) jitterStrategy {
	if randSeed <= 0 {
		randSeed = time.Now().UnixNano()
	}
	//nolint:gosec // This isn't a cryptographic use-case, weak RNG is acceptable
	return &defaultJitterStrategy{random: rand.New(rand.NewSource(randSeed))}
}

func (s *defaultJitterStrategy) applyJitter(computedDelay time.Duration, ratio float64) time.Duration {
	if ratio > 1.0 {
		ratio = 1.0
	}
	span := int64(float64(computedDelay) * ratio)
	if span <= 0 {
		return computedDelay
	}
	jitter := time.Duration(s.random.Int63n(span))
	return computedDelay - jitter
}

// newRetryDelayStrategyFromOptions constructs a retryDelayStrategy from resolved
// streamOptions.
func newRetryDelayStrategyFromOptions(opts *streamOptions, randSeed int64) *retryDelayStrategy {
	// Resolve the effective default curve.
	effectiveDefault := opts.defaultRetryCurve
	if effectiveDefault == nil {
		// Synthesize from legacy stream options. Non-nil legacy pointers are
		// always propagated (including zero values); nil means the caller never
		// set the option and resolveCurveProperties should fall through to the
		// hard-coded fallback. See streamOptions doc for the semantics of zero.
		synth := &RetryCurve{}
		if opts.initialRetry != nil {
			v := *opts.initialRetry
			synth.baseDelay = &v
		}
		if opts.backoffMaxDelay != nil {
			v := *opts.backoffMaxDelay
			synth.maxDelay = &v
		}
		if opts.jitterRatio != nil {
			v := *opts.jitterRatio
			synth.jitter = &v
		}
		effectiveDefault = synth
	}

	// Build the per-curve runtime state map: effective default + any additional
	// registered curves.
	curves := map[*RetryCurve]*perCurveState{
		effectiveDefault: {},
	}
	for _, c := range opts.registeredRetryCurves {
		if c == nil || c == effectiveDefault {
			continue
		}
		if _, dup := curves[c]; dup {
			continue
		}
		curves[c] = &perCurveState{}
	}

	resetInterval := DefaultRetryResetInterval
	if opts.retryResetInterval != nil {
		resetInterval = *opts.retryResetInterval
	}

	return &retryDelayStrategy{
		curves:           curves,
		effectiveDefault: effectiveDefault,
		active:           effectiveDefault,
		resetInterval:    resetInterval,
		backoff:          newDefaultBackoff(),
		jitter:           newDefaultJitter(randSeed),
	}
}

// firstNonNil walks the two curve layers (primary then secondary) and returns the
// value of the first whose selected field is non-nil. Falls back to `fallback` if
// neither has the field set (or is itself nil).
func firstNonNil[T any](selector func(*RetryCurve) *T, primary, secondary *RetryCurve, fallback T) T {
	if primary != nil {
		if v := selector(primary); v != nil {
			return *v
		}
	}
	if secondary != nil {
		if v := selector(secondary); v != nil {
			return *v
		}
	}
	return fallback
}

// resolveCurveProperties computes the effective (baseDelay, maxDelay, jitter) for the given
// curve handle by walking the overlay stack: curve.spec → effectiveDefault.spec →
// hard-coded fallbacks.
//
// Caller must hold r.lock.
func (r *retryDelayStrategy) resolveCurveProperties(
	c *RetryCurve,
) (baseDelay, maxDelay time.Duration, jitter float64) {
	baseDelay = firstNonNil(
		func(c *RetryCurve) *time.Duration { return c.baseDelay },
		c, r.effectiveDefault, DefaultInitialRetry,
	)
	maxDelay = firstNonNil(
		func(c *RetryCurve) *time.Duration { return c.maxDelay },
		c, r.effectiveDefault, time.Duration(0),
	)
	jitter = firstNonNil(
		func(c *RetryCurve) *float64 { return c.jitter },
		c, r.effectiveDefault, float64(0),
	)
	return
}

// NextRetryDelay computes the next retry interval and marks the current state as "bad".
//
// Order of operations:
//  1. Check the healthy-operation reset condition (goodSince non-zero AND elapsed >=
//     resetInterval). If satisfied, apply reset: for each curve set retryCount = 0
//     (do NOT touch baseDelayOverride — persistent per SSE spec);
//     active = effective default.
//  2. Clear goodSince
//  3. Resolve the active curve's effective properties; baseDelayOverride, if set, wins.
//  4. Compute delay = min(effectiveBase * 2^retryCount, effectiveMax) with jitter.
//  5. Increment active.retryCount.
//
// currentTime is passed as a parameter rather than computed internally to keep tests
// deterministic.
func (r *retryDelayStrategy) NextRetryDelay(currentTime time.Time) time.Duration {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.goodSince.IsZero() && r.resetInterval > 0 && (currentTime.Sub(r.goodSince) >= r.resetInterval) {
		for _, c := range r.curves {
			c.retryCount = 0
			// baseDelayOverride is NOT cleared intentionally
		}
		r.active = r.effectiveDefault
	}
	r.goodSince = time.Time{}

	activeState := r.curves[r.active]

	effectiveBase, effectiveMax, effectiveJitter := r.resolveCurveProperties(r.active)
	if activeState.baseDelayOverride != nil {
		effectiveBase = *activeState.baseDelayOverride
	}

	delay := effectiveBase
	if effectiveMax > 0 {
		delay = r.backoff.applyBackoff(effectiveBase, activeState.retryCount, effectiveMax)
	}
	if effectiveJitter > 0 {
		delay = r.jitter.applyJitter(delay, effectiveJitter)
	}

	activeState.retryCount++

	return delay
}

// SetGoodSince marks the current state as "good" and records the time. See comments on the backoff type.
func (r *retryDelayStrategy) SetGoodSince(goodSince time.Time) {
	r.lock.Lock()
	r.goodSince = goodSince
	r.lock.Unlock()
}

// ApplyRetryTime records a server-directed reconnection-time hint received via the
// SSE `retry:` field. It sets every registered curve's baseDelayOverride to the
// given duration and zeroes every curve's retryCount (the backoff-formula counter),
// so the immediate next attempt in whatever regime is active uses the hinted value.
// The active curve is NOT changed.
func (r *retryDelayStrategy) ApplyRetryTime(hint time.Duration) {
	r.lock.Lock()
	defer r.lock.Unlock()
	for _, c := range r.curves {
		v := hint
		c.baseDelayOverride = &v
		c.retryCount = 0
	}
}

// activateCurve switches the currently-active curve immediately. Silent no-op if
// the curve is nil, unregistered, or already active.
//
// Does NOT reset the newly-activated curve's retryCount — each curve's counter
// retains its progression across activations. Does NOT touch any curve's
// baseDelayOverride.
//
// If a healthy-operation reset fires on the next NextRetryDelay call, that reset
// trumps this activation: active will be reverted to the effective default.
func (r *retryDelayStrategy) activateCurve(c *RetryCurve) {
	r.lock.Lock()
	defer r.lock.Unlock()
	if c == nil {
		return
	}
	if c == DefaultCurve {
		r.active = r.effectiveDefault
		return
	}
	if _, ok := r.curves[c]; !ok {
		return
	}
	r.active = c
}

// activeCurve returns the currently-active *RetryCurve — a real curve pointer
// registered on this stream, never the DefaultCurve sentinel.
func (r *retryDelayStrategy) activeCurve() *RetryCurve { //nolint:unused // used only in tests
	r.lock.Lock()
	defer r.lock.Unlock()
	return r.active
}

func (r *retryDelayStrategy) hasJitter() bool { //nolint:unused // used only in tests
	r.lock.Lock()
	defer r.lock.Unlock()
	_, _, j := r.resolveCurveProperties(r.active)
	return j > 0
}
