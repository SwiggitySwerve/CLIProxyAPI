package auth

import (
	"fmt"
	"maps"
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

// Slice 2: the per-provider header allowlist, retention order, Claude rejected
// signals and recheck cadence.

func TestProviderQuotaCharacterizationSignalAllowlist(t *testing.T) {
	headers := http.Header{
		"Retry-After":                                    {"30"},
		"Anthropic-Ratelimit-Unified-Status":             {"allowed"},
		"Anthropic-Ratelimit-Unified-5h-Reset":           {"1700000000"},
		"Anthropic-Ratelimit-Requests-Remaining":         {"9"},
		"X-Ratelimit-Remaining-Requests":                 {"8"},
		"X-Codex-Plan-Type":                              {"pro"},
		"X-Codex-Active-Limit":                           {"codex"},
		"X-Codex-Credits-Balance":                        {"5"},
		"X-Codex-Primary-Used-Percent":                   {"12"},
		"X-Codex-Secondary-Over-Secondary-Limit-Percent": {"0"},
		"X-Codex-Bengalfox-Primary-Window-Minutes":       {"300"},
		"X-Codex-Additional-Foo-Primary-Reset-At":        {"1700000001"},
		"X-Codex-Primary-Limit-Name":                     {"x"},
		"X-Codex-Request-Id":                             {"r"},
		"Content-Type":                                   {"application/json"},
	}
	claude := map[string]string{
		"Retry-After":                          "30",
		"Anthropic-Ratelimit-Unified-Status":   "allowed",
		"Anthropic-Ratelimit-Unified-5h-Reset": "1700000000",
	}
	codex := map[string]string{
		"Retry-After":                                    "30",
		"X-Ratelimit-Remaining-Requests":                 "8",
		"X-Codex-Plan-Type":                              "pro",
		"X-Codex-Active-Limit":                           "codex",
		"X-Codex-Credits-Balance":                        "5",
		"X-Codex-Primary-Used-Percent":                   "12",
		"X-Codex-Secondary-Over-Secondary-Limit-Percent": "0",
		"X-Codex-Bengalfox-Primary-Window-Minutes":       "300",
		"X-Codex-Additional-Foo-Primary-Reset-At":        "1700000001",
		"X-Codex-Primary-Limit-Name":                     "x",
	}
	for _, tc := range []struct {
		provider string
		want     map[string]string
	}{
		{"claude", claude}, {" CLAUDE ", claude}, {"codex", codex}, {"Codex", codex},
		{"devin", nil}, {"gemini", nil}, {"xai", nil}, {"", nil},
	} {
		if got := collectQuotaSignals(tc.provider, headers); !maps.Equal(got, tc.want) {
			t.Fatalf("collectQuotaSignals(%q) = %v, want %v", tc.provider, got, tc.want)
		}
	}
}

// Past the 64-header cap, ranked headers are kept first and ties go by name.
func TestProviderQuotaCharacterizationSignalRetentionOrder(t *testing.T) {
	codexHeaders := http.Header{
		"Retry-After":                              {"1"},
		"X-Codex-Plan-Type":                        {"1"},
		"X-Codex-Primary-Used-Percent":             {"1"},
		"X-Codex-Code-Review-Primary-Used-Percent": {"1"},
		"X-Codex-Zz-Primary-Used-Percent":          {"1"},
		"X-Ratelimit-Limit-Requests":               {"1"},
	}
	claudeHeaders := http.Header{"Retry-After": {"1"}}
	for i := range 70 {
		codexHeaders.Set(fmt.Sprintf("X-Codex-Additional-L%02d-Primary-Used-Percent", i), "1")
		claudeHeaders.Set(fmt.Sprintf("Anthropic-Ratelimit-Unified-W%02d-Status", i), "allowed")
	}
	codex := collectQuotaSignals("codex", codexHeaders)
	if len(codex) != maxQuotaSignalHeaders {
		t.Fatalf("codex kept %d signals, want %d", len(codex), maxQuotaSignalHeaders)
	}
	for _, name := range []string{"Retry-After", "X-Codex-Plan-Type", "X-Codex-Primary-Used-Percent",
		"X-Codex-Code-Review-Primary-Used-Percent", "X-Codex-Zz-Primary-Used-Percent",
		"X-Codex-Additional-L00-Primary-Used-Percent", "X-Codex-Additional-L58-Primary-Used-Percent"} {
		if _, ok := codex[name]; !ok {
			t.Fatalf("codex dropped %s", name)
		}
	}
	for _, name := range []string{"X-Codex-Additional-L59-Primary-Used-Percent", "X-Ratelimit-Limit-Requests"} {
		if _, ok := codex[name]; ok {
			t.Fatalf("codex kept %s past the cap", name)
		}
	}
	// Claude ranks every header equally, so Retry-After sorts after the
	// Anthropic names and is the first dropped.
	claude := collectQuotaSignals("claude", claudeHeaders)
	if len(claude) != maxQuotaSignalHeaders {
		t.Fatalf("claude kept %d signals, want %d", len(claude), maxQuotaSignalHeaders)
	}
	for name, kept := range map[string]bool{"Anthropic-Ratelimit-Unified-W63-Status": true, "Anthropic-Ratelimit-Unified-W64-Status": false, "Retry-After": false} {
		if _, ok := claude[name]; ok != kept {
			t.Fatalf("claude kept %s = %v, want %v", name, ok, kept)
		}
	}
}

func TestProviderQuotaCharacterizationRejectedCooldownSignals(t *testing.T) {
	rejected := http.Header{
		"Anthropic-Ratelimit-Unified-Status": {"rejected"},
		"Retry-After":                        {"30"},
		"X-Codex-Primary-Used-Percent":       {"100"},
	}
	windowRejected := http.Header{"Anthropic-Ratelimit-Unified-7d-Status": {"Rejected"}}
	allowed := http.Header{"Anthropic-Ratelimit-Unified-Status": {"allowed"}, "Retry-After": {"30"}}
	for _, tc := range []struct {
		provider string
		headers  http.Header
		want     map[string]string
	}{
		{"claude", rejected, map[string]string{"Anthropic-Ratelimit-Unified-Status": "rejected", "Retry-After": "30"}},
		{" Claude ", windowRejected, map[string]string{"Anthropic-Ratelimit-Unified-7d-Status": "Rejected"}},
		{"claude", allowed, nil},
		{"codex", rejected, nil},
		{"devin", rejected, nil},
		{"gemini", windowRejected, nil},
	} {
		if got := rejectedClaudeSignalsForCooldown(tc.provider, tc.headers); !maps.Equal(got, tc.want) {
			t.Fatalf("rejectedClaudeSignalsForCooldown(%q) = %v, want %v", tc.provider, got, tc.want)
		}
	}
	// Persisted signals keep only the Anthropic unified headers, and only while rejected.
	persisted := map[string]string{
		"Anthropic-Ratelimit-Unified-Status":   "rejected",
		"anthropic-ratelimit-unified-5h-reset": "1700000000",
		"Retry-After":                          "30",
		"X-Codex-Primary-Used-Percent":         "100",
	}
	want := map[string]string{"Anthropic-Ratelimit-Unified-Status": "rejected", "Anthropic-Ratelimit-Unified-5h-Reset": "1700000000"}
	if got := sanitizePersistedClaudeCooldownSignals(persisted); !maps.Equal(got, want) {
		t.Fatalf("sanitizePersistedClaudeCooldownSignals = %v, want %v", got, want)
	}
	for _, raw := range []map[string]string{nil, {"Anthropic-Ratelimit-Unified-Status": "allowed"}, {"Retry-After": "30"}} {
		if got := sanitizePersistedClaudeCooldownSignals(raw); got != nil {
			t.Fatalf("sanitizePersistedClaudeCooldownSignals(%v) = %v, want nil", raw, got)
		}
	}
}

// Codex probes on every pass; every other configured provider gets the slow
// cadence with a faster one shortly before a known reset.
func TestProviderQuotaCharacterizationRecheckCadence(t *testing.T) {
	on := internalconfig.QuotaReconcileProvider{Mode: internalconfig.QuotaReconcileClear}
	cfg := &internalconfig.Config{QuotaReconcile: internalconfig.QuotaReconcileConfig{
		"codex": on, "claude": on, "devin": on, "gemini": on,
	}}
	fast := [3]time.Duration{quotaProbeMinGap, quotaProbeMinGap, 0}
	slow := [3]time.Duration{30 * time.Minute, 5 * time.Minute, time.Hour}
	for provider, want := range map[string][3]time.Duration{
		"codex": fast, " CODEX ": fast, "claude": slow, "devin": slow, "gemini": slow,
	} {
		p := quotaRecheckPolicyFor(cfg, provider)
		if got := [3]time.Duration{p.interval, p.nearInterval, p.nearWindow}; p.mode != internalconfig.QuotaReconcileClear || got != want {
			t.Fatalf("quotaRecheckPolicyFor(%q) = %+v, want cadence %v", provider, p, want)
		}
	}
	cfg.QuotaReconcile["codex"] = internalconfig.QuotaReconcileProvider{Mode: internalconfig.QuotaReconcileObserve, Interval: time.Hour, NearResetWindow: time.Hour}
	p := quotaRecheckPolicyFor(cfg, "codex")
	if p.interval != time.Hour || p.nearInterval != quotaProbeMinGap || p.nearWindow != time.Hour {
		t.Fatalf("codex overrides = %+v", p)
	}
}
