package management

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/kimi"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/htmlsanitize"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/redisqueue"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

const (
	providerCatalogSchemaVersion = 1
	acceptedProvidersEnvVar      = "CLIPROXY_ACCEPTED_PROVIDERS"
)

const (
	lifecycleAwaitingAuth   = "awaiting_auth"
	lifecycleActive         = "active"
	lifecycleDisabled       = "disabled"
	lifecycleNeedsAttention = "needs_attention"
)

type providerCatalogResponse struct {
	SchemaVersion     int                    `json:"schema_version"`
	InstanceID        *string                `json:"instance_id"`
	AcceptedProviders []string               `json:"accepted_providers"`
	Providers         []providerCatalogEntry `json:"providers"`
}

type providerCatalogEntry struct {
	ID          string                     `json:"id"`
	DisplayName string                     `json:"display_name"`
	Source      string                     `json:"source"`
	PluginID    *string                    `json:"plugin_id"`
	Logo        *string                    `json:"logo"`
	Lifecycle   string                     `json:"lifecycle"`
	Credentials providerCatalogCredentials `json:"credentials"`
	OAuth       *providerCatalogOAuth      `json:"oauth"`
	Quota       providerCatalogQuota       `json:"quota"`
}

type providerCatalogCredentials struct {
	Total    int `json:"total"`
	Active   int `json:"active"`
	Disabled int `json:"disabled"`
	Problem  int `json:"problem"`
}

type providerCatalogOAuth struct {
	Flow           string   `json:"flow"`
	Aliases        []string `json:"aliases"`
	ManualCallback bool     `json:"manual_callback"`
	WebUI          bool     `json:"webui"`
	Domains        []string `json:"domains"`
}

type providerCatalogQuota struct {
	Supported     bool    `json:"supported"`
	Reader        string  `json:"reader"`
	PluginID      *string `json:"plugin_id"`
	SupportsReset bool    `json:"supports_reset"`
}

type builtinOAuthProviderSpec struct {
	ID             string
	DisplayName    string
	Flow           string
	Aliases        []string
	ManualCallback bool
	WebUI          bool
	Domains        []string
	QuotaSupported bool
	SupportsReset  bool
	Start          func(*Handler, *gin.Context)
}

// builtinOAuthProviders is the single source of truth shared by StartOAuthV8 and
// GET /v8/management/providers.
var builtinOAuthProviders = []builtinOAuthProviderSpec{
	{
		ID:             "claude",
		DisplayName:    "Claude",
		Flow:           "browser",
		Aliases:        []string{"anthropic"},
		ManualCallback: true,
		WebUI:          true,
		Domains:        []string{},
		QuotaSupported: true,
		SupportsReset:  true,
		Start:          (*Handler).RequestAnthropicToken,
	},
	{
		ID:             "codex",
		DisplayName:    "Codex",
		Flow:           "browser",
		Aliases:        []string{},
		ManualCallback: true,
		WebUI:          true,
		Domains:        []string{},
		QuotaSupported: true,
		SupportsReset:  true,
		Start:          (*Handler).RequestCodexToken,
	},
	{
		ID:             "antigravity",
		DisplayName:    "Gemini / Antigravity",
		Flow:           "browser",
		Aliases:        []string{},
		ManualCallback: true,
		WebUI:          true,
		Domains:        []string{},
		QuotaSupported: true,
		SupportsReset:  false,
		Start:          (*Handler).RequestAntigravityToken,
	},
	{
		ID:             "kimi",
		DisplayName:    "Kimi (kimi.com)",
		Flow:           "device",
		Aliases:        []string{},
		ManualCallback: false,
		WebUI:          false,
		Domains:        []string{"kimi.com"},
		QuotaSupported: false,
		SupportsReset:  false,
		Start:          (*Handler).RequestKimiToken,
	},
	{
		ID:             "kimi-ai",
		DisplayName:    "Kimi (kimi.ai)",
		Flow:           "device",
		Aliases:        []string{},
		ManualCallback: false,
		WebUI:          false,
		Domains:        []string{"kimi.ai"},
		QuotaSupported: false,
		SupportsReset:  false,
		Start:          (*Handler).RequestKimiAIToken,
	},
	{
		ID:             "xai",
		DisplayName:    "xAI",
		Flow:           "device",
		Aliases:        []string{},
		ManualCallback: false,
		WebUI:          false,
		Domains:        []string{},
		QuotaSupported: false,
		SupportsReset:  false,
		Start:          (*Handler).RequestXAIToken,
	},
	{
		ID:             "devin",
		DisplayName:    "Devin",
		Flow:           "browser",
		Aliases:        []string{},
		ManualCallback: true,
		WebUI:          true,
		Domains:        []string{},
		QuotaSupported: false,
		SupportsReset:  false,
		Start:          (*Handler).RequestDevinToken,
	},
	{
		ID:             "meta",
		DisplayName:    "Meta",
		Flow:           "device",
		Aliases:        []string{},
		ManualCallback: false,
		WebUI:          false,
		Domains:        []string{},
		QuotaSupported: false,
		SupportsReset:  false,
		Start:          (*Handler).RequestMetaToken,
	},
}

