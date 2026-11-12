package config

import (
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// QuotaReconcileMode says what the quota recheck loop does for one provider.
type QuotaReconcileMode string

const (
	// QuotaReconcileOff never rechecks the provider.
	QuotaReconcileOff QuotaReconcileMode = "off"
	// QuotaReconcileObserve rechecks and logs the verdict but never clears a cooldown.
	QuotaReconcileObserve QuotaReconcileMode = "observe"
	// QuotaReconcileClear rechecks and clears a cooldown the provider has lifted.
	QuotaReconcileClear QuotaReconcileMode = "clear"
)

// QuotaReconcileProvider configures the quota recheck loop for one provider.
// Zero durations select the provider default.
type QuotaReconcileProvider struct {
	Mode QuotaReconcileMode `yaml:"mode" json:"mode"`
	// Interval is the gap between rechecks of a cooled credential.
	Interval time.Duration `yaml:"interval,omitempty" json:"interval,omitempty"`
	// NearResetInterval replaces Interval within NearResetWindow before a known reset.
	NearResetInterval time.Duration `yaml:"near-reset-interval,omitempty" json:"near-reset-interval,omitempty"`
	// NearResetWindow is how long before a known reset the near interval applies.
	NearResetWindow time.Duration `yaml:"near-reset-window,omitempty" json:"near-reset-window,omitempty"`
}

// QuotaReconcileConfig maps a lowercase provider key to its recheck settings.
// In YAML it accepts the legacy bool (true means codex in clear mode, false
// means off) or a per-provider map.
type QuotaReconcileConfig map[string]QuotaReconcileProvider

// QuotaReconcileFromBool converts the legacy quota-reconcile bool.
func QuotaReconcileFromBool(enabled bool) QuotaReconcileConfig {
	if !enabled {
		return nil
	}
	return QuotaReconcileConfig{"codex": {Mode: QuotaReconcileClear}}
}

// Provider returns the settings for provider, or false when none are configured.
func (c QuotaReconcileConfig) Provider(provider string) (QuotaReconcileProvider, bool) {
	p, ok := c[strings.ToLower(strings.TrimSpace(provider))]
	return p, ok
}

// UnmarshalYAML accepts the legacy bool and the per-provider map.
func (c *QuotaReconcileConfig) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		var enabled bool
		if err := value.Decode(&enabled); err != nil {
			return fmt.Errorf("invalid quota-reconcile value: want a bool or a provider map")
		}
		*c = QuotaReconcileFromBool(enabled)
		return nil
	}
	var raw map[string]QuotaReconcileProvider
	if err := value.Decode(&raw); err != nil {
		return fmt.Errorf("invalid quota-reconcile value: %w", err)
	}
	out := make(QuotaReconcileConfig, len(raw))
	for key, p := range raw {
		provider := strings.ToLower(strings.TrimSpace(key))
		switch p.Mode {
		case QuotaReconcileOff, QuotaReconcileObserve, QuotaReconcileClear:
		default:
			return fmt.Errorf("invalid quota-reconcile mode %q for %q (allowed: off, observe, clear)", p.Mode, key)
		}
		if p.Interval < 0 || p.NearResetInterval < 0 || p.NearResetWindow < 0 {
			return fmt.Errorf("invalid quota-reconcile durations for %q: must not be negative", key)
		}
		out[provider] = p
	}
	*c = out
	return nil
}

// MarshalYAML writes the legacy bool when the value is expressible as one so a
// loaded legacy config saves back unchanged.
func (c QuotaReconcileConfig) MarshalYAML() (any, error) {
	if len(c) == 0 {
		return false, nil
	}
	if p, ok := c["codex"]; ok && len(c) == 1 && p == (QuotaReconcileProvider{Mode: QuotaReconcileClear}) {
		return true, nil
	}
	return map[string]QuotaReconcileProvider(c), nil
}
