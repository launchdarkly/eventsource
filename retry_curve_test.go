package eventsource

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// mkRetryDelayWithCurves builds a retryDelayStrategy from streamOptions that
// include a default curve, any number of registered curves, and an optional
// stream-level reset interval. A resetInterval of 0 defers to the library default
// (DefaultRetryResetInterval).
func mkRetryDelayWithCurves(
	defaultCurve *RetryCurve,
	registered []*RetryCurve,
	resetInterval time.Duration,
	randSeed int64,
) *retryDelayStrategy {
	opts := &streamOptions{
		defaultRetryCurve:     defaultCurve,
		registeredRetryCurves: registered,
		retryResetInterval:    &resetInterval,
	}
	return newRetryDelayStrategyFromOptions(opts, randSeed)
}

func TestActiveCurveReturnsEffectiveDefaultAtStart(t *testing.T) {
	def := NewRetryCurve(
		RetryCurveBaseDelay(time.Second),
		RetryCurveMaxDelay(time.Second*30),
	)
	r := mkRetryDelayWithCurves(def, nil, 0, 0)
	assert.Same(t, def, r.activeCurve())
}

func TestActivateCurveSwitchesToRegisteredCurveImmediately(t *testing.T) {
	def := NewRetryCurve(RetryCurveBaseDelay(time.Second))
	ext := NewRetryCurve(RetryCurveBaseDelay(time.Minute * 5))
	r := mkRetryDelayWithCurves(def, []*RetryCurve{ext}, 0, 0)

	r.activateCurve(ext)
	// Activation is immediate; the observer sees the change right away.
	assert.Same(t, ext, r.activeCurve())

	d := r.NextRetryDelay(time.Now())
	assert.Same(t, ext, r.activeCurve())
	assert.Equal(t, time.Minute*5, d)
}

func TestActivateCurveUnregisteredIsSilentNoOp(t *testing.T) {
	def := NewRetryCurve(RetryCurveBaseDelay(time.Second))
	ext := NewRetryCurve(RetryCurveBaseDelay(time.Minute * 5))
	unrelated := NewRetryCurve(RetryCurveBaseDelay(time.Minute))
	r := mkRetryDelayWithCurves(def, []*RetryCurve{ext}, 0, 0)

	r.activateCurve(unrelated)
	// Unrelated curve silently ignored; active still points at default.
	assert.Same(t, def, r.activeCurve())
	d := r.NextRetryDelay(time.Now())
	assert.Equal(t, time.Second, d)
}

func TestActivateCurveDefaultSentinelRevertsToEffectiveDefault(t *testing.T) {
	def := NewRetryCurve(RetryCurveBaseDelay(time.Second))
	ext := NewRetryCurve(RetryCurveBaseDelay(time.Minute * 5))
	r := mkRetryDelayWithCurves(def, []*RetryCurve{ext}, 0, 0)

	// Move to extended immediately.
	r.activateCurve(ext)
	assert.Same(t, ext, r.activeCurve())

	// Sentinel means "revert to effective default."
	r.activateCurve(DefaultCurve)
	assert.Same(t, def, r.activeCurve())
	d := r.NextRetryDelay(time.Now())
	assert.Equal(t, time.Second, d)
}

func TestPerCurveRetryCountIsIndependent(t *testing.T) {
	def := NewRetryCurve(
		RetryCurveBaseDelay(time.Second),
		RetryCurveMaxDelay(time.Minute),
	)
	ext := NewRetryCurve(
		RetryCurveBaseDelay(time.Minute*5),
		RetryCurveMaxDelay(time.Hour),
	)
	r := mkRetryDelayWithCurves(def, []*RetryCurve{ext}, 0, 0)

	t0 := time.Now()
	// Progress default's counter to 3 attempts.
	_ = r.NextRetryDelay(t0) // 1s (counter 0 → 1)
	_ = r.NextRetryDelay(t0) // 2s (counter 1 → 2)
	_ = r.NextRetryDelay(t0) // 4s (counter 2 → 3)

	// Switch to extended. Extended's counter is still 0; first extended delay is baseDelay.
	r.activateCurve(ext)
	d := r.NextRetryDelay(t0)
	assert.Equal(t, time.Minute*5, d)

	// Second extended attempt: 5min * 2 = 10min.
	d = r.NextRetryDelay(t0)
	assert.Equal(t, time.Minute*10, d)

	// Revert to default: counter picks up where it left off (was 3, next attempt uses 3 → 8s).
	r.activateCurve(DefaultCurve)
	d = r.NextRetryDelay(t0)
	assert.Equal(t, time.Second*8, d)
}

