package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// newRecheckManager returns a manager whose recheck loop runs on a fake clock
// with a fake "claude" adapter returning verdict.
func newRecheckManager(t *testing.T, cfg internalconfig.QuotaReconcileConfig, verdict QuotaVerdictState) (*Manager, *fakeRecheckAdapter, *fakeClock) {
	t.Helper()
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{QuotaReconcile: cfg})
	adapter := &fakeRecheckAdapter{provider: "claude", verdict: QuotaVerdict{State: verdict, Source: "fake"}}
	manager.RegisterQuotaRecheckAdapter(adapter)
	clock := &fakeClock{t: time.Now()}
	manager.quotaReconcile.schedule.clock = clock.now
	return manager, adapter, clock
}

func claudeConfig(mode internalconfig.QuotaReconcileMode) internalconfig.QuotaReconcileConfig {
	return internalconfig.QuotaReconcileConfig{"claude": {Mode: mode}}
}

func TestQuotaRecheckObserveModeDoesNotClear(t *testing.T) {
	manager, adapter, _ := newRecheckManager(t, claudeConfig(internalconfig.QuotaReconcileObserve), QuotaVerdictAllowed)
	registerCooledAuth(t, manager, "claude-1", "claude")

	manager.reconcileQuotaCooldowns(context.Background())

	if adapter.calls != 1 {
		t.Fatalf("verdict calls = %d, want 1", adapter.calls)
	}
	if got, _ := manager.GetByID("claude-1"); !got.Quota.Exceeded {
		t.Fatalf("observe mode cleared the cooldown")
	}
}

func TestQuotaRecheckClearModeClears(t *testing.T) {
	manager, _, _ := newRecheckManager(t, claudeConfig(internalconfig.QuotaReconcileClear), QuotaVerdictAllowed)
	registerCooledAuth(t, manager, "claude-1", "claude")

	manager.reconcileQuotaCooldowns(context.Background())

	if got, _ := manager.GetByID("claude-1"); got.Quota.Exceeded {
		t.Fatalf("clear mode left the cooldown in place")
	}
}

func TestQuotaRecheckOffAndUnconfiguredProvidersAreSkipped(t *testing.T) {
	for name, cfg := range map[string]internalconfig.QuotaReconcileConfig{
		"off":                 claudeConfig(internalconfig.QuotaReconcileOff),
		"other provider only": {"codex": {Mode: internalconfig.QuotaReconcileClear}},
		"no providers at all": nil,
	} {
		t.Run(name, func(t *testing.T) {
			manager, adapter, _ := newRecheckManager(t, cfg, QuotaVerdictAllowed)
			registerCooledAuth(t, manager, "claude-1", "claude")

			manager.reconcileQuotaCooldowns(context.Background())

			if adapter.calls != 0 {
				t.Fatalf("verdict calls = %d, want 0", adapter.calls)
			}
		})
	}
}

func TestQuotaRecheckInactiveInHomeMode(t *testing.T) {
	manager, adapter, _ := newRecheckManager(t, claudeConfig(internalconfig.QuotaReconcileClear), QuotaVerdictAllowed)
	cfg := &internalconfig.Config{QuotaReconcile: claudeConfig(internalconfig.QuotaReconcileClear)}
	cfg.Home.Enabled = true
	manager.SetConfig(cfg)
	registerCooledAuth(t, manager, "claude-1", "claude")

	manager.reconcileQuotaCooldowns(context.Background())

	if adapter.calls != 0 {
		t.Fatalf("recheck ran in Home mode: calls = %d", adapter.calls)
	}
}

