package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLoadConfigOptional_RoutingUltrafast(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	configYAML := []byte(`
routing:
  ultrafast:
    account-ids: ["acct-ultrafast-0001"]
    auth-ids: ["codex-ultrafast.json"]
    emails: ["ultrafast-holder@example.test"]
`)
	if err := os.WriteFile(configPath, configYAML, 0o600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}
	cfg, err := LoadConfigOptional(configPath, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}
	want := UltrafastConfig{
		AccountIDs: []string{"acct-ultrafast-0001"},
		AuthIDs:    []string{"codex-ultrafast.json"},
		Emails:     []string{"ultrafast-holder@example.test"},
	}
	if !reflect.DeepEqual(cfg.Routing.Ultrafast, want) {
		t.Fatalf("Routing.Ultrafast = %#v, want %#v", cfg.Routing.Ultrafast, want)
	}
}

func TestLoadConfigOptional_RoutingUltrafastUnsetIsEmpty(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("routing:\n  strategy: fill-first\n"), 0o600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}
	cfg, err := LoadConfigOptional(configPath, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}
	if !reflect.DeepEqual(cfg.Routing.Ultrafast, UltrafastConfig{}) {
		t.Fatalf("unset routing.ultrafast must stay empty (deny all), got %#v", cfg.Routing.Ultrafast)
	}
}
