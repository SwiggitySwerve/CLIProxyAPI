package management

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// TestListAuthFilesCredentialQuotaScope pins what the credential listing
// reports for a credential-scoped 429 (a Claude unified rejection) recorded on
// one model while siblings hold their own states: N healthy, P its own 12h 404,
// Q its own 10m model-scoped 429.
func TestListAuthFilesCredentialQuotaScope(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	manager := coreauth.NewManager(nil, nil, nil)
	cfg := &config.Config{AuthDir: t.TempDir()}
	manager.SetConfig(cfg)
	registerAuthForLookupTest(t, manager, &coreauth.Auth{
		ID: "scope", Index: "index-scope", Provider: "claude", Status: coreauth.StatusActive,
		Attributes: map[string]string{"runtime_only": "true"},
	})
	mark := func(result coreauth.Result) {
		result.AuthID = "scope"
		result.Provider = "claude"
		manager.MarkResult(context.Background(), result)
	}
	start := time.Now()
	tenMinutes, fiveHours := 10*time.Minute, 5*time.Hour
	mark(coreauth.Result{Model: "n", Success: true})
	mark(coreauth.Result{Model: "p", Error: &coreauth.Error{HTTPStatus: http.StatusNotFound, Message: "not found"}})
	mark(coreauth.Result{Model: "q", RetryAfter: &tenMinutes, Error: &coreauth.Error{HTTPStatus: http.StatusTooManyRequests, Message: "model rate limit"}})
	mark(coreauth.Result{Model: "m", CredentialScope: true, RetryAfter: &fiveHours,
		Error: &coreauth.Error{Code: coreauth.ErrorCodeCredentialQuota, HTTPStatus: http.StatusTooManyRequests, Message: "rate_limit_error"}})

	near := func(got, want time.Time) bool {
		diff := got.Sub(want)
		return diff > -30*time.Second && diff < 30*time.Second
	}
	credUntil, pUntil := start.Add(fiveHours), start.Add(12*time.Hour)

	h := NewHandlerWithoutConfigFilePath(cfg, manager)
	payload := requestAuthFilesCooldowns(t, h, "")
	if len(payload.Files) != 1 {
		t.Fatalf("files = %+v", payload.Files)
	}
	file := payload.Files[0]
	if file.Status != string(coreauth.StatusError) || !file.Unavailable || !near(file.NextRetryAfter, credUntil) || file.StatusMessage != "rate_limit_error" {
		t.Fatalf("credential fields = status %q unavailable %v next %v message %q", file.Status, file.Unavailable, file.NextRetryAfter, file.StatusMessage)
	}
	var views []coreauth.CooldownView
	if errDecode := json.Unmarshal(file.Cooldowns, &views); errDecode != nil {
		t.Fatal(errDecode)
	}
	byModel := map[string]coreauth.CooldownView{}
	credentialViews := 0
	for _, view := range views {
		if view.Scope == "credential" {
			credentialViews++
			if view.Reason != "credential_quota" || !near(view.RetryAt, credUntil) {
				t.Fatalf("credential view = %+v", view)
			}
			continue
		}
		byModel[view.ModelKey] = view
	}
	if credentialViews != 1 || len(byModel) != 4 {
		t.Fatalf("views = %s, want one credential view and model views for m, n, p, q", file.Cooldowns)
	}
	for _, model := range []string{"m", "n", "q"} {
		view := byModel[model]
		if (view.Reason != "quota" && view.Reason != "credential_quota") || !near(view.RetryAt, credUntil) {
			t.Fatalf("%s view = %+v, want a quota reason until %v", model, view, credUntil)
		}
	}
	for _, model := range []string{"n", "q"} {
		if byModel[model].Reason != "credential_quota" {
			t.Fatalf("%s view reason = %q, want credential_quota", model, byModel[model].Reason)
		}
	}
	if view := byModel["p"]; view.Reason != "not_found" || !near(view.RetryAt, pUntil) {
		t.Fatalf("p view = %+v, want not_found until %v", view, pUntil)
	}

	// After the quota ends, only P's own block is left, so the credential is
	// listed as available.
	auth, _ := manager.GetByID("scope")
	unavailable, status, message, next := reconcileAuthFileCooldownState(auth, credUntil.Add(time.Minute))
	if unavailable || status != coreauth.StatusActive || message != "" || !next.IsZero() {
		t.Fatalf("after the quota: unavailable=%v status=%s message=%q next=%v, want available", unavailable, status, message, next)
	}
}
