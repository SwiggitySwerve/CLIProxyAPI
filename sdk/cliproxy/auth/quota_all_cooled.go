package auth

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

// allCooledTrialGap is the minimum time between two all-cooled rechecks of the
// same credential, so a burst of client requests cannot hammer upstream.
const allCooledTrialGap = 60 * time.Second

// claudeTrialVerdict judges a 200 served on the cooled credential with the
// Claude adapter's own checks, against the windows that set its cooldown.
func claudeTrialVerdict(cooled *Auth, now time.Time) func(http.Header) QuotaVerdictState {
	windows, _ := claudeCooldownRejectedWindows(cooled, now)
	return func(headers http.Header) QuotaVerdictState {
		signals := make(map[string]string, len(headers))
		for key, values := range headers {
			if len(values) > 0 {
				signals[http.CanonicalHeaderKey(key)] = values[0]
			}
		}
		state, _ := claudeSignalsVerdict(signals, windows)
		return state
	}
}

// allCooledGate remembers when each credential was last rechecked because
// every candidate was cooled.
type allCooledGate struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func (g *allCooledGate) lastCheck(id string) time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.last[id]
}

// claim reports whether id may be rechecked at now and, if so, records it.
func (g *allCooledGate) claim(id string, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if last, ok := g.last[id]; ok && now.Sub(last) < allCooledTrialGap {
		return false
	}
	if g.last == nil {
		g.last = make(map[string]time.Time)
	}
	g.last[id] = now
	return true
}

// allCooledRound is the per-request state of the recheck round that runs when
// selection fails because every candidate is cooled.
type allCooledRound struct {
	started bool
	probed  bool
	trials  int
	budget  int
	// requestRetry and retryRound say whether a retry round follows this one.
	requestRetry int
	retryRound   int
	// finalErr is the error the request returned before the round existed. It
	// is returned unchanged when no recheck succeeds.
	finalErr error
}

func newAllCooledRound(requestRetry, retryRound int) *allCooledRound {
	return &allCooledRound{budget: max(requestRetry, 0) + 1, requestRetry: requestRetry, retryRound: retryRound}
}

// errOr returns the error captured when the round started, or fallback when
// the round never started.
func (r *allCooledRound) errOr(fallback error) error {
	if r != nil && r.started {
		return r.finalErr
	}
	return fallback
}

// allCooledTrial is one half-open trial in flight.
type allCooledTrial struct {
	authID   string
	provider string
	policy   quotaRecheckPolicy
	identity quotaCooldownIdentity
	verdict  func(http.Header) QuotaVerdictState
	// knownReset is the soonest reset of the cooldown before the trial, used to
	// book the trial in the recheck schedule when it clears.
	knownReset time.Time
}