func findBuiltinOAuthProvider(provider string) (builtinOAuthProviderSpec, bool) {
	key := strings.ToLower(strings.TrimSpace(provider))
	if key == "" {
		return builtinOAuthProviderSpec{}, false
	}
	for _, spec := range builtinOAuthProviders {
		if spec.ID == key {
			return spec, true
		}
	}
	return builtinOAuthProviderSpec{}, false
}

func (h *Handler) dispatchBuiltinOAuthV8(c *gin.Context, provider string) bool {
	spec, ok := findBuiltinOAuthProvider(provider)
	if !ok || spec.Start == nil {
		return false
	}
	spec.Start(h, c)
	return true
}

func canonicalBuiltinProviderKey(provider string) string {
	key := strings.ToLower(strings.TrimSpace(provider))
	if key == "" {
		return ""
	}
	for _, spec := range builtinOAuthProviders {
		if spec.ID == key {
			return spec.ID
		}
		for _, alias := range spec.Aliases {
			if alias == key {
				return spec.ID
			}
		}
	}
	return key
}

type providerNotAcceptedError struct {
	Provider          string
	AcceptedProviders []string
}

func (e *providerNotAcceptedError) Error() string {
	return fmt.Sprintf("provider %q is not accepted by this hub", e.Provider)
}

func (e *providerNotAcceptedError) responseBody() gin.H {
	accepted := make([]string, len(e.AcceptedProviders))
	copy(accepted, e.AcceptedProviders)
	return gin.H{
		"error":              "provider_not_accepted",
		"provider":           e.Provider,
		"accepted_providers": accepted,
	}
}

