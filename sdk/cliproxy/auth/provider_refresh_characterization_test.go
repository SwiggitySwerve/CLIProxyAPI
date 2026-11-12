package auth

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

// Characterization tests for the provider refresh rules (roadmap 3.2 slice 4).
// They pin today's per-provider behaviour so moving the rules into the
// provider registry cannot change them.

type refreshStatusErr struct {
	msg    string
	status int
}

func (e refreshStatusErr) Error() string   { return e.msg }
func (e refreshStatusErr) StatusCode() int { return e.status }

func TestProviderRefreshCharacterization_DefaultRefreshLead(t *testing.T) {
	for _, tc := range []struct {
		provider string
		want     time.Duration // 0 means nil
	}{
		{"codex", CodexOuterRefreshLead},
		{" Codex ", CodexOuterRefreshLead},
		{"claude", 0},
		{"gemini", 0},
		{"antigravity", 0},
		{"kimi", 0},
		{"xai", 0},
		{"devin", 0},
		{"", 0},
	} {
		setRefreshLeadFactory(t, tc.provider, nil)
		got := ProviderRefreshLead(tc.provider, nil)
		if tc.want == 0 {
			if got != nil {
				t.Errorf("ProviderRefreshLead(%q) = %v, want nil", tc.provider, *got)
			}
			continue
		}
		if got == nil || *got != tc.want {
			t.Errorf("ProviderRefreshLead(%q) = %v, want %v", tc.provider, got, tc.want)
		}
	}
	if CodexOuterRefreshLead != 96*time.Hour {
		t.Fatalf("CodexOuterRefreshLead = %v, want 96h", CodexOuterRefreshLead)
	}
}

func TestProviderRefreshCharacterization_FactoryOverridesDefault(t *testing.T) {
	// A registered factory wins over the built-in default, including a factory
	// that returns no lead.
	short := time.Hour
	setRefreshLeadFactory(t, "codex", func() *time.Duration { return &short })
	if got := ProviderRefreshLead("codex", nil); got == nil || *got != short {
		t.Fatalf("codex with factory = %v, want %v", got, short)
	}
	setRefreshLeadFactory(t, "codex", func() *time.Duration { return nil })
	if got := ProviderRefreshLead("codex", nil); got != nil {
		t.Fatalf("codex with nil factory = %v, want nil", *got)
	}
}

func TestProviderRefreshCharacterization_CodexOuterNeedsRefreshLead(t *testing.T) {
	// CodexOuterNeedsRefresh falls back to the provider default lead even when
	// a registered factory returns none; other providers have no lead.
	now := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	expiry := now.Add(200 * time.Hour)
	for _, tc := range []struct {
		name       string
		provider   string
		nilFactory bool
		wantDue    time.Time
		wantHas    bool
	}{
		{"codex default", "codex", false, expiry.Add(-CodexOuterRefreshLead), true},
		{"codex nil factory", "codex", true, expiry.Add(-CodexOuterRefreshLead), true},
		{"claude", "claude", false, time.Time{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.nilFactory {
				setRefreshLeadFactory(t, tc.provider, func() *time.Duration { return nil })
			} else {
				setRefreshLeadFactory(t, tc.provider, nil)
			}
			a := &Auth{
				Provider: tc.provider,
				Metadata: map[string]any{
					"access_token":  "fake-access-token",
					"refresh_token": "fake-refresh-token",
					"expired":       expiry.Format(time.RFC3339),
				},
			}
			due, has, needs := CodexOuterNeedsRefresh(a, now)
			if has != tc.wantHas || !due.Equal(tc.wantDue) || needs {
				t.Fatalf("CodexOuterNeedsRefresh = (%v, %v, %v), want (%v, %v, false)", due, has, needs, tc.wantDue, tc.wantHas)
			}
		})
	}
}

func TestProviderRefreshCharacterization_InvalidGrantTerminal(t *testing.T) {
	// A bare invalid_grant refresh failure is terminal only for Codex; other
	// terminal grant codes and 401s are terminal for every provider.
	invalidGrant := refreshStatusErr{msg: "oauth refresh failed: invalid_grant", status: http.StatusBadRequest}
	for _, tc := range []struct {
		provider string
		err      error
		want     bool
	}{
		{"codex", invalidGrant, true},
		{"CODEX", invalidGrant, true},
		{"codex", errors.New("invalid_grant"), true},
		{"codex", refreshStatusErr{msg: "invalid_grant", status: http.StatusInternalServerError}, false},
		{"claude", invalidGrant, false},
		{"gemini", invalidGrant, false},
		{"antigravity", invalidGrant, false},
		{"kimi", invalidGrant, false},
		{"claude", refreshStatusErr{msg: "refresh_token_reused", status: http.StatusBadRequest}, true},
		{"claude", refreshStatusErr{msg: "unauthorized", status: http.StatusUnauthorized}, true},
		{"codex", refreshStatusErr{msg: "upstream timeout", status: http.StatusBadGateway}, false},
	} {
		if got := isTerminalRefreshError(&Auth{Provider: tc.provider}, tc.err); got != tc.want {
			t.Errorf("isTerminalRefreshError(%q, %q) = %v, want %v", tc.provider, tc.err, got, tc.want)
		}
	}
	if isTerminalRefreshError(nil, invalidGrant) {
		t.Errorf("isTerminalRefreshError(nil auth, invalid_grant) = true, want false")
	}
}
