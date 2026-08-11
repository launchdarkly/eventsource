package eventsource

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// mkRetryDelayWithProfiles builds a retryDelayStrategy from streamOptions that
// include a default profile, any number of registered profiles, and an optional
// reset interval (applies per eventsource instance).
//
// The helper always addresses resetInterval, so passing 0 explicitly disables
// the healthy-op reset (the NextRetryDelay reset check gates on
// `resetInterval > 0`). Tests that want the library-default 60s fallback should
// construct streamOptions directly and leave retryResetInterval nil.
func mkRetryDelayWithProfiles(
	defaultProfile *RetryProfile,
	registered []*RetryProfile,
	resetInterval time.Duration,
	randSeed int64,
) *retryDelayStrategy {
	opts := &streamOptions{
		defaultRetryProfile:     defaultProfile,
		registeredRetryProfiles: registered,
		retryResetInterval:    &resetInterval,
	}
	return newRetryDelayStrategyFromOptions(opts, randSeed)
}

func TestActiveProfileReturnsEffectiveDefaultAtStart(t *testing.T) {
	def := NewRetryProfile(
		RetryProfileBaseDelay(time.Second),
		RetryProfileMaxDelay(time.Second*30),
	)
	r := mkRetryDelayWithProfiles(def, nil, 0, 0)
	assert.Same(t, def, r.activeProfile())
}

func TestActivateProfileSwitchesToRegisteredProfileImmediately(t *testing.T) {
	def := NewRetryProfile(RetryProfileBaseDelay(time.Second))
	ext := NewRetryProfile(RetryProfileBaseDelay(time.Minute * 5))
	r := mkRetryDelayWithProfiles(def, []*RetryProfile{ext}, 0, 0)

	r.activateProfile(ext)
	// Activation is immediate; the observer sees the change right away.
	assert.Same(t, ext, r.activeProfile())

	d := r.NextRetryDelay(time.Now())
	assert.Same(t, ext, r.activeProfile())
	assert.Equal(t, time.Minute*5, d)
}

func TestActivateProfileUnregisteredIsSilentNoOp(t *testing.T) {
	def := NewRetryProfile(RetryProfileBaseDelay(time.Second))
	ext := NewRetryProfile(RetryProfileBaseDelay(time.Minute * 5))
	unrelated := NewRetryProfile(RetryProfileBaseDelay(time.Minute))
	r := mkRetryDelayWithProfiles(def, []*RetryProfile{ext}, 0, 0)

	r.activateProfile(unrelated)
	// Unrelated profile silently ignored; active still points at default.
	assert.Same(t, def, r.activeProfile())
	d := r.NextRetryDelay(time.Now())
	assert.Equal(t, time.Second, d)
}

func TestActivateProfileDefaultSentinelRevertsToEffectiveDefault(t *testing.T) {
	def := NewRetryProfile(RetryProfileBaseDelay(time.Second))
	ext := NewRetryProfile(RetryProfileBaseDelay(time.Minute * 5))
	r := mkRetryDelayWithProfiles(def, []*RetryProfile{ext}, 0, 0)

	// Move to extended immediately.
	r.activateProfile(ext)
	assert.Same(t, ext, r.activeProfile())

	// Sentinel means "revert to effective default."
	r.activateProfile(DefaultProfile)
	assert.Same(t, def, r.activeProfile())
	d := r.NextRetryDelay(time.Now())
	assert.Equal(t, time.Second, d)
}

func TestPerProfileRetryCountIsIndependent(t *testing.T) {
	def := NewRetryProfile(
		RetryProfileBaseDelay(time.Second),
		RetryProfileMaxDelay(time.Minute),
	)
	ext := NewRetryProfile(
		RetryProfileBaseDelay(time.Minute*5),
		RetryProfileMaxDelay(time.Hour),
	)
	r := mkRetryDelayWithProfiles(def, []*RetryProfile{ext}, 0, 0)

	t0 := time.Now()
	// Progress default's counter to 3 attempts.
	_ = r.NextRetryDelay(t0) // 1s (counter 0 → 1)
	_ = r.NextRetryDelay(t0) // 2s (counter 1 → 2)
	_ = r.NextRetryDelay(t0) // 4s (counter 2 → 3)

	// Switch to extended. Extended's counter is still 0; first extended delay is baseDelay.
	r.activateProfile(ext)
	d := r.NextRetryDelay(t0)
	assert.Equal(t, time.Minute*5, d)

	// Second extended attempt: 5min * 2 = 10min.
	d = r.NextRetryDelay(t0)
	assert.Equal(t, time.Minute*10, d)

	// Revert to default: counter picks up where it left off (was 3, next attempt uses 3 → 8s).
	r.activateProfile(DefaultProfile)
	d = r.NextRetryDelay(t0)
	assert.Equal(t, time.Second*8, d)
}