func TestQuotaRecheckPolicyDefaults(t *testing.T) {
	cfg := &internalconfig.Config{QuotaReconcile: internalconfig.QuotaReconcileConfig{
		"codex":  {Mode: internalconfig.QuotaReconcileClear},
		"claude": {Mode: internalconfig.QuotaReconcileObserve},
		"custom": {Mode: internalconfig.QuotaReconcileClear, Interval: time.Second, NearResetInterval: 2 * time.Minute, NearResetWindow: 3 * time.Hour},
	}}
	codex := quotaRecheckPolicyFor(cfg, "Codex")
	if codex.interval != quotaProbeMinGap || codex.nearInterval != quotaProbeMinGap || codex.nearWindow != 0 {
		t.Fatalf("codex defaults = %+v, want the original %s minimum gap and no near window", codex, quotaProbeMinGap)
	}
	claude := quotaRecheckPolicyFor(cfg, "claude")
	if claude.mode != internalconfig.QuotaReconcileObserve || claude.interval != 30*time.Minute || claude.nearInterval != 5*time.Minute || claude.nearWindow != time.Hour {
		t.Fatalf("claude defaults = %+v", claude)
	}
	custom := quotaRecheckPolicyFor(cfg, "custom")
	if custom.interval != quotaProbeMinGap || custom.nearInterval != 2*time.Minute || custom.nearWindow != 3*time.Hour {
		t.Fatalf("custom = %+v, want interval floored to %s", custom, quotaProbeMinGap)
	}
	if got := quotaRecheckPolicyFor(cfg, "gemini"); got.mode != internalconfig.QuotaReconcileOff {
		t.Fatalf("unconfigured provider mode = %q, want off", got.mode)
	}
}

func TestQuotaRecheckCodexDefaultKeepsOriginalMinimumGap(t *testing.T) {
	exec := &usageProbeExecutor{status: http.StatusOK, body: `{"rate_limit":{"allowed":false,"limit_reached":true}}`}
	manager, _ := newQuotaCooldownManager(t, exec)
	clock := &fakeClock{t: time.Now()}
	manager.quotaReconcile.schedule.clock = clock.now

	manager.reconcileQuotaCooldowns(context.Background())
	clock.advance(quotaProbeMinGap - time.Second)
	manager.reconcileQuotaCooldowns(context.Background())
	if exec.calls != 1 {
		t.Fatalf("probes inside the minimum gap = %d, want 1", exec.calls)
	}
	clock.advance(time.Second)
	manager.reconcileQuotaCooldowns(context.Background())
	if exec.calls != 2 {
		t.Fatalf("probes after the minimum gap = %d, want 2", exec.calls)
	}
}

func TestQuotaRecheckCadenceFarAndNear(t *testing.T) {
	manager, adapter, clock := newRecheckManager(t, claudeConfig(internalconfig.QuotaReconcileObserve), QuotaVerdictLimited)
	// Known reset 3h away: far cadence (30m) until it is within 1h, then near (5m).
	resetAt := clock.t.Add(3 * time.Hour)
	if _, err := manager.Register(context.Background(), &Auth{
		ID: "claude-1", Provider: "claude", Status: StatusError, Unavailable: true, NextRetryAfter: resetAt,
		Quota: QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: resetAt},
	}); err != nil {
		t.Fatal(err)
	}
	run := func(wantCalls int, at string) {
		t.Helper()
		manager.reconcileQuotaCooldowns(context.Background())
		if adapter.calls != wantCalls {
			t.Fatalf("%s: verdict calls = %d, want %d", at, adapter.calls, wantCalls)
		}
	}

	start := clock.t
	at := func(d time.Duration) { clock.t = start.Add(d) }

	run(1, "first pass")
	at(29 * time.Minute)
	run(1, "inside the far interval")
	at(30 * time.Minute)
	run(2, "far interval elapsed, reset 2.5h away")
	at(35 * time.Minute)
	run(2, "far cadence is not shortened outside the near window")
	at(60 * time.Minute)
	run(3, "far interval elapsed again, reset 2h away")
	at(2 * time.Hour) // reset is exactly one window away
	run(4, "probe on entering the near window")
	at(2*time.Hour + 4*time.Minute)
	run(4, "inside the near interval")
	at(2*time.Hour + 5*time.Minute)
	run(5, "near interval elapsed")
}