// allCooledNext runs after selection failed with errPick. finalErr is what the
// request returns without a recheck. It returns a cooled credential to run a
// half-open trial on, reselect when an adapter recheck cleared a cooldown and
// selection should run again, or neither when the request should fail.
func (m *Manager) allCooledNext(ctx context.Context, round *allCooledRound, providers []string, model string, opts cliproxyexecutor.Options, tried map[string]struct{}, errPick, finalErr error) (trial *allCooledTrial, auth *Auth, executor ProviderExecutor, provider string, reselect bool) {
	if m == nil || round == nil || m.HomeEnabled() {
		return nil, nil, nil, "", false
	}
	if !round.started {
		var cooldownErr *modelCooldownError
		if !errors.As(errPick, &cooldownErr) {
			return nil, nil, nil, "", false
		}
		cfg := m.runtimeConfigSnapshot()
		if cfg == nil || cfg.Home.Enabled || len(cfg.QuotaReconcile) == 0 {
			return nil, nil, nil, "", false
		}
		// A credential this request already tried that is still selectable (a
		// dropped connection does not cool it) and that a following retry round
		// will try again means the pool is not all cooled: the request's own
		// retry goes back to it, so no half-open trial is spent on the cooled
		// ones. A failure that cooled it, no retry round left, or a round that
		// ended in a 429 (the next round would wait a quota floor first) keeps
		// the trial.
		if isRequestRetryRoundError(finalErr) && statusCodeFromError(finalErr) != http.StatusTooManyRequests && m.triedCredentialRetriedNextRound(ctx, round, providers, model, opts, tried) {
			return nil, nil, nil, "", false
		}
		round.started = true
		round.finalErr = finalErr
	}
	cfg := m.runtimeConfigSnapshot()
	if cfg == nil {
		return nil, nil, nil, "", false
	}
	gate := &m.quotaReconcile.allCooled
	candidates := m.allCooledCandidates(ctx, cfg, providers, model, opts, tried)

	if !round.probed {
		round.probed = true
		if m.allCooledAdapterRecheck(ctx, cfg, gate, candidates) {
			return nil, nil, nil, "", true
		}
	}

	if round.trials >= round.budget {
		return nil, nil, nil, "", false
	}
	halfOpen := make([]*Auth, 0, len(candidates))
	for _, candidate := range candidates {
		if providerQuotaFor(candidate.Provider).trialVerdict != nil {
			halfOpen = append(halfOpen, candidate)
		}
	}
	// Oldest last check first: a credential nothing has looked at for longest
	// is the likeliest to have reset.
	lastChecked := func(a *Auth) time.Time {
		last := gate.lastCheck(a.ID)
		if a.Quota.ObservedAt.After(last) {
			last = a.Quota.ObservedAt
		}
		return last
	}
	sort.SliceStable(halfOpen, func(i, j int) bool {
		li, lj := lastChecked(halfOpen[i]), lastChecked(halfOpen[j])
		if !li.Equal(lj) {
			return li.Before(lj)
		}
		return halfOpen[i].ID < halfOpen[j].ID
	})
	now := m.quotaReconcile.schedule.now()
	for _, candidate := range halfOpen {
		providerKey := executorKeyFromAuth(candidate)
		exec, ok := m.Executor(providerKey)
		if !ok || !gate.claim(candidate.ID, now) {
			continue
		}
		round.trials++
		key := quotaRecheckProviderKey(candidate.Provider)
		trial = &allCooledTrial{
			authID:     candidate.ID,
			provider:   key,
			policy:     quotaRecheckPolicyFor(cfg, key),
			identity:   newQuotaCooldownIdentity(candidate),
			verdict:    providerQuotaFor(key).trialVerdict(candidate, now),
			knownReset: soonestKnownReset(candidate, now),
		}
		logEntryWithRequestID(ctx).WithFields(log.Fields{"provider": key, "auth_id": candidate.ID}).
			Info("quota recheck: every candidate is cooled, running a half-open trial on the user's request")
		return trial, m.indexedAuthCopy(candidate), exec, providerKey, false
	}
	return nil, nil, nil, "", false
}

// allCooledAdapterRecheck synchronously rechecks cooled candidates whose
// provider has no half-open path but has a registered adapter. It reports
// whether any cooldown was cleared.
func (m *Manager) allCooledAdapterRecheck(ctx context.Context, cfg *internalconfig.Config, gate *allCooledGate, candidates []*Auth) bool {
	cleared := false
	for _, candidate := range candidates {
		key := quotaRecheckProviderKey(candidate.Provider)
		if providerQuotaFor(key).trialVerdict != nil {
			continue
		}
		adapter := m.quotaRecheckAdapter(key)
		if adapter == nil || !gate.claim(candidate.ID, m.quotaReconcile.schedule.now()) {
			continue
		}
		if m.recheckQuotaCooldown(ctx, adapter, quotaRecheckPolicyFor(cfg, key).mode, candidate) {
			cleared = true
		}
	}
	return cleared
}

// triedCredentialRetriedNextRound reports whether tried holds a credential
// of one of providers that is selectable for model now and that a following
// retry round of this request will try again.
func (m *Manager) triedCredentialRetriedNextRound(ctx context.Context, round *allCooledRound, providers []string, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) bool {
	if len(tried) == 0 {
		return false
	}
	providerSet := make(map[string]struct{}, len(providers))
	for _, p := range providers {
		if norm := canonicalSchedulingProvider(p); norm != "" && norm != "mixed" {
			providerSet[norm] = struct{}{}
		}
	}
	now := time.Now()
	m.mu.RLock()
	defer m.mu.RUnlock()
	for id := range tried {
		auth := m.auths[id]
		if auth == nil || round.retryRound >= effectiveRequestRetryLimit(auth, round.requestRetry) {
			continue
		}
		if blocked, _, _ := isAuthBlockedForModelForRequest(ctx, auth, m.selectionModelForAuth(auth, model), now, opts.Headers); blocked {
			continue
		}
		if _, ok := providerSet[executorKeyFromAuth(auth)]; len(providerSet) > 0 && !ok {
			continue
		}
		return true
	}
	return false
}