// Reset trumps a same-cycle activation: a caller who activated an alternative profile
// before NextRetryDelay observes the reset condition sees the reset override their
// choice. This gives the natural semantic that a transient unexpected failure after
// a long healthy period does not push the SDK into the alternative regime.
func TestResetTrumpsActivation(t *testing.T) {
	def := NewRetryProfile(
		RetryProfileBaseDelay(time.Second),
		RetryProfileMaxDelay(time.Minute),
	)
	ext := NewRetryProfile(
		RetryProfileBaseDelay(time.Minute*5),
		RetryProfileMaxDelay(time.Hour),
	)
	resetInterval := time.Second * 30
	r := mkRetryDelayWithProfiles(def, []*RetryProfile{ext}, resetInterval, 0)

	t0 := time.Now()
	// Establish a healthy period longer than resetInterval.
	r.SetGoodSince(t0)

	// Caller activates ext (immediate).
	r.activateProfile(ext)
	assert.Same(t, ext, r.activeProfile())

	// NextRetryDelay fires with a currentTime past the reset threshold. Reset trumps
	// activation: active reverts to default, counters zeroed, delay uses default.baseDelay.
	d := r.NextRetryDelay(t0.Add(resetInterval))
	assert.Same(t, def, r.activeProfile())
	assert.Equal(t, time.Second, d)
}

// Persistent failures — after reset trumps the first activation, subsequent activations
// (with no intervening healthy period long enough to reset) succeed and drive extended
// regime progression.
func TestPersistentFailuresRampIntoExtended(t *testing.T) {
	def := NewRetryProfile(
		RetryProfileBaseDelay(time.Second),
		RetryProfileMaxDelay(time.Minute),
	)
	ext := NewRetryProfile(
		RetryProfileBaseDelay(time.Minute*5),
		RetryProfileMaxDelay(time.Hour),
	)
	resetInterval := time.Second * 30
	r := mkRetryDelayWithProfiles(def, []*RetryProfile{ext}, resetInterval, 0)

	t0 := time.Now()
	r.SetGoodSince(t0)

	// Failure #1: after healthy period. Reset trumps activation.
	r.activateProfile(ext)
	d := r.NextRetryDelay(t0.Add(resetInterval))
	assert.Equal(t, time.Second, d) // reset fired, default active

	// Failure #2 shortly after: no healthy period, no reset.
	r.activateProfile(ext)
	d = r.NextRetryDelay(t0.Add(resetInterval + time.Millisecond))
	assert.Same(t, ext, r.activeProfile())
	assert.Equal(t, time.Minute*5, d)

	// Failure #3: continues extended progression.
	r.activateProfile(ext) // no-op, ext already active
	d = r.NextRetryDelay(t0.Add(resetInterval + time.Millisecond*2))
	assert.Equal(t, time.Minute*10, d)
}

func TestResetZerosAllProfilesCounters(t *testing.T) {
	def := NewRetryProfile(
		RetryProfileBaseDelay(time.Second),
		RetryProfileMaxDelay(time.Minute),
	)
	ext := NewRetryProfile(
		RetryProfileBaseDelay(time.Minute*5),
		RetryProfileMaxDelay(time.Hour),
	)
	resetInterval := time.Second * 30
	r := mkRetryDelayWithProfiles(def, []*RetryProfile{ext}, resetInterval, 0)

	t0 := time.Now()

	// Progress default.
	_ = r.NextRetryDelay(t0)
	_ = r.NextRetryDelay(t0)

	// Progress extended.
	r.activateProfile(ext)
	_ = r.NextRetryDelay(t0)
	_ = r.NextRetryDelay(t0)

	// Return to default, healthy period elapses, then failure triggers reset.
	r.activateProfile(DefaultProfile)
	r.SetGoodSince(t0)
	d := r.NextRetryDelay(t0.Add(resetInterval))
	assert.Equal(t, time.Second, d) // default's counter zeroed

	// Confirm extended's counter was also zeroed.
	r.activateProfile(ext)
	d = r.NextRetryDelay(t0.Add(resetInterval + time.Millisecond))
	assert.Equal(t, time.Minute*5, d)
}