// Reset trumps a same-cycle activation: a caller who activated an alternative curve
// before NextRetryDelay observes the reset condition sees the reset override their
// choice. This gives the natural semantic that a transient unexpected failure after
// a long healthy period does not push the SDK into the alternative regime.
func TestResetTrumpsActivation(t *testing.T) {
	def := NewRetryCurve(
		RetryCurveBaseDelay(time.Second),
		RetryCurveMaxDelay(time.Minute),
	)
	ext := NewRetryCurve(
		RetryCurveBaseDelay(time.Minute*5),
		RetryCurveMaxDelay(time.Hour),
	)
	resetInterval := time.Second * 30
	r := mkRetryDelayWithCurves(def, []*RetryCurve{ext}, resetInterval, 0)

	t0 := time.Now()
	// Establish a healthy period longer than resetInterval.
	r.SetGoodSince(t0)

	// Caller activates ext (immediate).
	r.activateCurve(ext)
	assert.Same(t, ext, r.activeCurve())

	// NextRetryDelay fires with a currentTime past the reset threshold. Reset trumps
	// activation: active reverts to default, counters zeroed, delay uses default.baseDelay.
	d := r.NextRetryDelay(t0.Add(resetInterval))
	assert.Same(t, def, r.activeCurve())
	assert.Equal(t, time.Second, d)
}

// Persistent failures — after reset trumps the first activation, subsequent activations
// (with no intervening healthy period long enough to reset) succeed and drive extended
// regime progression.
func TestPersistentFailuresRampIntoExtended(t *testing.T) {
	def := NewRetryCurve(
		RetryCurveBaseDelay(time.Second),
		RetryCurveMaxDelay(time.Minute),
	)
	ext := NewRetryCurve(
		RetryCurveBaseDelay(time.Minute*5),
		RetryCurveMaxDelay(time.Hour),
	)
	resetInterval := time.Second * 30
	r := mkRetryDelayWithCurves(def, []*RetryCurve{ext}, resetInterval, 0)

	t0 := time.Now()
	r.SetGoodSince(t0)

	// Failure #1: after healthy period. Reset trumps activation.
	r.activateCurve(ext)
	d := r.NextRetryDelay(t0.Add(resetInterval))
	assert.Equal(t, time.Second, d) // reset fired, default active

	// Failure #2 shortly after: no healthy period, no reset.
	r.activateCurve(ext)
	d = r.NextRetryDelay(t0.Add(resetInterval + time.Millisecond))
	assert.Same(t, ext, r.activeCurve())
	assert.Equal(t, time.Minute*5, d)

	// Failure #3: continues extended progression.
	r.activateCurve(ext) // no-op, ext already active
	d = r.NextRetryDelay(t0.Add(resetInterval + time.Millisecond*2))
	assert.Equal(t, time.Minute*10, d)
}

func TestResetZerosAllCurvesCounters(t *testing.T) {
	def := NewRetryCurve(
		RetryCurveBaseDelay(time.Second),
		RetryCurveMaxDelay(time.Minute),
	)
	ext := NewRetryCurve(
		RetryCurveBaseDelay(time.Minute*5),
		RetryCurveMaxDelay(time.Hour),
	)
	resetInterval := time.Second * 30
	r := mkRetryDelayWithCurves(def, []*RetryCurve{ext}, resetInterval, 0)

	t0 := time.Now()

	// Progress default.
	_ = r.NextRetryDelay(t0)
	_ = r.NextRetryDelay(t0)

	// Progress extended.
	r.activateCurve(ext)
	_ = r.NextRetryDelay(t0)
	_ = r.NextRetryDelay(t0)

	// Return to default, healthy period elapses, then failure triggers reset.
	r.activateCurve(DefaultCurve)
	r.SetGoodSince(t0)
	d := r.NextRetryDelay(t0.Add(resetInterval))
	assert.Equal(t, time.Second, d) // default's counter zeroed

	// Confirm extended's counter was also zeroed.
	r.activateCurve(ext)
	d = r.NextRetryDelay(t0.Add(resetInterval + time.Millisecond))
	assert.Equal(t, time.Minute*5, d)
}

