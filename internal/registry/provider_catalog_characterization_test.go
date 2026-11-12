package registry

import (
	"reflect"
	"testing"
)

// Characterization tests for the static catalog each provider name reads
// (roadmap 3.2 slice 5). They pin today's mapping so moving the provider
// facts into internal/providerspec cannot change it.

func catalogIDs(models []*ModelInfo) []string {
	if models == nil {
		return nil
	}
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	return ids
}

func TestProviderCatalogCharacterization_StaticModelDefinitionsByChannel(t *testing.T) {
	catalogs := map[string]func() []*ModelInfo{
		"claude":      GetClaudeModels,
		"gemini":      GetGeminiModels,
		"vertex":      GetGeminiVertexModels,
		"aistudio":    GetAIStudioModels,
		"codex":       GetCodexProModels,
		"kimi":        GetKimiModels,
		"antigravity": GetAntigravityModels,
		"xai":         GetXAIModels,
		"devin":       GetDevinModels,
		"meta":        GetMetaModels,
	}
	// Each catalog must be non-empty and distinct from the others, so a name
	// mapped to the wrong catalog fails below.
	seen := map[string]string{}
	for name, get := range catalogs {
		ids := catalogIDs(get())
		if len(ids) == 0 {
			t.Fatalf("catalog %s is empty; the test cannot tell catalogs apart", name)
		}
		key := ""
		for _, id := range ids {
			key += id + "\n"
		}
		if other, dup := seen[key]; dup {
			t.Fatalf("catalogs %s and %s are identical; the test cannot tell them apart", name, other)
		}
		seen[key] = name
	}

	for _, tc := range []struct {
		channel string
		want    string // catalog name; "" means nil
	}{
		{"claude", "claude"},
		{" Claude ", "claude"},
		{"gemini", "gemini"},
		{"gemini-interactions", "gemini"},
		{"vertex", "vertex"},
		{"aistudio", "aistudio"},
		{"codex", "codex"},
		{"kimi", "kimi"},
		{"kimi-ai", "kimi"},
		{"kimi.ai", "kimi"},
		{"KIMI.COM", "kimi"},
		{"antigravity", "antigravity"},
		{"xai", "xai"},
		{"x-ai", "xai"},
		{"grok", "xai"},
		{"devin", "devin"},
		{"meta", "meta"},
		{"muse", "meta"},
		// Names that read no static catalog.
		{"x.ai", ""},
		{"anthropic", ""},
		{"openai", ""},
		{"openai-compatibility", ""},
		{"interactions", ""},
		{"gemini-cli", ""},
		{"cognition", ""},
		{"anti-gravity", ""},
		{"", ""},
	} {
		got := catalogIDs(GetStaticModelDefinitionsByChannel(tc.channel))
		var want []string
		if tc.want != "" {
			want = catalogIDs(catalogs[tc.want]())
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("GetStaticModelDefinitionsByChannel(%q) = %d models, want catalog %q (%d models)", tc.channel, len(got), tc.want, len(want))
		}
	}
}