// ApplyRetryTime applies per eventsource instance, matching the WHATWG HTML
// Living Standard's EventSource "reconnection time" semantics. It updates every
// registered profile's baseDelay uniformly and resets each profile's formula
// counter, so subsequent attempts in any regime start from the hinted base.
// Each regime still enforces its own maxDelay ceiling.
func TestApplyRetryTimeUpdatesAllProfiles(t *testing.T) {
	def := NewRetryProfile(
		RetryProfileBaseDelay(time.Second),
		RetryProfileMaxDelay(time.Second*30),
	)
	ext := NewRetryProfile(
		RetryProfileBaseDelay(time.Minute*5),
		RetryProfileMaxDelay(time.Hour),
	)
	r := mkRetryDelayWithProfiles(def, []*RetryProfile{ext}, 0, 0)

	t0 := time.Now()

	// Progress default's counter so we can verify the reset that comes with the
	// hint.
	_ = r.NextRetryDelay(t0)
	_ = r.NextRetryDelay(t0)

	// Server hint: 500ms. All profiles' baseDelays should become 500ms and their
	// formula counters should be zeroed.
	r.ApplyRetryTime(time.Millisecond * 500)

	// Default: first attempt after hint uses the hinted value literally.
	d := r.NextRetryDelay(t0)
	assert.Equal(t, time.Millisecond*500, d)

	// Second attempt on the default profile: backoff continues to apply against
	// the hinted base (500ms * 2^1 = 1s), pinning that ApplyRetryTime replaces
	// the base but does not disable backoff.
	d = r.NextRetryDelay(t0)
	assert.Equal(t, time.Second, d)

	// Third attempt on the default profile: 500ms * 2^2 = 2s (still under the
	// 30s cap).
	d = r.NextRetryDelay(t0)
	assert.Equal(t, time.Second*2, d)

	// Switch to extended. Extended's baseDelay is now also 500ms (the hint
	// applied to every registered profile on this eventsource instance) and its
	// retryCount was zeroed by the hint. First attempt uses the hinted value
	// literally; its maxDelay ceiling of 1hr still applies.
	r.activateProfile(ext)
	d = r.NextRetryDelay(t0)
	assert.Equal(t, time.Millisecond*500, d)

	// Second attempt on extended: 500ms * 2^1 = 1s. Backoff continues even after
	// the hint replaces the base, and it does so independently of the default
	// profile's own counter progression above.
	d = r.NextRetryDelay(t0)
	assert.Equal(t, time.Second, d)
}

// Base-delay mutations by ApplyRetryTime persist across healthy-operation reset —
// matching the SSE spec's "reconnection time is set until updated" semantic.
// Reset zeros counters and reverts the active pointer, but does not touch the
// mutated base delays.
func TestBaseDelayMutationPersistsAcrossReset(t *testing.T) {
	def := NewRetryProfile(RetryProfileBaseDelay(time.Second))
	resetInterval := time.Second * 30
	r := mkRetryDelayWithProfiles(def, nil, resetInterval, 0)

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
	def := NewRetryProfile(
		RetryProfileBaseDelay(time.Second),
		RetryProfileMaxDelay(time.Minute),
		RetryProfileJitter(0),
	)
	// ext specifies only baseDelay + maxDelay; jitter should inherit from default (0).
	ext := NewRetryProfile(
		RetryProfileBaseDelay(time.Minute*5),
		RetryProfileMaxDelay(time.Hour),
	)
	r := mkRetryDelayWithProfiles(def, []*RetryProfile{ext}, 0, 0)

	// Verify extended's resolved values by observing behavior. First extended attempt
	// yields 5min (baseDelay) with no jitter (inherited 0).
	r.activateProfile(ext)
	d := r.NextRetryDelay(time.Now())
	assert.Equal(t, time.Minute*5, d)
}