// ApplyRetryTime is stream-level per the SSE spec's "reconnection time" semantics.
// It updates every registered curve's baseDelay uniformly and resets each
// curve's formula counter, so subsequent attempts in any regime start from the
// hinted base. Each regime still enforces its own maxDelay ceiling.
func TestApplyRetryTimeUpdatesAllCurves(t *testing.T) {
	def := NewRetryCurve(
		RetryCurveBaseDelay(time.Second),
		RetryCurveMaxDelay(time.Second*30),
	)
	ext := NewRetryCurve(
		RetryCurveBaseDelay(time.Minute*5),
		RetryCurveMaxDelay(time.Hour),
	)
	r := mkRetryDelayWithCurves(def, []*RetryCurve{ext}, 0, 0)

	t0 := time.Now()

	// Progress default's counter so we can verify the reset that comes with the
	// hint.
	_ = r.NextRetryDelay(t0)
	_ = r.NextRetryDelay(t0)

	// Server hint: 500ms. All curves' baseDelays should become 500ms and their
	// formula counters should be zeroed.
	r.ApplyRetryTime(time.Millisecond * 500)

	// Default: first attempt after hint uses the hinted value literally.
	d := r.NextRetryDelay(t0)
	assert.Equal(t, time.Millisecond*500, d)

	// Switch to extended. Extended's baseDelay is now also 500ms (per stream-level
	// mutation) but its maxDelay ceiling of 1hr still applies.
	r.activateCurve(ext)
	d = r.NextRetryDelay(t0)
	assert.Equal(t, time.Millisecond*500, d)
}

// Base-delay mutations by ApplyRetryTime persist across healthy-operation reset —
// matching the SSE spec's "reconnection time is set until updated" semantic.
// Reset zeros counters and reverts the active pointer, but does not touch the
// mutated base delays.
func TestBaseDelayMutationPersistsAcrossReset(t *testing.T) {
	def := NewRetryCurve(RetryCurveBaseDelay(time.Second))
	resetInterval := time.Second * 30
	r := mkRetryDelayWithCurves(def, nil, resetInterval, 0)

	t0 := time.Now()

	// Server hints 500ms.
	r.ApplyRetryTime(time.Millisecond * 500)
	d := r.NextRetryDelay(t0)
	assert.Equal(t, time.Millisecond*500, d)

	// Enter healthy state, then trigger reset.
	r.SetGoodSince(t0.Add(time.Second))
	d = r.NextRetryDelay(t0.Add(time.Second + resetInterval))

	// Reset zeroed the counter, but the 500ms baseDelay persists.
	assert.Equal(t, time.Millisecond*500, d)
}

func TestOverlayInheritanceFillsUnsetPropertiesFromDefault(t *testing.T) {
	def := NewRetryCurve(
		RetryCurveBaseDelay(time.Second),
		RetryCurveMaxDelay(time.Minute),
		RetryCurveJitter(0),
	)
	// ext specifies only baseDelay + maxDelay; jitter should inherit from default (0).
	ext := NewRetryCurve(
		RetryCurveBaseDelay(time.Minute*5),
		RetryCurveMaxDelay(time.Hour),
	)
	r := mkRetryDelayWithCurves(def, []*RetryCurve{ext}, 0, 0)

	// Verify extended's resolved values by observing behavior. First extended attempt
	// yields 5min (baseDelay) with no jitter (inherited 0).
	r.activateCurve(ext)
	d := r.NextRetryDelay(time.Now())
	assert.Equal(t, time.Minute*5, d)
}

func TestNilRegisteredCurvesAreIgnored(t *testing.T) {
	def := NewRetryCurve(RetryCurveBaseDelay(time.Second))
	r := mkRetryDelayWithCurves(def, []*RetryCurve{nil, nil}, 0, 0)
	// Should not panic and should still work as a single-curve stream.
	d := r.NextRetryDelay(time.Now())
	assert.Equal(t, time.Second, d)
}

func TestRegisteredCurveEqualToDefaultIsDeduped(t *testing.T) {
	def := NewRetryCurve(RetryCurveBaseDelay(time.Second))
	// Registering the default as an additional curve is a no-op.
	r := mkRetryDelayWithCurves(def, []*RetryCurve{def}, 0, 0)
	assert.Len(t, r.curves, 1)
}

func TestLegacyOptionsSynthesizeEffectiveDefault(t *testing.T) {
	// No explicit default curve provided; legacy stream options should populate
	// the effective default.
	initialRetry := time.Millisecond * 500
	backoffMaxDelay := time.Second * 10
	jitterRatio := float64(0)
	retryResetInterval := time.Second * 30
	opts := &streamOptions{
		initialRetry:       &initialRetry,
		backoffMaxDelay:    &backoffMaxDelay,
		jitterRatio:        &jitterRatio,
		retryResetInterval: &retryResetInterval,
	}
	r := newRetryDelayStrategyFromOptions(opts, 0)

	// First delay uses baseDelay from legacy options.
	d := r.NextRetryDelay(time.Now())
	assert.Equal(t, time.Millisecond*500, d)
	// Second delay applies backoff: 500ms * 2 = 1s.
	d = r.NextRetryDelay(time.Now())
	assert.Equal(t, time.Second, d)
}

