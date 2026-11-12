package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

func TestCredentialQuotaKeepsHealthyCatalogAcrossRestart(t *testing.T) {
	withQuotaCooldownEnabled(t)
	ctx := context.Background()
	m := NewManager(nil, nil, nil)
	store := NewFileCooldownStateStore(t.TempDir())
	m.SetCooldownStateStore(store)
	r := registry.GetGlobalRegistry()
	models := []*registry.ModelInfo{{ID: "audit-luna"}, {ID: "audit-sol"}, {ID: "audit-terra"}}
	for _, id := range []string{"audit-healthy-api", "audit-quota-oauth"} {
		if _, err := m.Register(ctx, &Auth{ID: id, Provider: "codex", Status: StatusActive}); err != nil {
			t.Fatal(err)
		}
		r.RegisterClient(id, "codex", models)
		t.Cleanup(func() { r.UnregisterClient(id) })
	}
	for _, model := range models {
		m.MarkResult(ctx, Result{AuthID: "audit-quota-oauth", Provider: "codex", Model: model.ID, Success: true})
	}
	retry := 3 * time.Hour
	m.MarkResult(ctx, Result{
		AuthID: "audit-quota-oauth", Provider: "codex", Model: "audit-sol",
		Error:      &Error{HTTPStatus: http.StatusTooManyRequests, Code: "usage_limit_reached", Message: "quota exceeded"},
		RetryAfter: &retry, CredentialScope: true,
	})
	oauth, _ := m.GetByID("audit-quota-oauth")
	listed := map[string]bool{}
	for _, model := range r.GetAvailableModels("openai") {
		id, _ := model["id"].(string)
		listed[id] = true
	}
	for _, model := range models {
		blocked, reason, next := isAuthBlockedForModel(oauth, model.ID, time.Now())
		if !blocked || reason != blockReasonCooldown || next.Before(time.Now().Add(retry-time.Minute)) {
			t.Fatalf("model %s on the quota credential: blocked=%v reason=%v until %v, want the 3h credential quota", model.ID, blocked, reason, next)
		}
		t.Logf("model=%s OAuth_blocked_until=%s healthy_provider_blocked=%v listed=%v",
			model.ID, next.Format(time.RFC3339), false, listed[model.ID])
		wantListed := true
		if listed[model.ID] != wantListed {
			t.Errorf("healthy catalog lost %s: listed=%v, want %v", model.ID, listed[model.ID], wantListed)
		}
	}
	healthy, _ := m.GetByID("audit-healthy-api")
	for _, model := range models {
		blocked, _, _ := isAuthBlockedForModel(healthy, model.ID, time.Now())
		if blocked {
			t.Fatalf("healthy API provider unexpectedly blocked for %s", model.ID)
		}
	}
	// Simulate the same service restart sequence using the real .cds store.
	for _, id := range []string{"audit-healthy-api", "audit-quota-oauth"} {
		r.UnregisterClient(id)
	}
	restarted := NewManager(nil, nil, nil)
	for _, id := range []string{"audit-healthy-api", "audit-quota-oauth"} {
		if _, err := restarted.Register(ctx, &Auth{ID: id, Provider: "codex", Status: StatusActive}); err != nil {
			t.Fatal(err)
		}
		r.RegisterClient(id, "codex", models)
	}
	restarted.SetCooldownStateStore(store)
	if err := restarted.RestoreCooldownStates(ctx); err != nil {
		t.Fatal(err)
	}
	restarted.ReconcileRegistryModelStates(ctx, "audit-quota-oauth")
	restored, _ := restarted.GetByID("audit-quota-oauth")
	if restored.Quota.Reason != "credential_quota" || !restored.Quota.NextRecoverAt.After(time.Now()) {
		t.Fatal("restart did not restore the active credential quota")
	}
	listedAfterRestart := map[string]bool{}
	for _, model := range r.GetAvailableModels("openai") {
		id, _ := model["id"].(string)
		listedAfterRestart[id] = true
	}
	if !listedAfterRestart["audit-luna"] || !listedAfterRestart["audit-terra"] || !listedAfterRestart["audit-sol"] {
		t.Fatalf("restart hid a healthy provider: %v", listedAfterRestart)
	}
	t.Log("real .cds persistence + restart + reconciliation keeps all models with a healthy provider visible")
}