func TestQuotaRecheckBackoffDoublesOnFlapAndCaps(t *testing.T) {
	var schedule quotaRecheckSchedule
	clock := &fakeClock{t: time.Now()}
	schedule.clock = clock.now
	policy := quotaRecheckPolicy{interval: 30 * time.Minute, nearInterval: 5 * time.Minute}

	schedule.begin("a", clock.t, policy, time.Time{})
	schedule.noteCleared("a", clock.t)
	clock.advance(9 * time.Minute)
	schedule.noteQuotaRejection("a") // flap inside 10m
	if got := schedule.states["a"].backoffLevel; got != 1 {
		t.Fatalf("backoff level after a flap = %d, want 1", got)
	}
	if got, want := schedule.states["a"].nextCheck, schedule.states["a"].probedAt.Add(time.Hour); !got.Equal(want) {
		t.Fatalf("next check after a flap = %v, want %v (interval doubled)", got, want)
	}

	// A rejection long after the clear is not a flap, and a second rejection for the same clear counts once.
	schedule.noteCleared("a", clock.t)
	clock.advance(11 * time.Minute)
	schedule.noteQuotaRejection("a")
	schedule.noteQuotaRejection("a")
	if got := schedule.states["a"].backoffLevel; got != 1 {
		t.Fatalf("backoff level after a late rejection = %d, want 1", got)
	}

	for i := 0; i < 8; i++ {
		schedule.noteCleared("a", clock.t)
		schedule.noteQuotaRejection("a")
	}
	if got := backedOffInterval(30*time.Minute, schedule.states["a"].backoffLevel); got != quotaBackoffCap {
		t.Fatalf("capped interval = %s, want %s", got, quotaBackoffCap)
	}
}

func TestQuotaRecheckBackoffResetsWhenKnownResetPasses(t *testing.T) {
	var schedule quotaRecheckSchedule
	clock := &fakeClock{t: time.Now()}
	schedule.clock = clock.now
	policy := quotaRecheckPolicy{interval: 30 * time.Minute, nearInterval: 5 * time.Minute}

	schedule.begin("a", clock.t, policy, time.Time{})
	schedule.noteCleared("a", clock.t)
	schedule.noteQuotaRejection("a")
	if schedule.states["a"].backoffLevel != 1 {
		t.Fatal("expected one backoff level")
	}

	reset := clock.t.Add(2 * time.Hour)
	clock.t = schedule.states["a"].nextCheck
	if !schedule.begin("a", clock.t, policy, reset) {
		t.Fatal("not due at its next check")
	}
	if schedule.states["a"].backoffLevel != 1 {
		t.Fatalf("backoff dropped before the known reset passed")
	}
	clock.t = reset.Add(time.Second)
	if !schedule.begin("a", clock.t, policy, time.Time{}) {
		t.Fatal("not due after the reset")
	}
	if got := schedule.states["a"].backoffLevel; got != 0 {
		t.Fatalf("backoff level after the reset = %d, want 0", got)
	}
}

func TestMarkResultQuotaRejectionAfterLoopClearBacksOff(t *testing.T) {
	manager, _, clock := newRecheckManager(t, claudeConfig(internalconfig.QuotaReconcileClear), QuotaVerdictAllowed)
	registerCooledAuth(t, manager, "claude-1", "claude")
	manager.reconcileQuotaCooldowns(context.Background())
	if got, _ := manager.GetByID("claude-1"); got.Quota.Exceeded {
		t.Fatal("setup: cooldown not cleared")
	}

	clock.advance(time.Minute)
	manager.MarkResult(context.Background(), Result{
		AuthID: "claude-1", Provider: "claude", Model: "claude-test", Success: false,
		Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota"},
	})

	if got := manager.quotaReconcile.schedule.states["claude-1"].backoffLevel; got != 1 {
		t.Fatalf("backoff level after a quota 429 following a clear = %d, want 1", got)
	}
}