func TestNoConfigurationYieldsHardCodedFallbackBehavior(t *testing.T) {
	// No default curve, no legacy option overrides. The library synthesizes an
	// empty *RetryCurve as the effective default; overlay resolution falls through
	// to hard-coded fallbacks (DefaultInitialRetry for baseDelay; no backoff; no
	// jitter). Verify by observing behavior: first delay is DefaultInitialRetry.
	opts := &streamOptions{}
	r := newRetryDelayStrategyFromOptions(opts, 0)
	d := r.NextRetryDelay(time.Now())
	assert.Equal(t, DefaultInitialRetry, d)
}

// The extended regime targeted by the RETRY spec starts at 5 minutes and doubles
// until it clamps to a 1-hour ceiling: 5m, 10m, 20m, 40m, 1hr, 1hr, ... This test
// pins the exact sequence so the epic-stated behavior is guarded against
// accidental math regressions.
func TestExtendedCurveProgressionMatchesRetrySpec(t *testing.T) {
	ext := NewRetryCurve(
		RetryCurveBaseDelay(time.Minute*5),
		RetryCurveMaxDelay(time.Hour),
	)
	r := mkRetryDelayWithCurves(ext, nil, 0, 0)

	expected := []time.Duration{
		time.Minute * 5,
		time.Minute * 10,
		time.Minute * 20,
		time.Minute * 40,
		time.Hour,
		time.Hour,
		time.Hour,
	}
	t0 := time.Now()
	for i, want := range expected {
		got := r.NextRetryDelay(t0)
		assert.Equal(t, want, got, "attempt %d", i)
	}
}

// The public StreamOption wrappers must build the same strategy shape as
// constructing the streamOptions struct directly. This is the only test that
// exercises defaultRetryCurveOption.apply and registerRetryCurveOption.apply
// end-to-end; every other curve-related test bypasses the options by calling
// mkRetryDelayWithCurves.
func TestStreamOptionCurveWrappersWireStrategy(t *testing.T) {
	def := NewRetryCurve(RetryCurveBaseDelay(time.Second))
	ext := NewRetryCurve(
		RetryCurveBaseDelay(time.Minute*5),
		RetryCurveMaxDelay(time.Hour),
	)

	opts := &streamOptions{}
	assert.NoError(t, StreamOptionDefaultRetryCurve(def).apply(opts))
	assert.NoError(t, StreamOptionRegisterRetryCurve(ext).apply(opts))
	r := newRetryDelayStrategyFromOptions(opts, 0)

	// Effective default is the curve installed via the option.
	assert.Same(t, def, r.effectiveDefault)
	// First delay uses def's baseDelay.
	assert.Equal(t, time.Second, r.NextRetryDelay(time.Now()))
	// ext is reachable via activation.
	r.activateCurve(ext)
	assert.Equal(t, time.Minute*5, r.NextRetryDelay(time.Now()))
}

// When both a legacy StreamOptionInitialRetry and a new StreamOptionDefaultRetryCurve
// are provided, the explicit curve wins and the legacy value is silently ignored.
// This pins the currently-undocumented precedence so a future change to it is a
// deliberate act.
func TestExplicitDefaultCurveOverridesLegacyInitialRetry(t *testing.T) {
	explicit := NewRetryCurve(RetryCurveBaseDelay(time.Millisecond * 250))

	opts := &streamOptions{}
	assert.NoError(t, StreamOptionInitialRetry(time.Second*7).apply(opts))
	assert.NoError(t, StreamOptionDefaultRetryCurve(explicit).apply(opts))
	r := newRetryDelayStrategyFromOptions(opts, 0)

	// The explicit curve's baseDelay wins; the legacy 7s value is ignored.
	assert.Equal(t, time.Millisecond*250, r.NextRetryDelay(time.Now()))
}

func TestActivateCurveNilIsSilentNoOp(t *testing.T) {
	def := NewRetryCurve(RetryCurveBaseDelay(time.Second))
	ext := NewRetryCurve(RetryCurveBaseDelay(time.Minute * 5))
	r := mkRetryDelayWithCurves(def, []*RetryCurve{ext}, 0, 0)

	r.activateCurve(ext)
	assert.Same(t, ext, r.activeCurve())

	// activateCurve(nil) must not panic and must not change the active pointer.
	r.activateCurve(nil)
	assert.Same(t, ext, r.activeCurve())
}

