package registry

import "testing"

// Characterization test for whether each provider's Responses path supports
// native web search (roadmap 3.2 slice 5b). It pins today's answer so moving
// the provider facts into internal/providerspec cannot change it.

func TestProviderWebSearchCharacterization_PathSupport(t *testing.T) {
	const (
		yes     = "yes"
		no      = "no"
		unknown = "unknown"
	)
	for _, tc := range []struct {
		provider string
		want     string
	}{
		{"codex", yes},
		{" Codex ", yes},
		{"xai", yes},
		{"XAI", yes},
		{"claude", yes},
		{"antigravity", yes},
		{"openai", no},
		{"openai-compatibility", no},
		{"gemini", no},
		{"Gemini", no},
		{"aistudio", no},
		{"vertex", no},
		{"kimi", no},
		{"kimi-ai", no},
		{"kimi.ai", no},
		{"kimi.com", no},
		{"KIMI.COM", no},
		{"interactions", no},
		{"gemini-interactions", no},
		{"openai-compatible-", no},
		{"openai-compatible-acme", no},
		{" OpenAI-Compatible-Acme ", no},
		// Aliases of supported or unsupported providers that the switch does
		// not list stay unknown.
		{"x-ai", unknown},
		{"x.ai", unknown},
		{"grok", unknown},
		{"anthropic", unknown},
		{"anti-gravity", unknown},
		{"devin", unknown},
		{"cognition", unknown},
		{"meta", unknown},
		{"muse", unknown},
		{"openai-compatibility-acme", unknown},
		{"openai-compatible", unknown},
		{"unknown-provider", unknown},
		{"", unknown},
	} {
		got := unknown
		if support := responsesWebSearchProviderPathSupport(tc.provider); support != nil {
			got = no
			if *support {
				got = yes
			}
		}
		if got != tc.want {
			t.Errorf("responsesWebSearchProviderPathSupport(%q) = %s, want %s", tc.provider, got, tc.want)
		}
	}
}
