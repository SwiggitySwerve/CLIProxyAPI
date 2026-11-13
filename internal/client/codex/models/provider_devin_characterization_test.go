package models

import "testing"

// Characterization test for the provider check in isCodexClientDevinModel
// (roadmap 3.2 slice 5b): a model is a Devin model when a provider serving it
// is Devin. The model ID carries no other Devin marker and is not in any
// catalog, so only the provider decides.

func TestProviderDevinCharacterization_ProvidersForModel(t *testing.T) {
	const id = "slice5b-unlisted-model"
	for _, tc := range []struct {
		providers []string
		want      bool
	}{
		{[]string{"devin"}, true},
		{[]string{"Devin"}, true},
		{[]string{" DEVIN "}, true},
		{[]string{"codex", "devin"}, true},
		{[]string{"cognition"}, false},
		{[]string{"codex"}, false},
		{[]string{"claude"}, false},
		{[]string{"meta"}, false},
		{[]string{"unknown-provider"}, false},
		{[]string{""}, false},
		{nil, false},
	} {
		providersForModel := func(string) []string { return tc.providers }
		if got := isCodexClientDevinModel(id, nil, nil, providersForModel); got != tc.want {
			t.Errorf("isCodexClientDevinModel with providers %q = %v, want %v", tc.providers, got, tc.want)
		}
	}
}
