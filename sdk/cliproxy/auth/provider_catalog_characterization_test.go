package auth

import "testing"

// Characterization tests for the OAuth model alias channel of each provider
// (roadmap 3.2 slice 5). They pin today's mapping so moving the provider fact
// into internal/providerspec cannot change it.

func TestProviderCatalogCharacterization_OAuthModelAliasChannel(t *testing.T) {
	providers := []string{
		"claude", "codex", "vertex", "aistudio", "antigravity",
		"kimi", "kimi-ai", "kimi.ai", "kimi.com", "xai", "meta", "devin",
		"gemini-interactions", "gemini-cli", "openai-compatibility", "my-plugin",
	}
	for _, kind := range []string{"oauth", "", "OAuth"} {
		for _, p := range providers {
			if got := OAuthModelAliasChannel(p, kind); got != p {
				t.Errorf("OAuthModelAliasChannel(%q, %q) = %q, want %q", p, kind, got, p)
			}
		}
		// Gemini OAuth credentials have no alias channel.
		if got := OAuthModelAliasChannel("gemini", kind); got != "" {
			t.Errorf("OAuthModelAliasChannel(gemini, %q) = %q, want empty", kind, got)
		}
		// The provider name is trimmed and lower-cased first.
		if got := OAuthModelAliasChannel(" Gemini ", kind); got != "" {
			t.Errorf("OAuthModelAliasChannel(\" Gemini \", %q) = %q, want empty", kind, got)
		}
		if got := OAuthModelAliasChannel(" Claude ", kind); got != "claude" {
			t.Errorf("OAuthModelAliasChannel(\" Claude \", %q) = %q, want claude", kind, got)
		}
	}
	// API-key credentials never have an alias channel.
	for _, kind := range []string{"apikey", "api_key", "api-key", " API-Key "} {
		for _, p := range append(providers, "gemini") {
			if got := OAuthModelAliasChannel(p, kind); got != "" {
				t.Errorf("OAuthModelAliasChannel(%q, %q) = %q, want empty", p, kind, got)
			}
		}
	}
}
