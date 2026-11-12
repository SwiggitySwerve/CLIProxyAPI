// Package providerspec is the exported part of the provider registry
// (roadmap 3.2): static per-provider facts that packages outside
// sdk/cliproxy/auth need. It imports nothing from this module, so every
// package, including internal/config, can read it without an import cycle.
// Behaviour that needs auth types (quota signals, recheck adapters) stays in
// sdk/cliproxy/auth/provider_registry.go.
package providerspec

import (
	"strings"
	"time"
)

// CodexOuterRefreshLead is Codex's default proactive refresh lead before the
// nominal 10-day OAuth JWT expiration.
const CodexOuterRefreshLead = 96 * time.Hour

// Spec is one provider's static facts. The zero Spec is an unknown provider.
type Spec struct {
	// DefaultRefreshLead is the proactive refresh lead used when no
	// authenticator registers one. Zero means none.
	DefaultRefreshLead time.Duration
	// InvalidGrantIsTerminal marks a bare invalid_grant refresh failure as
	// terminal for the provider's credentials.
	InvalidGrantIsTerminal bool
	// LegacyQuotaReconcile marks the provider the legacy `quota-reconcile: true`
	// config value enables in clear mode. At most one provider sets it.
	LegacyQuotaReconcile bool
	// OAuthFlow is the canonical name of the built-in OAuth login flow the
	// management API accepts for this provider. Empty means none.
	OAuthFlow string
	// OAuthAliases are further names that select OAuthFlow.
	OAuthAliases []string
	// IdentitylessLogin marks OAuth logins that return no account identity,
	// so a targeted re-login cannot be matched by email or account id.
	IdentitylessLogin bool
}

// specs is keyed by lower-case provider and never written after init.
var specs = map[string]Spec{
	"claude": {OAuthFlow: "anthropic", OAuthAliases: []string{"claude"}},
	"codex": {
		DefaultRefreshLead:     CodexOuterRefreshLead,
		InvalidGrantIsTerminal: true,
		LegacyQuotaReconcile:   true,
		OAuthFlow:              "codex",
		OAuthAliases:           []string{"openai"},
	},
	"antigravity": {OAuthFlow: "antigravity", OAuthAliases: []string{"anti-gravity"}},
	"xai":         {OAuthFlow: "xai", OAuthAliases: []string{"x-ai", "x.ai", "grok"}},
	"devin":       {OAuthFlow: "devin", OAuthAliases: []string{"cognition"}},
	"meta":        {OAuthFlow: "meta", OAuthAliases: []string{"muse"}},
	"kimi":        {IdentitylessLogin: true},
	"kimi-ai":     {IdentitylessLogin: true},
}

// oauthFlows maps every accepted OAuth flow name and alias to its canonical flow.
var oauthFlows = func() map[string]string {
	out := map[string]string{}
	for _, spec := range specs {
		if spec.OAuthFlow == "" {
			continue
		}
		out[spec.OAuthFlow] = spec.OAuthFlow
		for _, alias := range spec.OAuthAliases {
			out[alias] = spec.OAuthFlow
		}
	}
	return out
}()

func key(provider string) string {
	return strings.ToLower(strings.TrimSpace(provider))
}

// For returns the provider's facts, or the zero Spec for an unknown provider.
func For(provider string) Spec {
	return specs[key(provider)]
}

// OAuthFlow returns the canonical built-in OAuth flow for a provider name or
// alias, or false when there is none.
func OAuthFlow(name string) (string, bool) {
	flow, ok := oauthFlows[key(name)]
	return flow, ok
}

// LegacyQuotaReconcileProvider returns the provider the legacy
// `quota-reconcile: true` value enables.
func LegacyQuotaReconcileProvider() string {
	for provider, spec := range specs {
		if spec.LegacyQuotaReconcile {
			return provider
		}
	}
	return ""
}
