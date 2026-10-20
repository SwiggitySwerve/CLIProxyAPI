package helps

import (
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"net/http"
	"testing"
)

func TestFinalCodexServiceTierAccount(t *testing.T) {
	other := &auth.Auth{Provider: "codex", Metadata: map[string]any{"email": "wrollings@astrabit.io"}}
	research := &auth.Auth{Provider: "codex", Metadata: map[string]any{"email": "research@astrabit.io"}}
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
