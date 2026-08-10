package eventsource

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// mkRetryDelay constructs a retryDelayStrategy for tests that only exercise the
// (single default curve) legacy shape. Uses legacy stream-option fields, so the
// effective default is synthesized from those values, with any remaining unset
// properties falling through to the library's hard-coded fallbacks during lazy
// overlay resolution.
//
// The helper always addresses every timing knob (including zero values), so
// callers exercising the "explicit zero" leg of the API -- immediate retry for
// baseDelay, disabled healthy-op reset for resetInterval, disabled backoff/
// jitter for the other two -- can construct that state through the helper.
// Tests wanting the "never set / library default" fallback should construct
// streamOptions directly and leave the field nil.
func mkRetryDelay(
	baseDelay time.Duration,
	resetInterval time.Duration,
	backoffMaxDelay time.Duration,
	jitterRatio float64,
	randSeed int64,
) *retryDelayStrategy {
	opts := &streamOptions{
		initialRetry:       &baseDelay,
		backoffMaxDelay:    &backoffMaxDelay,
		jitterRatio:        &jitterRatio,
		retryResetInterval: &resetInterval,
	}
	return newRetryDelayStrategyFromOptions(opts, randSeed)
}

func TestFixedRetryDelay(t *testing.T) {
	d0 := time.Second * 10
	r := mkRetryDelay(d0, 0, 0, 0, 0)
	t0 := time.Now().Add(-time.Minute)
	d1 := r.NextRetryDelay(t0)
	d2 := r.NextRetryDelay(t0.Add(time.Second))
	d3 := r.NextRetryDelay(t0.Add(time.Second * 2))
	assert.Equal(t, d0, d1)
	assert.Equal(t, d0, d2)
	assert.Equal(t, d0, d3)
}

// Parity with pre-refactor behavior: StreamOptionInitialRetry(0) must yield a
// zero retry delay (immediate retry). Under the pointer-based streamOptions,
// nil means "never set" (falls back to DefaultInitialRetry) while &0 means
// "caller explicitly requested immediate retry" -- this test pins the &0 leg.
func TestLegacyInitialRetryZeroYieldsImmediateRetry(t *testing.T) {
	r := mkRetryDelay(0, 0, 0, 0, 0)
	t0 := time.Now().Add(-time.Minute)
	assert.Equal(t, time.Duration(0), r.NextRetryDelay(t0))
	assert.Equal(t, time.Duration(0), r.NextRetryDelay(t0.Add(time.Second)))
}

// The legacy-immediate-retry setting on the synthesized default curve must not
// contaminate a separately-registered curve that has its own baseDelay. This
// guards two invariants:
//  1. resolveCurveProperties for the extended curve resolves against the
//     extended curve's own spec first, then the effective default's spec.
//     A baseDelay explicitly set on the extended curve wins.
//  2. Reverting to the effective default (via activateCurve(DefaultCurve))
//     restores immediate-retry behavior.
func TestLegacyImmediateRetryDoesNotContaminateRegisteredCurve(t *testing.T) {
	zero := time.Duration(0)
	ext := NewRetryCurve(
		RetryCurveBaseDelay(time.Minute*5),
		RetryCurveMaxDelay(time.Hour),
	)
	opts := &streamOptions{
		initialRetry:          &zero,
		registeredRetryCurves: []*RetryCurve{ext},
	}
	r := newRetryDelayStrategyFromOptions(opts, 0)
	t0 := time.Now()

	// Default curve (synth) is active at start: immediate retry.
	assert.Equal(t, time.Duration(0), r.NextRetryDelay(t0), "default curve should retry immediately")

	// Switch to the extended curve: its own baseDelay drives the delay.
	r.activateCurve(ext)
	// retryCount starts at 0 for the extended curve, so applyBackoff yields base*2^0 = 5m.
	assert.Equal(t, time.Minute*5, r.NextRetryDelay(t0), "extended curve should use its own baseDelay")
	// Second attempt on extended: 5m*2 = 10m (still capped well below 1h).
	assert.Equal(t, time.Minute*10, r.NextRetryDelay(t0), "extended curve backoff independent of default")

	// Revert to effective default: immediate retry again.
	r.activateCurve(DefaultCurve)
	assert.Equal(t, time.Duration(0), r.NextRetryDelay(t0), "reverting to default restores immediate retry")
}

func TestBackoffWithoutJitter(t *testing.T) {
	d0 := time.Second * 10
	max := time.Minute
	r := mkRetryDelay(d0, 0, max, 0, 0)
	t0 := time.Now().Add(-time.Minute)
	d1 := r.NextRetryDelay(t0)
	d2 := r.NextRetryDelay(t0.Add(time.Second))
	d3 := r.NextRetryDelay(t0.Add(time.Second * 2))
	d4 := r.NextRetryDelay(t0.Add(time.Second * 3))
	assert.Equal(t, d0, d1)
	assert.Equal(t, d0*2, d2)
	assert.Equal(t, d0*4, d3)
	assert.Equal(t, max, d4)
}