func TestNilRegisteredProfilesAreIgnored(t *testing.T) {
	def := NewRetryProfile(RetryProfileBaseDelay(time.Second))
	r := mkRetryDelayWithProfiles(def, []*RetryProfile{nil, nil}, 0, 0)
	// Should not panic and should still work as a single-profile stream.
	d := r.NextRetryDelay(time.Now())
	assert.Equal(t, time.Second, d)
}

func TestRegisteredProfileEqualToDefaultIsDeduped(t *testing.T) {
	def := NewRetryProfile(RetryProfileBaseDelay(time.Second))
	// Registering the default as an additional profile is a no-op.
	r := mkRetryDelayWithProfiles(def, []*RetryProfile{def}, 0, 0)
	assert.Len(t, r.profiles, 1)
}

func TestLegacyOptionsSynthesizeEffectiveDefault(t *testing.T) {
	// No explicit default profile provided; legacy stream options should populate
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
	// No default profile, no legacy option overrides. The library synthesizes an
	// empty *RetryProfile as the effective default; overlay resolution falls through
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
func TestExtendedProfileProgressionMatchesRetrySpec(t *testing.T) {
	ext := NewRetryProfile(
		RetryProfileBaseDelay(time.Minute*5),
		RetryProfileMaxDelay(time.Hour),
	)
	r := mkRetryDelayWithProfiles(ext, nil, 0, 0)

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
// exercises defaultRetryProfileOption.apply and registerRetryProfileOption.apply
// end-to-end; every other profile-related test bypasses the options by calling
// mkRetryDelayWithProfiles.
func TestStreamOptionProfileWrappersWireStrategy(t *testing.T) {
	def := NewRetryProfile(RetryProfileBaseDelay(time.Second))
	ext := NewRetryProfile(
		RetryProfileBaseDelay(time.Minute*5),
		RetryProfileMaxDelay(time.Hour),
	)

	opts := &streamOptions{}
	assert.NoError(t, StreamOptionDefaultRetryProfile(def).apply(opts))
	assert.NoError(t, StreamOptionRegisterRetryProfile(ext).apply(opts))
	r := newRetryDelayStrategyFromOptions(opts, 0)

	// Effective default is the profile installed via the option.
	assert.Same(t, def, r.effectiveDefault)
	// First delay uses def's baseDelay.
	assert.Equal(t, time.Second, r.NextRetryDelay(time.Now()))
	// ext is reachable via activation.
	r.activateProfile(ext)
	assert.Equal(t, time.Minute*5, r.NextRetryDelay(time.Now()))
}

// When both a legacy StreamOptionInitialRetry and a new StreamOptionDefaultRetryProfile
// are provided, the explicit profile wins and the legacy value is silently ignored.
// This pins the currently-undocumented precedence so a future change to it is a
// deliberate act.
func TestExplicitDefaultProfileOverridesLegacyInitialRetry(t *testing.T) {
	explicit := NewRetryProfile(RetryProfileBaseDelay(time.Millisecond * 250))

	opts := &streamOptions{}
	assert.NoError(t, StreamOptionInitialRetry(time.Second*7).apply(opts))
	assert.NoError(t, StreamOptionDefaultRetryProfile(explicit).apply(opts))
	r := newRetryDelayStrategyFromOptions(opts, 0)

	// The explicit profile's baseDelay wins; the legacy 7s value is ignored.
	assert.Equal(t, time.Millisecond*250, r.NextRetryDelay(time.Now()))
}

func TestActivateProfileNilIsSilentNoOp(t *testing.T) {
	def := NewRetryProfile(RetryProfileBaseDelay(time.Second))
	ext := NewRetryProfile(RetryProfileBaseDelay(time.Minute * 5))
	r := mkRetryDelayWithProfiles(def, []*RetryProfile{ext}, 0, 0)

	r.activateProfile(ext)
	assert.Same(t, ext, r.activeProfile())

	// activateProfile(nil) must not panic and must not change the active pointer.
	r.activateProfile(nil)
	assert.Same(t, ext, r.activeProfile())
}

// With more than one registered profile, healthy-op reset must zero every profile's
// retryCount (not just the active one or the default), and ApplyRetryTime must
// set every profile's baseDelayOverride. Existing tests only exercise a single
// registered profile; this one guards the loop against silently mis-iterating.
func TestMultipleRegisteredProfilesAllTrackedByResetAndApplyRetryTime(t *testing.T) {
	def := NewRetryProfile(RetryProfileBaseDelay(time.Second), RetryProfileMaxDelay(time.Minute))
	extA := NewRetryProfile(RetryProfileBaseDelay(time.Minute*5), RetryProfileMaxDelay(time.Hour))
	extB := NewRetryProfile(RetryProfileBaseDelay(time.Minute*15), RetryProfileMaxDelay(time.Hour))
	resetInterval := time.Second * 30
	r := mkRetryDelayWithProfiles(def, []*RetryProfile{extA, extB}, resetInterval, 0)

	t0 := time.Now()

	// Progress each profile's counter.
	_ = r.NextRetryDelay(t0) // def: 1s (n 0→1)
	r.activateProfile(extA)
	_ = r.NextRetryDelay(t0) // extA: 5m (n 0→1)
	r.activateProfile(extB)
	_ = r.NextRetryDelay(t0) // extB: 15m (n 0→1)

	// Reset. All three counters must be zero.
	r.activateProfile(DefaultProfile)
	r.SetGoodSince(t0)
	_ = r.NextRetryDelay(t0.Add(resetInterval))

	// Confirm every profile's counter was zeroed by activating each and observing
	// the first delay equals its declared baseDelay (n=0 branch).
	r.activateProfile(extA)
	assert.Equal(t, time.Minute*5, r.NextRetryDelay(t0.Add(resetInterval)))
	r.activateProfile(extB)
	assert.Equal(t, time.Minute*15, r.NextRetryDelay(t0.Add(resetInterval)))

	// ApplyRetryTime must hit every profile's baseDelayOverride.
	r.ApplyRetryTime(time.Millisecond * 750)
	r.activateProfile(DefaultProfile)
	assert.Equal(t, time.Millisecond*750, r.NextRetryDelay(t0.Add(resetInterval)))
	r.activateProfile(extA)
	assert.Equal(t, time.Millisecond*750, r.NextRetryDelay(t0.Add(resetInterval)))
	r.activateProfile(extB)
	assert.Equal(t, time.Millisecond*750, r.NextRetryDelay(t0.Add(resetInterval)))
}

// Registering the same profile pointer multiple times is deduped down to a single
// entry in the profiles map. Complements TestRegisteredProfileEqualToDefaultIsDeduped,
// which covers the (default, registered) collision; this covers the
// (registered, registered) collision.
func TestDuplicateRegisteredProfileIsDeduped(t *testing.T) {
	def := NewRetryProfile(RetryProfileBaseDelay(time.Second))
	ext := NewRetryProfile(RetryProfileBaseDelay(time.Minute * 5))

	r := mkRetryDelayWithProfiles(def, []*RetryProfile{ext, ext, ext}, 0, 0)
	assert.Len(t, r.profiles, 2) // def + ext, not def + ext + ext + ext
}

// RetryProfileBaseDelay(0) is a legitimate way to say "zero base delay" and is
// semantically distinct from "unset" — an unset baseDelay falls through overlay
// resolution to the effective default (or the hard-coded fallback). Explicit
// zero, in contrast, produces a 0 duration for the first attempt. This test
// pins the distinction.
func TestRetryProfileBaseDelayZeroIsExplicitNotUnset(t *testing.T) {
	// Effective default has a nonzero base, so if `explicit-zero` were treated
	// as `unset`, the first delay would be 1s (from the default) rather than 0.
	def := NewRetryProfile(RetryProfileBaseDelay(time.Second))
	explicitZero := NewRetryProfile(RetryProfileBaseDelay(0))
	r := mkRetryDelayWithProfiles(def, []*RetryProfile{explicitZero}, 0, 0)

	r.activateProfile(explicitZero)
	assert.Equal(t, time.Duration(0), r.NextRetryDelay(time.Now()))

	// Sanity: an unset baseDelay on a different profile DOES fall through to def.
	unset := NewRetryProfile(RetryProfileMaxDelay(time.Second * 10))
	r2 := mkRetryDelayWithProfiles(def, []*RetryProfile{unset}, 0, 0)
	r2.activateProfile(unset)
	assert.Equal(t, time.Second, r2.NextRetryDelay(time.Now()))
}

// RetryProfileMaxDelay(0) is the documented way to disable backoff on a specific
// profile (per the docstring on RetryProfileMaxDelay in retry_profile.go). This must
// override any positive default's maxDelay via the overlay stack — a profile
// with an explicit zero max should NOT inherit the default's backoff ceiling.
func TestRetryProfileMaxDelayZeroOverridesPositiveDefault(t *testing.T) {
	// Effective default has a positive maxDelay, so backoff would double each
	// attempt if inherited. Explicit-zero on the ext profile must disable that.
	def := NewRetryProfile(
		RetryProfileBaseDelay(time.Second),
		RetryProfileMaxDelay(time.Minute),
	)
	explicitZeroMax := NewRetryProfile(
		RetryProfileBaseDelay(time.Second),
		RetryProfileMaxDelay(0),
	)
	r := mkRetryDelayWithProfiles(def, []*RetryProfile{explicitZeroMax}, 0, 0)
	r.activateProfile(explicitZeroMax)

	// With backoff disabled every attempt uses the base delay directly; no
	// doubling despite retryCount advancing.
	t0 := time.Now()
	assert.Equal(t, time.Second, r.NextRetryDelay(t0))
	assert.Equal(t, time.Second, r.NextRetryDelay(t0))
	assert.Equal(t, time.Second, r.NextRetryDelay(t0))

	// Sanity: an unset maxDelay on a different profile DOES inherit the def's
	// positive ceiling, so backoff kicks in.
	unsetMax := NewRetryProfile(RetryProfileBaseDelay(time.Second))
	r2 := mkRetryDelayWithProfiles(def, []*RetryProfile{unsetMax}, 0, 0)
	r2.activateProfile(unsetMax)
	assert.Equal(t, time.Second, r2.NextRetryDelay(t0))
	assert.Equal(t, time.Second*2, r2.NextRetryDelay(t0))
}

// RetryProfileJitter(0) is the documented way to disable jitter on a specific
// profile. Analogous to the maxDelay-override test above: an explicit zero on
// the ext profile must override a positive default jitter via the overlay stack.
func TestRetryProfileJitterZeroOverridesPositiveDefault(t *testing.T) {
	// Effective default has a positive jitter ratio (jitter subtracts up to 50%
	// of the computed delay). Explicit-zero on the ext profile must disable it,
	// so NextRetryDelay returns the raw base every time.
	def := NewRetryProfile(
		RetryProfileBaseDelay(time.Second),
		RetryProfileJitter(0.5),
	)
	explicitZeroJitter := NewRetryProfile(
		RetryProfileBaseDelay(time.Second),
		RetryProfileJitter(0),
	)
	r := mkRetryDelayWithProfiles(def, []*RetryProfile{explicitZeroJitter}, 0, 0)
	r.activateProfile(explicitZeroJitter)

	// No jitter → the delay is deterministically the base, regardless of RNG.
	t0 := time.Now()
	assert.Equal(t, time.Second, r.NextRetryDelay(t0))
	assert.Equal(t, time.Second, r.NextRetryDelay(t0))

	// Sanity: an unset jitter on a different profile DOES inherit the def's
	// positive ratio, so the observed delay is strictly less than the base.
	unsetJitter := NewRetryProfile(RetryProfileBaseDelay(time.Second))
	r2 := mkRetryDelayWithProfiles(def, []*RetryProfile{unsetJitter}, 0, 42)
	r2.activateProfile(unsetJitter)
	d := r2.NextRetryDelay(t0)
	assert.Less(t, int64(d), int64(time.Second), "with inherited jitter the delay should be less than base")
	assert.Greater(t, int64(d), int64(time.Millisecond*500), "with 50%% jitter cap the delay should still be at least half of base")
}
