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

// Support is a tri-state capability answer. The zero value is unknown.
type Support int8

const (
	SupportUnknown Support = iota
	Supported
	Unsupported
)

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
	// ModelCatalog names the built-in static model catalog the provider's
	// credentials read. Empty means none.
	ModelCatalog string
	// ModelCatalogAliases are further names that read ModelCatalog.
	ModelCatalogAliases []string
	// NoOAuthModelAlias marks providers whose OAuth credentials have no
	// OAuth model alias channel.
	NoOAuthModelAlias bool
	// ResponsesWebSearch says whether the provider's Responses path supports
	// native web search.
	ResponsesWebSearch Support
	// ModelType is the model `type` that marks models this provider serves.
	// Empty means none.
	ModelType string
	// PrefersWebsocket marks providers whose websocket-enabled credentials
	// are picked first when the downstream connection is a websocket.
	PrefersWebsocket bool
}

// openAICompatiblePrefix starts the provider name of each configured
// OpenAI-compatible upstream.
const openAICompatiblePrefix = "openai-compatible-"

// specs is keyed by lower-case provider and never written after init.
var specs = map[string]Spec{
	"claude": {OAuthFlow: "anthropic", OAuthAliases: []string{"claude"}, ModelCatalog: "claude", ResponsesWebSearch: Supported},
	"codex": {
		DefaultRefreshLead:     CodexOuterRefreshLead,
		InvalidGrantIsTerminal: true,
		LegacyQuotaReconcile:   true,
		OAuthFlow:              "codex",
		OAuthAliases:           []string{"openai"},
		ModelCatalog:           "codex",
		ResponsesWebSearch:     Supported,
		PrefersWebsocket:       true,
	},
	"gemini":              {ModelCatalog: "gemini", NoOAuthModelAlias: true, ResponsesWebSearch: Unsupported},
	"gemini-interactions": {ModelCatalog: "gemini", ResponsesWebSearch: Unsupported},
	"interactions":        {ResponsesWebSearch: Unsupported},
	"vertex":              {ModelCatalog: "vertex", ResponsesWebSearch: Unsupported},
	"aistudio":            {ModelCatalog: "aistudio", ResponsesWebSearch: Unsupported},
	"antigravity": {
		OAuthFlow:          "antigravity",
		OAuthAliases:       []string{"anti-gravity"},
		ModelCatalog:       "antigravity",
		ResponsesWebSearch: Supported,
	},
	"xai": {
		OAuthFlow:           "xai",
		OAuthAliases:        []string{"x-ai", "x.ai", "grok"},
		ModelCatalog:        "xai",
		ModelCatalogAliases: []string{"x-ai", "grok"},
		ResponsesWebSearch:  Supported,
		PrefersWebsocket:    true,
	},
	"devin": {OAuthFlow: "devin", OAuthAliases: []string{"cognition"}, ModelCatalog: "devin", ModelType: "devin"},
	"meta":  {OAuthFlow: "meta", OAuthAliases: []string{"muse"}, ModelCatalog: "meta", ModelCatalogAliases: []string{"muse"}},
	"kimi": {
		IdentitylessLogin:   true,
		ModelCatalog:        "kimi",
		ModelCatalogAliases: []string{"kimi.ai", "kimi.com"},
		ResponsesWebSearch:  Unsupported,
	},
	"kimi-ai": {IdentitylessLogin: true, ModelCatalog: "kimi", ResponsesWebSearch: Unsupported},
	// Kimi domain names as provider names: only the web-search answer is
	// theirs; login and catalog facts stay with "kimi" above.
	"kimi.ai":  {ResponsesWebSearch: Unsupported},
	"kimi.com": {ResponsesWebSearch: Unsupported},
	// "openai" here is the OpenAI-compatible provider name, not the Codex
	// OAuth alias; its zero facts other than web search are unchanged.
	"openai":               {ResponsesWebSearch: Unsupported},
	"openai-compatibility": {ResponsesWebSearch: Unsupported},
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

// modelCatalogs maps every provider name and catalog alias to its static catalog.
var modelCatalogs = func() map[string]string {
	out := map[string]string{}
	for provider, spec := range specs {
		if spec.ModelCatalog == "" {
			continue
		}
		out[provider] = spec.ModelCatalog
		for _, alias := range spec.ModelCatalogAliases {
			out[alias] = spec.ModelCatalog
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

// ModelCatalog returns the built-in static model catalog a provider name or
// catalog alias reads, or "" when there is none.
func ModelCatalog(name string) string {
	return modelCatalogs[key(name)]
}

// ResponsesWebSearch says whether a provider's Responses path supports native
// web search. Every configured OpenAI-compatible upstream is unsupported.
func ResponsesWebSearch(provider string) Support {
	name := key(provider)
	if strings.HasPrefix(name, openAICompatiblePrefix) {
		return Unsupported
	}
	return specs[name].ResponsesWebSearch
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