func TestJitterWithoutBackoff(t *testing.T) {
	d0 := time.Second
	seed := int64(1000)
	r := mkRetryDelay(d0, 0, 0, 0.5, seed)
	t0 := time.Now().Add(-time.Minute)
	d1 := r.NextRetryDelay(t0)
	d2 := r.NextRetryDelay(t0.Add(time.Second))
	d3 := r.NextRetryDelay(t0.Add(time.Second * 2))
	assert.Equal(t, time.Duration(985036673), d1) // these are the randomized values we expect from that fixed seed value
	assert.Equal(t, time.Duration(925004285), d2)
	assert.Equal(t, time.Duration(847349921), d3)
}

func TestJitterWithBackoff(t *testing.T) {
	d0 := time.Second
	max := time.Minute
	seed := int64(1000)
	r := mkRetryDelay(d0, 0, max, 0.5, seed)
	t0 := time.Now().Add(-time.Minute)
	d1 := r.NextRetryDelay(t0)
	d2 := r.NextRetryDelay(t0.Add(time.Second))
	d3 := r.NextRetryDelay(t0.Add(time.Second * 2))
	assert.Equal(t, time.Duration(985036673), d1) // these are the randomized values we expect from that fixed seed value
	assert.Equal(t, time.Duration(1425004285), d2)
	assert.Equal(t, time.Duration(3347349921), d3)
}

func TestBackoffResetInterval(t *testing.T) {
	d0 := time.Second * 10
	max := time.Minute
	resetInterval := time.Second * 45
	r := mkRetryDelay(d0, resetInterval, max, 0, 0)
	t0 := time.Now().Add(-time.Minute)
	r.SetGoodSince(t0)

	t1 := t0.Add(time.Second)
	d1 := r.NextRetryDelay(t1)
	assert.Equal(t, d0, d1)

	t2 := t1.Add(d1)
	r.SetGoodSince(t2)

	t3 := t2.Add(time.Second * 10)
	d2 := r.NextRetryDelay(t3)
	assert.Equal(t, d0*2, d2)

	t4 := t3.Add(d2)
	r.SetGoodSince(t4)

	t5 := t4.Add(resetInterval)
	d3 := r.NextRetryDelay(t5)
	assert.Equal(t, d0, d3)
}

func TestBackoffAndJitterWorkWithHighRetryCount(t *testing.T) {
	// This test verifies that we do not get numeric overflow errors due to using a very high exponential
	// backoff number in calculations before it has been pinned to the maximum value. The jitter algorithm
	// uses 63-bit values, so it will fail if there is a time.Duration value greater than about 292 years,
	// which is unlikely to be a desirable backoff interval.
	d0 := time.Second
	max := 365 * 200 * 24 * time.Hour // 200 years
	retryCount := 35                  // 2^35 seconds exceeds a 63-bit count of nanoseconds

	backoff := newDefaultBackoff()
	jitter := newDefaultJitter(1)

	d1 := backoff.applyBackoff(d0, retryCount, max)
	_ = jitter.applyJitter(d1, 0.5)
	// No assertion - the test just needs to not panic.
}

// RETRY spec §1.11.4: a server-directed wait duration MUST NOT exceed 1 hour;
// values above 1 hour are treated as 1 hour. The clamp is applied by
// clampServerDirectedRetry at the SSE wire boundary, before ApplyRetryTime is
// called. This test pins the clamp behavior, including that clamping happens
// before the multiplication by time.Millisecond so extreme int64 wire values
// cannot overflow the Duration.
func TestClampServerDirectedRetry(t *testing.T) {
	// Below the ceiling: pass through unchanged.
	assert.Equal(t, time.Millisecond*500, clampServerDirectedRetry(500))
	assert.Equal(t, time.Second*30, clampServerDirectedRetry(30_000))

	// At the ceiling: passes through unchanged.
	assert.Equal(t, MaxServerDirectedRetryDelay,
		clampServerDirectedRetry(int64(MaxServerDirectedRetryDelay/time.Millisecond)))

	// Above the ceiling: clamped.
	assert.Equal(t, MaxServerDirectedRetryDelay,
		clampServerDirectedRetry(int64(MaxServerDirectedRetryDelay/time.Millisecond)+1))
	assert.Equal(t, MaxServerDirectedRetryDelay, clampServerDirectedRetry(9_223_372_036_854_775))
}

// ApplyRetryTime implements the HTML5 SSE spec's `retry:` directive: it sets the
// stream-level reconnection time. The library maps that to (a) setting each
// registered curve's baseDelayOverride uniformly and (b) resetting each curve's
// backoff-formula counter so the immediate next attempt uses the hinted value
// literally.
func TestApplyRetryTimeUpdatesBaseAndResetsFormulaCounter(t *testing.T) {
	d0 := time.Second
	max := time.Minute
	r := mkRetryDelay(d0, 0, max, 0, 0)

	t0 := time.Now()
	// Progress the counter through a few backoff steps.
	_ = r.NextRetryDelay(t0) // retryCount 0 → 1
	_ = r.NextRetryDelay(t0) // retryCount 1 → 2
	_ = r.NextRetryDelay(t0) // retryCount 2 → 3

	// Server hint: new base delay. Counter must reset so the next attempt uses the
	// hint value literally (matching browser EventSource semantics).
	r.ApplyRetryTime(time.Second * 2)

	d := r.NextRetryDelay(t0)
	assert.Equal(t, time.Second*2, d)
}
