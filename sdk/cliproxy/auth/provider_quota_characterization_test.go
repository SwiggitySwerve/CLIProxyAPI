package auth

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// These tests pin which provider observes quota and how, per call site, so the
// move to a provider registry (roadmap 3.2) can be shown to change nothing.

func TestProviderQuotaCharacterizationRecheckAdapters(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	for _, provider := range []string{"claude", " Claude "} {
		if _, ok := manager.quotaRecheckAdapter(provider).(claudeQuotaRecheckAdapter); !ok {
			t.Fatalf("adapter for %q = %T, want claudeQuotaRecheckAdapter", provider, manager.quotaRecheckAdapter(provider))
		}
	}
	for _, provider := range []string{"codex", "CODEX"} {
		if _, ok := manager.quotaRecheckAdapter(provider).(codexQuotaRecheckAdapter); !ok {
			t.Fatalf("adapter for %q = %T, want codexQuotaRecheckAdapter", provider, manager.quotaRecheckAdapter(provider))
		}
	}
	for _, provider := range []string{"", "devin", "gemini", "xai", "antigravity", "kimi"} {
		if adapter := manager.quotaRecheckAdapter(provider); adapter != nil {
			t.Fatalf("adapter for %q = %T, want none", provider, adapter)
		}
	}
}

func TestProviderQuotaCharacterizationResetWindowLookup(t *testing.T) {
	observedAt := time.Now()
	reset := strconv.FormatInt(observedAt.Add(2*time.Hour).Unix(), 10)
	claude := map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": reset}
	codex := map[string]string{"X-Codex-Secondary-Reset-At": reset}
	for _, tc := range []struct {
		provider string
		signals  map[string]string
		window   string
	}{
		{"claude", claude, "7d"},
		{" CLAUDE ", claude, "7d"},
		{"codex", codex, "secondary"},
		{"Codex", codex, "secondary"},
		{"claude", codex, ""},
		{"codex", claude, ""},
		{"devin", claude, ""},
		{"devin", codex, ""},
		{"gemini", claude, ""},
		{"xai", codex, ""},
		{"", claude, ""},
	} {
		_, window, ok := soonestResetWindow(tc.provider, tc.signals, observedAt, "claude-sonnet")
		if ok != (tc.window != "") || window != tc.window {
			t.Fatalf("soonestResetWindow(%q) = (%q, %v), want %q", tc.provider, window, ok, tc.window)
		}
	}
}

func TestProviderQuotaCharacterizationObservationKeepsOrClearsSnapshot(t *testing.T) {
	headers := http.Header{
		"Anthropic-Ratelimit-Unified-Status": {"allowed"},
		"X-Codex-Primary-Used-Percent":       {"12"},
		"Retry-After":                        {"30"},
	}
	prior := time.Now().Add(-time.Minute)
	now := time.Now()
	for _, tc := range []struct {
		provider    string
		wantChanged bool
		wantSignals map[string]string
	}{
		// Observing providers replace the snapshot with their own headers only.
		{"claude", true, map[string]string{"Anthropic-Ratelimit-Unified-Status": "allowed", "Retry-After": "30"}},
		{"codex", true, map[string]string{"X-Codex-Primary-Used-Percent": "12", "Retry-After": "30"}},
		// Devin observes but no header is on its allowlist, so the old snapshot stays.
		{"devin", false, map[string]string{"Old": "1"}},
		// Non-observing providers drop any snapshot they hold.
		{"gemini", true, nil},
		{"xai", true, nil},
	} {
		q := QuotaState{Signals: map[string]string{"Old": "1"}, ObservedAt: prior}
		changed := q.ObserveResponseHeadersForProvider(tc.provider, headers, now)
		if changed != tc.wantChanged {
			t.Fatalf("%s: changed = %v, want %v", tc.provider, changed, tc.wantChanged)
		}
		if len(q.Signals) != len(tc.wantSignals) {
			t.Fatalf("%s: signals = %v, want %v", tc.provider, q.Signals, tc.wantSignals)
		}
		for key, val := range tc.wantSignals {
			if q.Signals[key] != val {
				t.Fatalf("%s: signals = %v, want %v", tc.provider, q.Signals, tc.wantSignals)
			}
		}
	}
}

// Claude takes the half-open trial in the all-cooled round, so even a
// registered adapter's Verdict is not called. Codex takes the adapter path
// (TestAllCooledAdapterRecheckClearsAndReselects).
func TestProviderQuotaCharacterizationAllCooledClaudeUsesTrialNotAdapter(t *testing.T) {
	f := newAllCooledFixture(t, nil, internalconfig.QuotaReconcileClear, "claude-a")
	adapter := &fakeRecheckAdapter{provider: "claude", verdict: QuotaVerdict{State: QuotaVerdictAllowed, Source: "fake"}}
	f.manager.RegisterQuotaRecheckAdapter(adapter)
	f.cool(t, "claude-a")
	f.executor.lift("claude-a")

	if _, err := f.execute(cliproxyexecutor.Options{}); err != nil {
		t.Fatalf("Execute error = %v, want the trial to serve the request", err)
	}
	if adapter.calls != 0 {
		t.Fatalf("adapter verdict calls = %d, want 0 (half-open trial path)", adapter.calls)
	}
	if f.executor.callCount("claude-a") == 0 {
		t.Fatal("claude-a was never trialled")
	}
}
