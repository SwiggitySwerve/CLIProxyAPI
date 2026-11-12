package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Characterization test for the legacy quota-reconcile bool (roadmap 3.2
// slice 2b). It names the provider literally so a change to the provider
// registry that moves legacy `true` to another provider is caught.
func TestQuotaReconcileLegacyBoolCharacterization(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte("quota-reconcile: true\n"))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}
	want := QuotaReconcileConfig{"codex": {Mode: QuotaReconcileClear}}
	if len(cfg.QuotaReconcile) != 1 || cfg.QuotaReconcile["codex"] != want["codex"] {
		t.Fatalf("legacy true = %+v, want %+v", cfg.QuotaReconcile, want)
	}
	for name, tc := range map[string]struct {
		in   QuotaReconcileConfig
		want string
	}{
		"codex clear is the legacy bool":    {want, "q: true\n"},
		"claude clear stays a map":          {QuotaReconcileConfig{"claude": {Mode: QuotaReconcileClear}}, "claude:"},
		"codex observe stays a map":         {QuotaReconcileConfig{"codex": {Mode: QuotaReconcileObserve}}, "codex:"},
		"codex clear plus another is a map": {QuotaReconcileConfig{"codex": {Mode: QuotaReconcileClear}, "claude": {Mode: QuotaReconcileClear}}, "codex:"},
	} {
		t.Run(name, func(t *testing.T) {
			out, errMarshal := yaml.Marshal(struct {
				Q QuotaReconcileConfig `yaml:"q"`
			}{tc.in})
			if errMarshal != nil {
				t.Fatal(errMarshal)
			}
			if !strings.Contains(string(out), tc.want) {
				t.Fatalf("marshalled %q, want it to contain %q", out, tc.want)
			}
		})
	}
}
