package auth

import (
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
)

// quotaRecheckPolicy is the resolved recheck setting for one provider.
type quotaRecheckPolicy struct {
	mode         internalconfig.QuotaReconcileMode
	interval     time.Duration
	nearInterval time.Duration
	nearWindow   time.Duration
}

// quotaRecheckPolicyFor resolves the configured mode and cadence for provider.
// Codex defaults reproduce the original loop: probe a cooled credential on
// every pass, limited only by quotaProbeMinGap. Other providers default to a
// slow cadence with a faster one shortly before a known reset.
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
	if strings.EqualFold(strings.TrimSpace(provider), "codex") {
		policy.interval, policy.nearInterval, policy.nearWindow = quotaProbeMinGap, quotaProbeMinGap, 0
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
	return true
}

// noteCleared records that the loop cleared id's cooldown at now.
func (s *quotaRecheckSchedule) noteCleared(id string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state(id).lastCleared = now
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
// credential's quota cooldowns, or zero when none is known.
func soonestKnownReset(auth *Auth, now time.Time) time.Time {
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
