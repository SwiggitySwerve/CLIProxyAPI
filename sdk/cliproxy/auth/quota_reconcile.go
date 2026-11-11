package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MoeForge/moego/ojson"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	log "github.com/sirupsen/logrus"
)

const (
	codexUsageProbeURL         = "https://chatgpt.com/backend-api/wham/usage"
	defaultQuotaReconcileEvery = 30 * time.Second
	quotaProbeMinGap           = 10 * time.Second
	quotaProbeTimeout          = 20 * time.Second
	quotaProbeMaxBody          = 1 << 20
)

// quotaReconciler keeps quota cooldowns in step with the provider's own usage
// report. A usage_limit_reached response stores a fixed recovery time, but the
// provider can lift the limit earlier (manual reset, plan change, window
// rollover). Without reconciliation the stale cooldown blocks the credential
// until that stored time passes.
type quotaReconciler struct {
	started  atomic.Bool
	wake     atomic.Pointer[chan struct{}]
	schedule quotaRecheckSchedule
	adapters sync.Map // lowercase provider -> QuotaRecheckAdapter
	// allCooled spaces the synchronous rechecks run when every candidate is cooled.
	allCooled allCooledGate
}

// StartQuotaReconciler launches a loop that, for each provider enabled in quota-reconcile, rechecks credentials
// sitting in a quota cooldown and, in clear mode, clears the cooldown once the provider reports usage is
// allowed. It also runs when a request is rejected because of a cooldown.
func (m *Manager) StartQuotaReconciler(parent context.Context, interval time.Duration) {
	if m == nil || !m.quotaReconcile.started.CompareAndSwap(false, true) {
		return
	}
	if interval <= 0 {
		interval = defaultQuotaReconcileEvery
	}
	wake := make(chan struct{}, 1)
	m.quotaReconcile.wake.Store(&wake)
	go func() {
		defer m.quotaReconcile.started.Store(false)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-parent.Done():
				return
			case <-ticker.C:
			case <-wake:
			}
			m.reconcileQuotaCooldowns(parent)
		}
	}()
}

// requestQuotaReconcile asks the reconciler to run soon. It never blocks.
func (m *Manager) requestQuotaReconcile() {
	if m == nil {
		return
	}
	if wake := m.quotaReconcile.wake.Load(); wake != nil {
		select {
		case *wake <- struct{}{}:
		default:
		}
	}
}

func authHasQuotaCooldown(auth *Auth) bool {
	if auth == nil || auth.Disabled || auth.Status == StatusDisabled {
		return false
	}
	if auth.Quota.Exceeded {
		return true
	}
	for _, state := range auth.ModelStates {
		if state != nil && state.Quota.Exceeded {
			return true
		}
	}
	return false
}

func (m *Manager) reconcileQuotaCooldowns(ctx context.Context) {
	// Home mode dispatches credentials from the control plane, so the loop stays idle.
	cfg := m.runtimeConfigSnapshot()
	if cfg == nil || cfg.Home.Enabled || len(cfg.QuotaReconcile) == 0 {
		return
	}
	schedule := &m.quotaReconcile.schedule
	for _, auth := range m.List() {
		if ctx.Err() != nil {
			return
		}
		if auth == nil || !authHasQuotaCooldown(auth) {
			continue
		}
		adapter := m.quotaRecheckAdapter(auth.Provider)
		policy := quotaRecheckPolicyFor(cfg, auth.Provider)
		if adapter == nil || policy.mode == internalconfig.QuotaReconcileOff {
			continue
		}
		now := schedule.now()
		knownReset := soonestKnownReset(auth, now)
		if !schedule.begin(auth.ID, now, policy, knownReset) {
			// Not due on the timer: an advisory usage snapshot may still ask
			// for an early verdict. The snapshot itself never clears.
			if !m.quotaUsageSnapshotChanged(ctx, adapter, auth) ||
				!schedule.beginTriggered(auth.ID, schedule.now(), policy, knownReset) {
				continue
			}
		}

		m.recheckQuotaCooldown(ctx, adapter, policy.mode, auth)
	}
}

// recheckQuotaCooldown asks adapter for auth's verdict and, in clear mode,
// clears the cooldown when the provider says usage is allowed. It reports
// whether the cooldown was cleared.
func (m *Manager) recheckQuotaCooldown(ctx context.Context, adapter QuotaRecheckAdapter, mode internalconfig.QuotaReconcileMode, auth *Auth) bool {
	schedule := &m.quotaReconcile.schedule
	// Capture the cooldown the probe is about to judge so a newer one set
	// while the probe is in flight is not cleared by a stale verdict.
	identity := newQuotaCooldownIdentity(auth)
	verdict, err := m.quotaRecheckVerdict(ctx, adapter, auth)
	if err != nil {
		if mode == internalconfig.QuotaReconcileObserve {
			log.WithFields(log.Fields{
				"provider":    strings.ToLower(strings.TrimSpace(auth.Provider)),
				"auth_id":     auth.ID,
				"verdict":     QuotaVerdictUnknown.String(),
				"reason":      err.Error(),
				"cooldown_to": auth.Quota.NextRecoverAt,
			}).Info("quota recheck: observed verdict, cooldown kept (observe mode)")
			return false
		}
		log.Debugf("quota reconcile: probe failed for %s: %v", auth.ID, err)
		return false
	}
	if mode == internalconfig.QuotaReconcileObserve {
		log.WithFields(log.Fields{
			"provider":    strings.ToLower(strings.TrimSpace(auth.Provider)),
			"auth_id":     auth.ID,
			"verdict":     verdict.State.String(),
			"source":      verdict.Source,
			"signals":     summarizeQuotaSignals(verdict.Signals),
			"cooldown_to": auth.Quota.NextRecoverAt,
		}).Info("quota recheck: observed verdict, cooldown kept (observe mode)")
		return false
	}
	if mode != internalconfig.QuotaReconcileClear || verdict.State != QuotaVerdictAllowed {
		return false
	}
	_, _, errReset := m.resetQuota(ctx, auth.ID, func(current *Auth) bool {
		return newQuotaCooldownIdentity(current) == identity
	})
	if errors.Is(errReset, errQuotaCooldownChanged) {
		log.Infof("quota reconcile: cooldown for %s changed during the probe, keeping it", auth.ID)
		return false
	}
	if errReset != nil {
		log.Warnf("quota reconcile: reset failed for %s: %v", auth.ID, errReset)
		return false
	}
	schedule.noteCleared(auth.ID, schedule.now())
	log.Infof("quota reconcile: provider reports usage allowed, cleared stale quota cooldown for %s", auth.ID)
	return true
}