// With more than one registered curve, healthy-op reset must zero every curve's
// retryCount (not just the active one or the default), and ApplyRetryTime must
// set every curve's baseDelayOverride. Existing tests only exercise a single
// registered curve; this one guards the loop against silently mis-iterating.
func TestMultipleRegisteredCurvesAllTrackedByResetAndApplyRetryTime(t *testing.T) {
	def := NewRetryCurve(RetryCurveBaseDelay(time.Second), RetryCurveMaxDelay(time.Minute))
	extA := NewRetryCurve(RetryCurveBaseDelay(time.Minute*5), RetryCurveMaxDelay(time.Hour))
	extB := NewRetryCurve(RetryCurveBaseDelay(time.Minute*15), RetryCurveMaxDelay(time.Hour))
	resetInterval := time.Second * 30
	r := mkRetryDelayWithCurves(def, []*RetryCurve{extA, extB}, resetInterval, 0)

	t0 := time.Now()

	// Progress each curve's counter.
	_ = r.NextRetryDelay(t0) // def: 1s (n 0→1)
	r.activateCurve(extA)
	_ = r.NextRetryDelay(t0) // extA: 5m (n 0→1)
	r.activateCurve(extB)
	_ = r.NextRetryDelay(t0) // extB: 15m (n 0→1)

	// Reset. All three counters must be zero.
	r.activateCurve(DefaultCurve)
	r.SetGoodSince(t0)
	_ = r.NextRetryDelay(t0.Add(resetInterval))

	// Confirm every curve's counter was zeroed by activating each and observing
	// the first delay equals its declared baseDelay (n=0 branch).
	r.activateCurve(extA)
	assert.Equal(t, time.Minute*5, r.NextRetryDelay(t0.Add(resetInterval)))
	r.activateCurve(extB)
	assert.Equal(t, time.Minute*15, r.NextRetryDelay(t0.Add(resetInterval)))

	// ApplyRetryTime must hit every curve's baseDelayOverride.
	r.ApplyRetryTime(time.Millisecond * 750)
	r.activateCurve(DefaultCurve)
	assert.Equal(t, time.Millisecond*750, r.NextRetryDelay(t0.Add(resetInterval)))
	r.activateCurve(extA)
	assert.Equal(t, time.Millisecond*750, r.NextRetryDelay(t0.Add(resetInterval)))
	r.activateCurve(extB)
	assert.Equal(t, time.Millisecond*750, r.NextRetryDelay(t0.Add(resetInterval)))
}

// Registering the same curve pointer multiple times is deduped down to a single
// entry in the curves map. Complements TestRegisteredCurveEqualToDefaultIsDeduped,
// which covers the (default, registered) collision; this covers the
// (registered, registered) collision.
func TestDuplicateRegisteredCurveIsDeduped(t *testing.T) {
	def := NewRetryCurve(RetryCurveBaseDelay(time.Second))
	ext := NewRetryCurve(RetryCurveBaseDelay(time.Minute * 5))

	r := mkRetryDelayWithCurves(def, []*RetryCurve{ext, ext, ext}, 0, 0)
	assert.Len(t, r.curves, 2) // def + ext, not def + ext + ext + ext
}

// RetryCurveBaseDelay(0) is a legitimate way to say "zero base delay" and is
// semantically distinct from "unset" — an unset baseDelay falls through overlay
// resolution to the effective default (or the hard-coded fallback). Explicit
// zero, in contrast, produces a 0 duration for the first attempt. This test
// pins the distinction.
func TestRetryCurveBaseDelayZeroIsExplicitNotUnset(t *testing.T) {
	// Effective default has a nonzero base, so if `explicit-zero` were treated
	// as `unset`, the first delay would be 1s (from the default) rather than 0.
	def := NewRetryCurve(RetryCurveBaseDelay(time.Second))
	explicitZero := NewRetryCurve(RetryCurveBaseDelay(0))
	r := mkRetryDelayWithCurves(def, []*RetryCurve{explicitZero}, 0, 0)

	r.activateCurve(explicitZero)
	assert.Equal(t, time.Duration(0), r.NextRetryDelay(time.Now()))

	// Sanity: an unset baseDelay on a different curve DOES fall through to def.
	unset := NewRetryCurve(RetryCurveMaxDelay(time.Second * 10))
	r2 := mkRetryDelayWithCurves(def, []*RetryCurve{unset}, 0, 0)
	r2.activateCurve(unset)
	assert.Equal(t, time.Second, r2.NextRetryDelay(time.Now()))
}
