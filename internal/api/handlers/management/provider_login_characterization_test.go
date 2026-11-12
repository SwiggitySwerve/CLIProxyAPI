package management

import (
	"errors"
	"testing"
)

// Characterization tests for the provider login rules (roadmap 3.2 slice 4).
// They pin today's per-provider behaviour so moving the rules into the
// provider registry cannot change them.

func TestProviderLoginCharacterization_NormalizeOAuthProvider(t *testing.T) {
	for in, want := range map[string]string{
		"anthropic":    "anthropic",
		"claude":       "anthropic",
		" Claude ":     "anthropic",
		"codex":        "codex",
		"openai":       "codex",
		"OpenAI":       "codex",
		"antigravity":  "antigravity",
		"anti-gravity": "antigravity",
		"xai":          "xai",
		"x-ai":         "xai",
		"x.ai":         "xai",
		"grok":         "xai",
		"devin":        "devin",
		"cognition":    "devin",
		"meta":         "meta",
		"muse":         "meta",
	} {
		got, err := NormalizeOAuthProvider(in)
		if err != nil || got != want {
			t.Errorf("NormalizeOAuthProvider(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "gemini", "gemini-cli", "kimi", "kimi-ai", "vertex", "qwen", "iflow", "anthropic-ai"} {
		got, err := NormalizeOAuthProvider(in)
		if !errors.Is(err, errUnsupportedOAuthFlow) || got != "" {
			t.Errorf("NormalizeOAuthProvider(%q) = %q, %v; want unsupported", in, got, err)
		}
	}
}

func TestProviderLoginCharacterization_IdentitylessOAuthProvider(t *testing.T) {
	for in, want := range map[string]bool{
		"kimi":        true,
		"kimi-ai":     true,
		" KIMI ":      true,
		"Kimi-AI":     true,
		"kimi.ai":     false,
		"kimi.com":    false,
		"codex":       false,
		"claude":      false,
		"antigravity": false,
		"xai":         false,
		"devin":       false,
		"meta":        false,
		"":            false,
	} {
		if got := isIdentitylessOAuthProvider(in); got != want {
			t.Errorf("isIdentitylessOAuthProvider(%q) = %v, want %v", in, got, want)
		}
	}
}
