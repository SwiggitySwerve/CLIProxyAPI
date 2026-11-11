package config

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestParseConfigBytesQuotaReconcileBool(t *testing.T) {
	for name, tc := range map[string]struct {
		yaml string
		want QuotaReconcileConfig
	}{
		"legacy flat true":  {"quota-reconcile: true\n", QuotaReconcileFromBool(true)},
		"legacy flat false": {"quota-reconcile: false\n", nil},
		"v8 nested true":    {"routing:\n  cooldown:\n    quota-reconcile: true\n", QuotaReconcileFromBool(true)},
		"absent":            {"port: 8317\n", nil},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := ParseConfigBytes([]byte(tc.yaml))
			if err != nil {
				t.Fatalf("ParseConfigBytes() error = %v", err)
			}
			if len(cfg.QuotaReconcile) != len(tc.want) || cfg.QuotaReconcile["codex"] != tc.want["codex"] {
				t.Fatalf("QuotaReconcile = %+v, want %+v", cfg.QuotaReconcile, tc.want)
			}
		})
	}
}

func TestParseConfigBytesQuotaReconcileProviderMap(t *testing.T) {
	const body = `
quota-reconcile:
  codex:
    mode: clear
  Claude:
    mode: observe
    interval: 30m
    near-reset-interval: 5m
    near-reset-window: 1h
`
	for name, doc := range map[string]string{
		"legacy flat": body,
		"v8 nested":   "routing:\n  cooldown:\n" + indent(body, "    "),
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := ParseConfigBytes([]byte(doc))
			if err != nil {
				t.Fatalf("ParseConfigBytes() error = %v", err)
			}
			if got, ok := cfg.QuotaReconcile.Provider("codex"); !ok || got != (QuotaReconcileProvider{Mode: QuotaReconcileClear}) {
				t.Fatalf("codex = %+v ok=%v", got, ok)
			}
			want := QuotaReconcileProvider{Mode: QuotaReconcileObserve, Interval: 30 * time.Minute, NearResetInterval: 5 * time.Minute, NearResetWindow: time.Hour}
			if got, ok := cfg.QuotaReconcile.Provider("claude"); !ok || got != want {
				t.Fatalf("claude = %+v ok=%v, want %+v", got, ok, want)
			}
			if _, ok := cfg.QuotaReconcile.Provider("gemini"); ok {
				t.Fatalf("unconfigured provider reported as configured")
			}
		})
	}
}

func TestParseConfigBytesQuotaReconcileRejectsInvalid(t *testing.T) {
	for name, doc := range map[string]string{
		"unknown mode":     "quota-reconcile:\n  claude:\n    mode: clean\n",
		"missing mode":     "quota-reconcile:\n  claude:\n    interval: 30m\n",
		"negative":         "quota-reconcile:\n  claude:\n    mode: clear\n    interval: -1m\n",
		"not bool or map":  "quota-reconcile: sometimes\n",
		"sequence of keys": "quota-reconcile:\n  - codex\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseConfigBytes([]byte(doc)); err == nil {
				t.Fatalf("ParseConfigBytes() accepted %q", doc)
			}
		})
	}
}

func TestQuotaReconcileConfigMarshalRoundTrip(t *testing.T) {
	for name, tc := range map[string]struct {
		in   QuotaReconcileConfig
		want string
	}{
		"legacy true stays a bool": {QuotaReconcileFromBool(true), "true"},
		"empty is false":           {nil, "false"},
		"map form stays a map":     {QuotaReconcileConfig{"claude": {Mode: QuotaReconcileObserve, Interval: 30 * time.Minute}}, "claude:"},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := yaml.Marshal(struct {
				Q QuotaReconcileConfig `yaml:"q"`
			}{tc.in})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(out), tc.want) {
				t.Fatalf("marshalled %q, want it to contain %q", out, tc.want)
			}
			var back struct {
				Q QuotaReconcileConfig `yaml:"q"`
			}
			if err := yaml.Unmarshal(out, &back); err != nil {
				t.Fatalf("unmarshal %q: %v", out, err)
			}
			if len(back.Q) != len(tc.in) {
				t.Fatalf("round trip = %+v, want %+v", back.Q, tc.in)
			}
			for k, v := range tc.in {
				if back.Q[k] != v {
					t.Fatalf("round trip %s = %+v, want %+v", k, back.Q[k], v)
				}
			}
		})
	}
}

func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimLeft(s, "\n"), "\n")
	for i, line := range lines {
		if line != "" {
			lines[i] = prefix + line
		}
	}
	return strings.Join(lines, "\n")
}
