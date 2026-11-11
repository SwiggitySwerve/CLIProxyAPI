package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// hookedUsageProbeExecutor runs onCall while the usage probe is in flight.
type hookedUsageProbeExecutor struct {
	*usageProbeExecutor
	onCall func()
}

func (e *hookedUsageProbeExecutor) HttpRequest(ctx context.Context, auth *Auth, req *http.Request) (*http.Response, error) {
	if e.onCall != nil {
		e.onCall()
	}
	return e.usageProbeExecutor.HttpRequest(ctx, auth, req)
}

type fakeRecheckAdapter struct {
	provider string
	verdict  QuotaVerdict
	err      error
	calls    int
}

func (a *fakeRecheckAdapter) Provider() string { return a.provider }
func (a *fakeRecheckAdapter) UsageSnapshot(context.Context, QuotaRecheckHost, *Auth) (QuotaUsageSnapshot, error) {
	return QuotaUsageSnapshot{}, nil
}
func (a *fakeRecheckAdapter) Verdict(context.Context, QuotaRecheckHost, *Auth) (QuotaVerdict, error) {
	a.calls++
	return a.verdict, a.err
}

func registerCooledAuth(t *testing.T, manager *Manager, id, provider string) {
	t.Helper()
	next := time.Now().Add(100 * time.Hour)
	if _, err := manager.Register(context.Background(), &Auth{
		ID: id, Provider: provider, Status: StatusError, Unavailable: true, NextRetryAfter: next,
		Quota: QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: next},
	}); err != nil {
		t.Fatalf("register %s: %v", id, err)
	}
}

func TestReconcileQuotaCooldownsDispatchesByProvider(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{QuotaReconcile: internalconfig.QuotaReconcileConfig{
		"claude": {Mode: internalconfig.QuotaReconcileClear}, "nobody": {Mode: internalconfig.QuotaReconcileClear},
	}})
	adapter := &fakeRecheckAdapter{provider: "Claude", verdict: QuotaVerdict{State: QuotaVerdictAllowed, Source: "fake"}}
	manager.RegisterQuotaRecheckAdapter(adapter)
	registerCooledAuth(t, manager, "claude-1", "claude")
	registerCooledAuth(t, manager, "nobody-1", "nobody")

	manager.reconcileQuotaCooldowns(context.Background())

	if adapter.calls != 1 {
		t.Fatalf("adapter calls = %d, want 1 (only the claude credential)", adapter.calls)
	}
	if got, _ := manager.GetByID("claude-1"); got.Quota.Exceeded {
		t.Fatalf("claude cooldown not cleared by an allowed verdict")
	}
	if got, _ := manager.GetByID("nobody-1"); !got.Quota.Exceeded {
		t.Fatalf("provider without an adapter was cleared")
	}
}

func TestReconcileQuotaCooldownsOnlyAllowedVerdictClears(t *testing.T) {
	for name, tc := range map[string]*fakeRecheckAdapter{
		"limited": {provider: "claude", verdict: QuotaVerdict{State: QuotaVerdictLimited}},
		"unknown": {provider: "claude", verdict: QuotaVerdict{State: QuotaVerdictUnknown}},
		"error":   {provider: "claude", err: context.DeadlineExceeded},
	} {
		t.Run(name, func(t *testing.T) {
			manager := NewManager(nil, nil, nil)
			manager.SetConfig(&internalconfig.Config{QuotaReconcile: internalconfig.QuotaReconcileConfig{
				"claude": {Mode: internalconfig.QuotaReconcileClear},
			}})
			manager.RegisterQuotaRecheckAdapter(tc)
			registerCooledAuth(t, manager, "claude-1", "claude")

			manager.reconcileQuotaCooldowns(context.Background())

			if got, _ := manager.GetByID("claude-1"); !got.Quota.Exceeded {
				t.Fatalf("cooldown cleared on a %s verdict", name)
			}
		})
	}
}

func TestQuotaCooldownIdentityChangesWithRecoverTimeVersionAndEpoch(t *testing.T) {
	next := time.Now().Add(time.Hour)
	base := &Auth{Quota: QuotaState{Exceeded: true, NextRecoverAt: next}, CredentialVersion: 1, RegistrationEpoch: 1}
	want := newQuotaCooldownIdentity(base)
	if newQuotaCooldownIdentity(base.Clone()) != want {
		t.Fatalf("identity differs for an identical auth")
	}
	for name, mutate := range map[string]func(*Auth){
		"recover time": func(a *Auth) { a.Quota.NextRecoverAt = next.Add(time.Minute) },
		"version":      func(a *Auth) { a.CredentialVersion++ },
		"epoch":        func(a *Auth) { a.RegistrationEpoch++ },
		"model cooldown": func(a *Auth) {
			a.ModelStates = map[string]*ModelState{"m": {Quota: QuotaState{Exceeded: true, NextRecoverAt: next}}}
		},
	} {
		changed := base.Clone()
		mutate(changed)
		if newQuotaCooldownIdentity(changed) == want {
			t.Errorf("identity unchanged after %s changed", name)
		}
	}
}

func TestReconcileQuotaCooldownsKeepsCooldownSetDuringProbe(t *testing.T) {
	exec := &hookedUsageProbeExecutor{usageProbeExecutor: &usageProbeExecutor{
		status: http.StatusOK, body: `{"rate_limit":{"allowed":true,"limit_reached":false}}`,
	}}
	manager, authID := newQuotaCooldownManager(t, exec.usageProbeExecutor)
	manager.RegisterExecutor(exec)
	newer := time.Now().Add(200 * time.Hour)
	exec.onCall = func() {
		_, _, err := manager.Modify(context.Background(), authID, 0, func(a *Auth) error {
			a.Quota.NextRecoverAt = newer
			a.NextRetryAfter = newer
			return nil
		})
		if err != nil {
			t.Errorf("modify: %v", err)
		}
	}

	manager.reconcileQuotaCooldowns(context.Background())

	got, _ := manager.GetByID(authID)
	if !got.Quota.Exceeded || !got.Quota.NextRecoverAt.Equal(newer) {
		t.Fatalf("cooldown set during the probe was cleared: exceeded=%v next=%v", got.Quota.Exceeded, got.Quota.NextRecoverAt)
	}
}
