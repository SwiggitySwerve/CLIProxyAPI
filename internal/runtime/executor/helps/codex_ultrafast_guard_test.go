package helps

import (
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"net/http"
	"testing"
)

func TestFinalCodexServiceTierAccount(t *testing.T) {
	auth.SetUltrafastPolicy(config.UltrafastConfig{AccountIDs: []string{"acct-ultrafast-0001"}})
	t.Cleanup(func() { auth.SetUltrafastPolicy(config.UltrafastConfig{}) })
	other := &auth.Auth{Provider: "codex", Metadata: map[string]any{"account_id": "acct-other-0002"}}
	research := &auth.Auth{Provider: "codex", Metadata: map[string]any{"account_id": "acct-ultrafast-0001"}}
	for _, payload := range []string{
		`{"service_tier":"priority","service_tier":"ultrafast"}`,
		`{"service_tier":"ultrafast","service_tier":"priority"}`,
		`{"service_tier":"priority","service_\u0074ier":"ultrafast"}`,
	} {
		if err := ValidateCodexServiceTier(other, []byte(payload)); err == nil {
			t.Fatalf("duplicate tier escaped: %s", payload)
		}
	}
	if err := ValidateCodexServiceTier(other, []byte(`{"service_tier":"ultrafast"}`)); err == nil {
		t.Fatal("final ultrafast override or WebSocket continuation must reject another account")
	}
	if err := ValidateCodexServiceTier(research, []byte(`{"service_tier":"ultrafast"}`)); err != nil {
		t.Fatal(err)
	}
	if err := ValidateCodexServiceTier(other, []byte(`{"service_tier":"priority"}`)); err != nil {
		t.Fatal(err)
	}
	if err := ValidateCodexServiceTier(other, []byte(`{}`), http.Header{"X-Codex-Routing-Hint": {"model=gpt-5.5;tier=ultrafast"}}); err == nil {
		t.Fatal("operator header override must reject another account")
	}
	if err := ValidateCodexServiceTier(other, []byte(`{}`), http.Header{"x-codex-routing-hint": {"model=gpt-5.5;tier=ultrafast"}}); err == nil {
		t.Fatal("case-preserved routing header must also reject another account")
	}
}

func TestFinalCodexServiceTierUnsetPolicyDeniesEveryone(t *testing.T) {
	auth.SetUltrafastPolicy(config.UltrafastConfig{})
	holder := &auth.Auth{Provider: "codex", Metadata: map[string]any{"account_id": "acct-ultrafast-0001"}}
	if err := ValidateCodexServiceTier(holder, []byte(`{"service_tier":"ultrafast"}`)); err == nil {
		t.Fatal("an unset policy must deny ultrafast on every credential")
	}
	if err := ValidateCodexServiceTier(holder, []byte(`{}`), http.Header{"X-Codex-Routing-Hint": {"model=gpt-5.5;tier=ultrafast"}}); err == nil {
		t.Fatal("an unset policy must deny the routing-hint tier on every credential")
	}
	if err := ValidateCodexServiceTier(holder, []byte(`{"service_tier":"priority"}`)); err != nil {
		t.Fatalf("other tiers must stay allowed: %v", err)
	}
}