// quotaUsageSnapshotChanged polls the adapter's advisory usage snapshot on its
// own cadence and reports whether it changed in a way that warrants an early
// verdict. A failed snapshot is an unknown reading: the baseline is kept and the
// next snapshot backs off.
func (m *Manager) quotaUsageSnapshotChanged(ctx context.Context, adapter QuotaRecheckAdapter, auth *Auth) bool {
	trigger, ok := adapter.(quotaSnapshotTrigger)
	if !ok {
		return false
	}
	schedule := &m.quotaReconcile.schedule
	interval := trigger.snapshotInterval()
	if !schedule.beginSnapshot(auth.ID, schedule.now(), interval) {
		return false
	}
	snapCtx, cancel := context.WithTimeout(ctx, quotaProbeTimeout)
	snapshot, err := adapter.UsageSnapshot(snapCtx, m, auth)
	cancel()
	if err != nil {
		wait := schedule.noteSnapshotFailure(auth.ID, schedule.now(), interval)
		log.Debugf("quota recheck: usage snapshot unknown for %s, next in %s: %v", auth.ID, wait, err)
		return false
	}
	if !schedule.noteSnapshot(auth.ID, snapshot.Signals, trigger.snapshotChanged) {
		return false
	}
	log.Infof("quota recheck: usage snapshot changed for %s, requesting an early verdict", auth.ID)
	return true
}

func (m *Manager) quotaRecheckVerdict(ctx context.Context, adapter QuotaRecheckAdapter, auth *Auth) (QuotaVerdict, error) {
	probeCtx, cancel := context.WithTimeout(ctx, quotaProbeTimeout)
	defer cancel()
	return adapter.Verdict(probeCtx, m, auth)
}

// codexUsageProbeURLFor returns the usage endpoint next to the credential's
// configured base URL (for example a staging mock), or the default. A base URL
// ending in the Codex API path "/codex" has it removed, as wham/usage is its
// sibling.
func codexUsageProbeURLFor(auth *Auth) string {
	var base string
	if auth != nil && auth.Attributes != nil {
		base = strings.TrimRight(strings.TrimSpace(auth.Attributes["base_url"]), "/")
	}
	if base == "" {
		return codexUsageProbeURL
	}
	return strings.TrimSuffix(base, "/codex") + "/wham/usage"
}

// codexQuotaRecheckAdapter checks Codex credentials against the usage report.
type codexQuotaRecheckAdapter struct{}

func (codexQuotaRecheckAdapter) Provider() string { return "codex" }

// UsageSnapshot returns the quota snapshot already stored from the last
// response; it makes no upstream call.
func (codexQuotaRecheckAdapter) UsageSnapshot(_ context.Context, _ QuotaRecheckHost, auth *Auth) (QuotaUsageSnapshot, error) {
	return QuotaUsageSnapshot{Signals: auth.Quota.Clone().Signals, ObservedAt: auth.Quota.ObservedAt}, nil
}

// Verdict reports whether the provider says the credential may make requests
// right now. An unreadable or partial answer is an error so the existing
// cooldown is kept.
func (codexQuotaRecheckAdapter) Verdict(ctx context.Context, host QuotaRecheckHost, auth *Auth) (QuotaVerdict, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, codexUsageProbeURLFor(auth), nil)
	if err != nil {
		return QuotaVerdict{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "codex_cli_rs/0.164.0")
	if accountID, _ := auth.Metadata["account_id"].(string); strings.TrimSpace(accountID) != "" {
		req.Header.Set("Chatgpt-Account-Id", strings.TrimSpace(accountID))
	}
	resp, err := host.HttpRequest(ctx, auth, req)
	if err != nil {
		return QuotaVerdict{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, quotaProbeMaxBody))
	if err != nil {
		return QuotaVerdict{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return QuotaVerdict{}, fmt.Errorf("usage probe returned HTTP %d", resp.StatusCode)
	}
	allowed := ojson.GetBytes(body, "rate_limit.allowed")
	limitReached := ojson.GetBytes(body, "rate_limit.limit_reached")
	if !allowed.Exists() || !limitReached.Exists() {
		return QuotaVerdict{}, fmt.Errorf("usage probe response has no rate_limit state")
	}
	verdict := QuotaVerdict{State: QuotaVerdictLimited, ObservedAt: time.Now(), Source: "wham-usage"}
	if allowed.Bool() && !limitReached.Bool() {
		verdict.State = QuotaVerdictAllowed
	}
	return verdict, nil
}
