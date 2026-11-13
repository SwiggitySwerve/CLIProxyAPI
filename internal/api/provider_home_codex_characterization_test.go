package api

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// Characterization tests for the provider facts in the Home Codex model list
// (roadmap 3.2 slice 5b). They pin today's decisions so moving provider facts
// into internal/providerspec cannot change them.

// TestProviderHomeCodexCharacterization_DevinType pins which Home providers tag
// a model `type: devin`. Provider names reach formatHomeCodexModel trimmed and
// lower-cased by decodeHomeModels, so they are listed that way here.
func TestProviderHomeCodexCharacterization_DevinType(t *testing.T) {
	for _, tc := range []struct {
		providers []string
		want      string // "" means no type
	}{
		{[]string{"devin"}, "devin"},
		{[]string{"codex", "devin"}, "devin"},
		{[]string{"cognition"}, ""},
		{[]string{"codex"}, ""},
		{[]string{"claude"}, ""},
		{[]string{"meta"}, ""},
		{[]string{"unknown-provider"}, ""},
		{nil, ""},
	} {
		model := formatHomeCodexModel(homeModelEntry{id: "m", providers: tc.providers})
		got, _ := model["type"].(string)
		if got != tc.want {
			t.Errorf("formatHomeCodexModel providers %q type = %q, want %q", tc.providers, got, tc.want)
		}
	}
}

// TestProviderHomeCodexCharacterization_DevinTypeFromPayload pins the same
// decision through the Home payload decoder, including mixed-case section names.
func TestProviderHomeCodexCharacterization_DevinTypeFromPayload(t *testing.T) {
	entries, err := decodeHomeModels([]byte(`{" Devin ":[{"id":"a"}],"CODEX":[{"id":"b"}],"Cognition":[{"id":"c"}]}`))
	if err != nil {
		t.Fatalf("decodeHomeModels: %v", err)
	}
	want := map[string]string{"a": "devin", "b": "", "c": ""}
	for _, entry := range entries {
		got, _ := formatHomeCodexModel(entry)["type"].(string)
		if got != want[entry.id] {
			t.Errorf("model %s type = %q, want %q", entry.id, got, want[entry.id])
		}
	}
}

// TestProviderHomeCodexCharacterization_CodexSettingsFirst pins that Codex's
// oauth-settings win over other providers' for a model several serve, and
// that the rest are tried in name order.
func TestProviderHomeCodexCharacterization_CodexSettingsFirst(t *testing.T) {
	setting := func(n int) []config.OAuthModelSetting {
		return []config.OAuthModelSetting{{Name: "m", MaxContextLength: n}}
	}
	cfg := &config.Config{OAuthSettings: map[string][]config.OAuthModelSetting{
		"claude": setting(1),
		"codex":  setting(2),
		"devin":  setting(3),
	}}
	for _, tc := range []struct {
		providers []string
		want      int
	}{
		{[]string{"claude", "codex", "devin"}, 2},
		{[]string{"devin", "codex"}, 2},
		{[]string{"devin", "claude"}, 1},
		{[]string{"devin"}, 3},
		{[]string{"unknown-provider"}, 0},
	} {
		model := formatHomeCodexModelWithSettings(homeModelEntry{id: "m", providers: tc.providers}, cfg)
		got, _ := model["max_context_length"].(int)
		if got != tc.want {
			t.Errorf("providers %q max_context_length = %d, want %d", tc.providers, got, tc.want)
		}
	}
}