func normalizeProviderList(items []string) []string {
	if len(items) == 0 {
		return nil
	}
	out := make([]string, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for _, raw := range items {
		id := strings.ToLower(strings.TrimSpace(raw))
		if id == "" {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (h *Handler) effectiveAcceptedProviders() []string {
	if rawEnv, ok := os.LookupEnv(acceptedProvidersEnvVar); ok {
		trimmed := strings.TrimSpace(rawEnv)
		if trimmed != "" {
			parts := strings.Split(trimmed, ",")
			if normalized := normalizeProviderList(parts); len(normalized) > 0 {
				return normalized
			}
		}
	}
	if h == nil {
		return nil
	}
	cfg := h.configSnapshot()
	if cfg == nil {
		return nil
	}
	return normalizeProviderList(cfg.AcceptedProviders)
}

func (h *Handler) providerMatchesAccepted(provider string, accepted []string, host *pluginhost.Host) bool {
	if len(accepted) == 0 {
		return true
	}
	trimmed := strings.ToLower(strings.TrimSpace(provider))
	if trimmed == "" {
		return false
	}
	canonical := canonicalBuiltinProviderKey(trimmed)
	for _, allowed := range accepted {
		if allowed == trimmed || canonicalBuiltinProviderKey(allowed) == canonical {
			return true
		}
		if host != nil && host.SameAuthProvider(trimmed, allowed) {
			return true
		}
	}
	return false
}

func (h *Handler) checkAcceptedProvider(provider string) error {
	accepted := h.effectiveAcceptedProviders()
	if len(accepted) == 0 {
		return nil
	}
	var host *pluginhost.Host
	if h != nil {
		h.mu.Lock()
		host = h.pluginHost
		h.mu.Unlock()
	}
	trimmed := strings.ToLower(strings.TrimSpace(provider))
	if h.providerMatchesAccepted(trimmed, accepted, host) {
		return nil
	}
	return &providerNotAcceptedError{
		Provider:          trimmed,
		AcceptedProviders: accepted,
	}
}

func (h *Handler) rejectIfProviderNotAccepted(c *gin.Context, provider string) bool {
	errAccepted := h.checkAcceptedProvider(provider)
	if errAccepted == nil {
		return false
	}
	if notAccepted, ok := errAccepted.(*providerNotAcceptedError); ok {
		c.JSON(http.StatusConflict, notAccepted.responseBody())
		return true
	}
	c.JSON(http.StatusConflict, gin.H{"error": "provider_not_accepted", "provider": strings.ToLower(strings.TrimSpace(provider))})
	return true
}

func credentialLifecycle(auth *coreauth.Auth) string {
	return credentialLifecycleAt(auth, time.Now().UTC())
}

func credentialLifecycleAt(auth *coreauth.Auth, now time.Time) string {
	if auth == nil {
		return lifecycleAwaitingAuth
	}
	if auth.Disabled || auth.Status == coreauth.StatusDisabled {
		return lifecycleDisabled
	}
	unavailable, status, _, _ := reconcileAuthFileCooldownState(auth, now)
	if auth.RequiresSignIn || status == coreauth.StatusError || unavailable {
		return lifecycleNeedsAttention
	}
	return lifecycleActive
}

func deriveProviderLifecycle(creds providerCatalogCredentials) string {
	if creds.Total == 0 {
		return lifecycleAwaitingAuth
	}
	if creds.Active > 0 {
		return lifecycleActive
	}
	if creds.Problem > 0 {
		return lifecycleNeedsAttention
	}
	if creds.Disabled > 0 {
		return lifecycleDisabled
	}
	return lifecycleAwaitingAuth
}

func (h *Handler) credentialCatalogProviderID(auth *coreauth.Auth, host *pluginhost.Host) string {
	if auth == nil {
		return ""
	}
	if host != nil {
		for _, key := range []string{"oauth_provider", "portal"} {
			if auth.Attributes != nil {
				if candidate := strings.ToLower(strings.TrimSpace(auth.Attributes[key])); candidate != "" && host.HasAuthProvider(candidate) {
					return candidate
				}
			}
			if auth.Metadata != nil {
				if raw, ok := auth.Metadata[key].(string); ok {
					if candidate := strings.ToLower(strings.TrimSpace(raw)); candidate != "" && host.HasAuthProvider(candidate) {
						return candidate
					}
				}
			}
		}
	}
	raw := strings.ToLower(strings.TrimSpace(auth.Provider))
	switch raw {
	case "kimi", "kimi-ai", "kimi.ai", "kimi.com":
		if kimi.IsKimiAIAuth(auth) {
			return "kimi-ai"
		}
		return "kimi"
	default:
		return canonicalBuiltinProviderKey(raw)
	}
}

func (h *Handler) collectProviderCredentialCounts(now time.Time, host *pluginhost.Host) map[string]providerCatalogCredentials {
	counts := make(map[string]providerCatalogCredentials)
	if h == nil || h.authManager == nil {
		return counts
	}
	for _, auth := range h.authManager.List() {
		if auth == nil {
			continue
		}
		runtimeOnly := isRuntimeOnlyAuth(auth)
		if runtimeOnly && (auth.Disabled || auth.Status == coreauth.StatusDisabled) {
			continue
		}
		path := strings.TrimSpace(authAttribute(auth, "path"))
		configKey := coreauth.IsConfigAPIKeyAuth(auth)
		if path == "" && !runtimeOnly && !configKey {
			continue
		}
		if path != "" {
			if _, errStat := os.Stat(path); os.IsNotExist(errStat) {
				if !runtimeOnly && (auth.Disabled || auth.Status == coreauth.StatusDisabled || strings.EqualFold(strings.TrimSpace(auth.StatusMessage), "removed via management api")) {
					continue
				}
			}
		}
		providerID := h.credentialCatalogProviderID(auth, host)
		if providerID == "" {
			continue
		}
		entry := counts[providerID]
		entry.Total++
		switch credentialLifecycleAt(auth, now) {
		case lifecycleDisabled:
			entry.Disabled++
		case lifecycleNeedsAttention:
			entry.Problem++
		default:
			entry.Active++
		}
		counts[providerID] = entry
	}
	return counts
}

func nonEmptyStringPtr(raw string) *string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

func cloneNonNilStrings(in []string) []string {
	if len(in) == 0 {
		return []string{}
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func ifNoneMatchMatches(headerVal, etag string) bool {
	if strings.TrimSpace(headerVal) == "" || etag == "" {
		return false
	}
	cleanTarget := strings.TrimPrefix(strings.TrimSpace(etag), "W/")
	for _, part := range strings.Split(headerVal, ",") {
		candidate := strings.TrimSpace(part)
		if candidate == "*" {
			return true
		}
		if strings.TrimPrefix(candidate, "W/") == cleanTarget {
			return true
		}
	}
	return false
}

// GetProviderCatalogV8 returns the authoritative catalog of providers this ProxD hub
// exposes (built-in and plugin-backed), including OAuth, quota, and lifecycle metadata.
func (h *Handler) GetProviderCatalogV8(c *gin.Context) {
	now := time.Now().UTC()
	var host *pluginhost.Host
	if h != nil {
		h.mu.Lock()
		host = h.pluginHost
		h.mu.Unlock()
	}

	accepted := h.effectiveAcceptedProviders()
	counts := h.collectProviderCredentialCounts(now, host)

	quotaPluginIDs := map[string]string{}
	quotaSupportsResetByPlugin := map[string]bool{}
	var registeredQuotaProviders []pluginhost.RegisteredQuotaProviderInfo
	if host != nil {
		ctx := c.Request.Context()
		quotaPluginIDs = host.QuotaProviderPluginIDs(ctx)
		registeredQuotaProviders = host.QuotaProviders(ctx)
		for _, qp := range registeredQuotaProviders {
			if qp.PluginID != "" {
				quotaSupportsResetByPlugin[qp.PluginID] = qp.SupportsReset
			}
		}
	}

	resolveQuota := func(providerID string, builtinSupported, builtinReset bool) providerCatalogQuota {
		if pluginID := strings.TrimSpace(quotaPluginIDs[providerID]); pluginID != "" {
			pid := htmlsanitize.String(pluginID)
			return providerCatalogQuota{
				Supported:     true,
				Reader:        "plugin",
				PluginID:      &pid,
				SupportsReset: quotaSupportsResetByPlugin[pluginID],
			}
		}
		if builtinSupported {
			return providerCatalogQuota{
				Supported:     true,
				Reader:        "builtin",
				PluginID:      nil,
				SupportsReset: builtinReset,
			}
		}
		return providerCatalogQuota{
			Supported:     false,
			Reader:        "none",
			PluginID:      nil,
			SupportsReset: false,
		}
	}

	entriesByID := make(map[string]providerCatalogEntry)

	// 1. Built-in OAuth providers from the single source-of-truth table.
	for _, spec := range builtinOAuthProviders {
		creds := counts[spec.ID]
		entriesByID[spec.ID] = providerCatalogEntry{
			ID:          spec.ID,
			DisplayName: spec.DisplayName,
			Source:      "builtin",
			PluginID:    nil,
			Logo:        nil,
			Lifecycle:   deriveProviderLifecycle(creds),
			Credentials: creds,
			OAuth: &providerCatalogOAuth{
				Flow:           spec.Flow,
				Aliases:        cloneNonNilStrings(spec.Aliases),
				ManualCallback: spec.ManualCallback,
				WebUI:          spec.WebUI,
				Domains:        cloneNonNilStrings(spec.Domains),
			},
			Quota: resolveQuota(spec.ID, spec.QuotaSupported, spec.SupportsReset),
		}
	}

	// 2. Active plugin OAuth providers.
	if host != nil {
		for _, info := range host.RegisteredPlugins() {
			pluginID := htmlsanitize.String(strings.TrimSpace(info.ID))
			if pluginID == "" {
				continue
			}
			fallbackName := htmlsanitize.String(strings.TrimSpace(info.Metadata.Name))
			fallbackLogo := htmlsanitize.String(strings.TrimSpace(info.Metadata.Logo))

			addPluginEntry := func(rawID, rawName, rawLogo string, aliases []string) {
				id := strings.ToLower(strings.TrimSpace(rawID))
				if id == "" {
					return
				}
				displayName := htmlsanitize.String(strings.TrimSpace(rawName))
				if displayName == "" {
					displayName = fallbackName
				}
				if displayName == "" {
					displayName = id
				}
				logoStr := htmlsanitize.String(strings.TrimSpace(rawLogo))
				if logoStr == "" {
					logoStr = fallbackLogo
				}
				pid := pluginID
				creds := counts[id]
				entriesByID[id] = providerCatalogEntry{
					ID:          id,
					DisplayName: displayName,
					Source:      "plugin",
					PluginID:    &pid,
					Logo:        nonEmptyStringPtr(logoStr),
					Lifecycle:   deriveProviderLifecycle(creds),
					Credentials: creds,
					OAuth: &providerCatalogOAuth{
						Flow:           "browser",
						Aliases:        cloneNonNilStrings(aliases),
						ManualCallback: true,
						WebUI:          true,
						Domains:        []string{},
					},
					Quota: resolveQuota(id, false, false),
				}
			}

			if len(info.OAuthProviderEntries) > 0 {
				for _, oauthEntry := range info.OAuthProviderEntries {
					addPluginEntry(oauthEntry.Identifier, oauthEntry.Name, oauthEntry.Logo, nil)
				}
			} else if len(info.OAuthProviders) > 0 {
				for _, ident := range info.OAuthProviders {
					addPluginEntry(ident, "", "", nil)
				}
			} else if strings.TrimSpace(info.OAuthProvider) != "" {
				addPluginEntry(info.OAuthProvider, "", "", nil)
			}
		}

		// 3. Quota-only plugin providers (no OAuth capability registered).
		for _, qp := range registeredQuotaProviders {
			candidates := qp.SupportedProviders
			if len(candidates) == 0 && strings.TrimSpace(qp.Provider) != "" {
				candidates = []string{qp.Provider}
			}
			for _, rawID := range candidates {
				id := strings.ToLower(strings.TrimSpace(rawID))
				if id == "" {
					continue
				}
				if _, exists := entriesByID[id]; exists {
					continue
				}
				displayName := htmlsanitize.String(strings.TrimSpace(qp.DisplayName))
				if displayName == "" {
					displayName = id
				}
				pid := htmlsanitize.String(strings.TrimSpace(qp.PluginID))
				var pidPtr *string
				if pid != "" {
					pidPtr = &pid
				}
				creds := counts[id]
				entriesByID[id] = providerCatalogEntry{
					ID:          id,
					DisplayName: displayName,
					Source:      "plugin",
					PluginID:    pidPtr,
					Logo:        nil,
					Lifecycle:   deriveProviderLifecycle(creds),
					Credentials: creds,
					OAuth:       nil,
					Quota:       resolveQuota(id, false, false),
				}
			}
		}
	}

	// 4. Any credential-bearing provider not already in the catalog (e.g. uploaded/imported credentials).
	for providerID, creds := range counts {
		if _, exists := entriesByID[providerID]; exists {
			continue
		}
		entriesByID[providerID] = providerCatalogEntry{
			ID:          providerID,
			DisplayName: providerID,
			Source:      "builtin",
			PluginID:    nil,
			Logo:        nil,
			Lifecycle:   deriveProviderLifecycle(creds),
			Credentials: creds,
			OAuth:       nil,
			Quota:       resolveQuota(providerID, false, false),
		}
	}

	// Filter to accepted providers when the hub explicitly declares accepted_providers,
	// while still retaining any provider that currently holds credentials on this hub.
	ids := make([]string, 0, len(entriesByID))
	for id, entry := range entriesByID {
		if len(accepted) > 0 && entry.Credentials.Total == 0 && !h.providerMatchesAccepted(id, accepted, host) {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)

	providers := make([]providerCatalogEntry, 0, len(ids))
	for _, id := range ids {
		providers = append(providers, entriesByID[id])
	}

	var instanceIDPtr *string
	if instanceID := redisqueue.DeploymentInstanceID(); instanceID != "" {
		instanceIDPtr = &instanceID
	}

	resp := providerCatalogResponse{
		SchemaVersion:     providerCatalogSchemaVersion,
		InstanceID:        instanceIDPtr,
		AcceptedProviders: accepted,
		Providers:         providers,
	}

	payload, errMarshal := json.Marshal(resp)
	if errMarshal != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encode provider catalog"})
		return
	}

	sum := sha256.Sum256(payload)
	etag := `"` + hex.EncodeToString(sum[:]) + `"`
	c.Header("ETag", etag)
	c.Header("Cache-Control", "no-cache")
	if ifNoneMatchMatches(c.GetHeader("If-None-Match"), etag) {
		c.Status(http.StatusNotModified)
		c.Writer.WriteHeaderNow()
		return
	}

	c.Data(http.StatusOK, "application/json; charset=utf-8", payload)
}