// allCooledCandidates returns copies of the credentials that could serve model
// for this request but hold a quota cooldown, for providers whose
// quota-reconcile mode is not off.
func (m *Manager) allCooledCandidates(ctx context.Context, cfg *internalconfig.Config, providers []string, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) []*Auth {
	providerSet := make(map[string]struct{}, len(providers))
	for _, p := range providers {
		if norm := canonicalSchedulingProvider(p); norm != "" && norm != "mixed" {
			providerSet[norm] = struct{}{}
		}
	}
	eligibility := authSelectionEligibilityForRequest(ctx, opts)
	pinnedAuthID := pinnedAuthIDFromMetadata(opts.Metadata)
	registryRef := registry.GetGlobalRegistry()
	now := time.Now()

	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Auth, 0)
	for _, candidate := range m.auths {
		if candidate == nil || !authHasQuotaCooldown(candidate) {
			continue
		}
		if quotaRecheckPolicyFor(cfg, candidate.Provider).mode == internalconfig.QuotaReconcileOff {
			continue
		}
		if _, used := tried[candidate.ID]; used {
			continue
		}
		if pinnedAuthID != "" && candidate.ID != pinnedAuthID {
			continue
		}
		providerKey := executorKeyFromAuth(candidate)
		if _, ok := providerSet[providerKey]; len(providerSet) > 0 && !ok {
			continue
		}
		if _, ok := m.executorLocked(providerKey); !ok {
			continue
		}
		if !eligibility.allows(candidate) {
			continue
		}
		if model != "" && !m.authSupportsRouteModel(registryRef, candidate, model) {
			continue
		}
		blocked, reason, _ := isAuthBlockedForModelForRequest(ctx, candidate, m.selectionModelForAuth(candidate, model), now, opts.Headers)
		if !blocked || reason != blockReasonCooldown {
			continue
		}
		out = append(out, candidate.Clone())
	}
	return out
}

// indexedAuthCopy returns a copy of auth with its index assigned, as selection
// does for the credential it picks.
func (m *Manager) indexedAuthCopy(auth *Auth) *Auth {
	if auth.indexAssigned {
		return auth
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if current := m.auths[auth.ID]; current != nil {
		current.EnsureIndex()
		return current.Clone()
	}
	return auth
}

// finishAllCooledTrial clears the trialled credential's cooldown when the
// served response says usage is allowed. The cooldown is cleared only if it is
// still the one observed before the trial.
func (m *Manager) finishAllCooledTrial(ctx context.Context, trial *allCooledTrial, headers http.Header) {
	if m == nil || trial == nil {
		return
	}
	state := trial.verdict(headers)
	entry := logEntryWithRequestID(ctx).WithFields(log.Fields{
		"provider": trial.provider,
		"auth_id":  trial.authID,
		"verdict":  state.String(),
		"source":   "half-open-trial",
	})
	if state != QuotaVerdictAllowed {
		entry.Info("quota recheck: half-open trial served without an allowed status, cooldown kept")
		return
	}
	if trial.policy.mode != internalconfig.QuotaReconcileClear {
		entry.Info("quota recheck: half-open trial says usage is allowed, cooldown kept (observe mode)")
		return
	}
	_, _, errReset := m.resetQuota(ctx, trial.authID, func(current *Auth) bool {
		return newQuotaCooldownIdentity(current) == trial.identity
	})
	if errors.Is(errReset, errQuotaCooldownChanged) {
		entry.Info("quota recheck: cooldown changed during the half-open trial, keeping it")
		return
	}
	if errReset != nil {
		entry.Warnf("quota recheck: reset after half-open trial failed: %v", errReset)
		return
	}
	schedule := &m.quotaReconcile.schedule
	schedule.noteTrialCleared(trial.authID, schedule.now(), trial.policy, trial.knownReset)
	entry.Info("quota recheck: half-open trial says usage is allowed, cleared stale quota cooldown")
}

func quotaRecheckProviderKey(provider string) string {
	return strings.ToLower(strings.TrimSpace(provider))
}
