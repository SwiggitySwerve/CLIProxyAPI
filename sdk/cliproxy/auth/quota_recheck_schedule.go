package auth

import (
	"maps"
	"sort"
	"strings"
	"sync"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

const (
	// quotaFlapWindow is how soon after the loop clears a cooldown a quota 429
	// counts as the clear having been wrong.
	quotaFlapWindow = 10 * time.Minute
	// quotaBackoffCap bounds the recheck interval after repeated flaps.
	quotaBackoffCap = 6 * time.Hour

	claudeRecheckInterval     = 30 * time.Minute
	claudeRecheckNearInterval = 5 * time.Minute
	claudeRecheckNearWindow   = time.Hour

	// quotaTriggerMinGap is the shortest gap between two verdicts when a usage
	// snapshot change asks for an early one.
	quotaTriggerMinGap = 2 * time.Minute
	// quotaSnapshotBackoffCap bounds the snapshot interval after failed snapshots.
	quotaSnapshotBackoffCap = time.Hour
)

// quotaRecheckPolicy is the resolved recheck setting for one provider.
type quotaRecheckPolicy struct {
	mode         internalconfig.QuotaReconcileMode
	interval     time.Duration
	nearInterval time.Duration
	nearWindow   time.Duration
}

// quotaRecheckCadence is a provider's default cadence (registry recheckCadence).
type quotaRecheckCadence struct {
	interval, nearInterval, nearWindow time.Duration
}

// quotaRecheckPolicyFor resolves the configured mode and cadence for provider.
// A provider without a registry cadence (Codex has one) defaults to a slow
// cadence with a faster one shortly before a known reset.
func quotaRecheckPolicyFor(cfg *internalconfig.Config, provider string) quotaRecheckPolicy {
	if cfg == nil {
		return quotaRecheckPolicy{mode: internalconfig.QuotaReconcileOff}
	}
	configured, ok := cfg.QuotaReconcile.Provider(provider)
	if !ok {
		return quotaRecheckPolicy{mode: internalconfig.QuotaReconcileOff}
	}
	policy := quotaRecheckPolicy{
		mode:         configured.Mode,
		interval:     claudeRecheckInterval,
		nearInterval: claudeRecheckNearInterval,
		nearWindow:   claudeRecheckNearWindow,
	}
	if c := providerQuotaFor(provider).recheckCadence; c != nil {
		policy.interval, policy.nearInterval, policy.nearWindow = c.interval, c.nearInterval, c.nearWindow
	}
	if configured.Interval > 0 {
		policy.interval = configured.Interval
	}
	if configured.NearResetInterval > 0 {
		policy.nearInterval = configured.NearResetInterval
	}
	if configured.NearResetWindow > 0 {
		policy.nearWindow = configured.NearResetWindow
	}
	policy.interval = max(policy.interval, quotaProbeMinGap)
	policy.nearInterval = max(policy.nearInterval, quotaProbeMinGap)
	return policy
}

// baseInterval picks the near interval within nearWindow before knownReset and
// the far interval otherwise.
func (p quotaRecheckPolicy) baseInterval(now, knownReset time.Time) time.Duration {
	if p.nearWindow > 0 && knownReset.After(now) && knownReset.Sub(now) <= p.nearWindow {
		return p.nearInterval
	}
	return p.interval
}

// quotaRecheckState is the per-credential schedule and flap-backoff state.
type quotaRecheckState struct {
	nextCheck    time.Time
	probedAt     time.Time
	baseInterval time.Duration
	lastCleared  time.Time
	backoffLevel int
	// backoffResetAt is the known reset time after which the backoff is dropped.
	backoffResetAt time.Time

	// snapshotNext is when the advisory usage snapshot is next due.
	snapshotNext time.Time
	// snapshotFailures counts consecutive failed snapshots for their backoff.
	snapshotFailures int
	// snapshotSignals is the last successful snapshot, the baseline for change
	// detection.
	snapshotSignals map[string]string
}

// quotaRecheckSchedule holds per-credential state for the recheck loop. clock
// is replaced in tests; nil means time.Now.
type quotaRecheckSchedule struct {
	mu     sync.Mutex
	states map[string]*quotaRecheckState
	clock  func() time.Time
}

func (s *quotaRecheckSchedule) now() time.Time {
	if s.clock != nil {
		return s.clock()
	}
	return time.Now()
}

func (s *quotaRecheckSchedule) state(id string) *quotaRecheckState {
	if s.states == nil {
		s.states = make(map[string]*quotaRecheckState)
	}
	st := s.states[id]
	if st == nil {
		st = &quotaRecheckState{}
		s.states[id] = st
	}
	return st
}

func backedOffInterval(base time.Duration, level int) time.Duration {
	limit := max(quotaBackoffCap, base)
	d := base
	for i := 0; i < level && d < limit; i++ {
		d *= 2
	}
	return min(d, limit)
}

// begin reports whether id is due at now and, if so, books its next check from
// policy and knownReset (the soonest known reset of its cooldown, or zero).
func (s *quotaRecheckSchedule) begin(id string, now time.Time, policy quotaRecheckPolicy, knownReset time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.state(id)
	if now.Before(st.nextCheck) {
		return false
	}
	st.book(now, policy, knownReset)
	return true
}

// beginTriggered books an early check for id after a change trigger, unless the
// previous check ran less than quotaTriggerMinGap before now.
func (s *quotaRecheckSchedule) beginTriggered(id string, now time.Time, policy quotaRecheckPolicy, knownReset time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.state(id)
	if !st.probedAt.IsZero() && now.Sub(st.probedAt) < quotaTriggerMinGap {
		return false
	}
	st.book(now, policy, knownReset)
	return true
}

// book records a check at now and schedules the next one.
func (st *quotaRecheckState) book(now time.Time, policy quotaRecheckPolicy, knownReset time.Time) {
	if st.backoffLevel > 0 {
		if st.backoffResetAt.IsZero() {
			st.backoffResetAt = knownReset
		} else if now.After(st.backoffResetAt) {
			st.backoffLevel, st.backoffResetAt = 0, time.Time{}
		}
	}
	st.probedAt = now
	st.baseInterval = policy.baseInterval(now, knownReset)
	st.nextCheck = now.Add(backedOffInterval(st.baseInterval, st.backoffLevel))
}

// beginSnapshot reports whether id's usage snapshot is due at now and, if so,
// books the next one after interval.
func (s *quotaRecheckSchedule) beginSnapshot(id string, now time.Time, interval time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.state(id)
	if now.Before(st.snapshotNext) {
		return false
	}
	st.snapshotNext = now.Add(interval)
	return true
}

// noteSnapshot stores signals as id's new baseline and reports whether changed
// says they differ from the previous baseline. The first snapshot only sets the
// baseline.
func (s *quotaRecheckSchedule) noteSnapshot(id string, signals map[string]string, changed func(prev, next map[string]string) bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.state(id)
	prev := st.snapshotSignals
	st.snapshotSignals = maps.Clone(signals)
	st.snapshotFailures = 0
	return len(prev) > 0 && changed(prev, signals)
}

// noteSnapshotFailure keeps id's baseline and doubles the snapshot interval for
// each consecutive failure, up to quotaSnapshotBackoffCap. It returns the wait.
func (s *quotaRecheckSchedule) noteSnapshotFailure(id string, now time.Time, interval time.Duration) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.state(id)
	st.snapshotFailures++
	wait := interval
	for i := 0; i < st.snapshotFailures && wait < quotaSnapshotBackoffCap; i++ {
		wait *= 2
	}
	wait = min(wait, max(quotaSnapshotBackoffCap, interval))
	st.snapshotNext = now.Add(wait)
	return wait
}

// noteCleared records that the loop cleared id's cooldown at now. The snapshot
// baseline belongs to the cleared episode, so it is dropped.
func (s *quotaRecheckSchedule) noteCleared(id string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.state(id)
	st.lastCleared = now
	st.snapshotSignals = nil
}

// noteTrialCleared records that an all-cooled half-open trial cleared id's
// cooldown at now. The trial counts as a check: it is booked like one, so a
// quota 429 within quotaFlapWindow backs off the next check as it does after a
// clear by the loop.
func (s *quotaRecheckSchedule) noteTrialCleared(id string, now time.Time, policy quotaRecheckPolicy, knownReset time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.state(id)
	st.book(now, policy, knownReset)
	st.lastCleared = now
	st.snapshotSignals = nil
}

// noteQuotaRejection records a quota 429 for id. When it follows a clear by the
// loop within quotaFlapWindow, the recheck interval doubles.
func (s *quotaRecheckSchedule) noteQuotaRejection(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.states[id]
	if st == nil || st.lastCleared.IsZero() {
		return
	}
	now := s.now()
	if now.Sub(st.lastCleared) <= quotaFlapWindow {
		st.backoffLevel++
		st.backoffResetAt = time.Time{}
		st.nextCheck = st.probedAt.Add(backedOffInterval(st.baseInterval, st.backoffLevel))
	}
	st.lastCleared = time.Time{}
}

// soonestKnownReset returns the earliest future recovery time among the
// credential's quota cooldowns, or zero when none is known. A live credential
// quota holds every model at least until it ends, so a model's shorter quota
// cannot reset sooner.
func soonestKnownReset(auth *Auth, now time.Time) time.Time {
	if claudeCredentialCooldownLive(auth, now) {
		return auth.Quota.NextRecoverAt
	}
	var soonest time.Time
	consider := func(q QuotaState) {
		if q.Exceeded && q.NextRecoverAt.After(now) && (soonest.IsZero() || q.NextRecoverAt.Before(soonest)) {
			soonest = q.NextRecoverAt
		}
	}
	consider(auth.Quota)
	for _, state := range auth.ModelStates {
		if state != nil {
			consider(state.Quota)
		}
	}
	return soonest
}

// summarizeQuotaSignals renders verdict signals as sorted key=value pairs. The
// adapter contract limits signals to quota values, never credentials.
func summarizeQuotaSignals(signals map[string]string) string {
	if len(signals) == 0 {
		return ""
	}
	keys := make([]string, 0, len(signals))
	for key := range signals {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, key := range keys {
		parts[i] = key + "=" + signals[key]
	}
	return strings.Join(parts, ",")
}
