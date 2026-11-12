package auth

import (
	"maps"
	"net/http"
	"sort"
	"strings"
	"time"
)

const (
	maxQuotaSignalHeaders = 64
	maxQuotaSignalValue   = 512
)

// ProviderSupportsQuotaObservation reports whether the named provider emits a
// passive credential-level quota snapshot understood by collectQuotaSignals.
func ProviderSupportsQuotaObservation(provider string) bool {
	return providerQuotaFor(provider).observes
}

// ObserveResponseHeadersForProvider replaces the passive quota snapshot with the
// signals carried by the current upstream response.
//
// The snapshot is replaced rather than merged: a watermark such as Retry-After
// or a "limit reached" flag only appears on the response that produced it, so
// accumulating signals across responses would leave an expired value visible
// indefinitely. Responses that carry no quota signal at all (transport
// failures, 5xx, unrelated endpoints) leave the previous snapshot untouched.
//
// This function only ever touches ObservedAt and Signals. Cooldown and
// scheduling fields are never read or written here.
func (q *QuotaState) ObserveResponseHeadersForProvider(provider string, headers http.Header, observedAt time.Time) bool {
	if q == nil {
		return false
	}
	if !ProviderSupportsQuotaObservation(provider) {
		return q.ClearObservationSignals()
	}
	next := collectQuotaSignals(provider, headers)
	if len(next) == 0 {
		return false
	}
	if observedAt.IsZero() {
		observedAt = time.Now()
	}
	q.Signals = next
	q.ObservedAt = observedAt
	return true
}

// ClearObservationSignals removes only passive observation data. It leaves
// cooldown and scheduler state untouched.
func (q *QuotaState) ClearObservationSignals() bool {
	if q == nil || (len(q.Signals) == 0 && q.ObservedAt.IsZero()) {
		return false
	}
	q.Signals = nil
	q.ObservedAt = time.Time{}
	return true
}

// cooldownFieldsOf copies only scheduler cooldown fields. Observation data is
// omitted so cooldown persistence and restore cannot replace a live snapshot.
func cooldownFieldsOf(q QuotaState) QuotaState {
	return QuotaState{
		Exceeded:      q.Exceeded,
		Reason:        q.Reason,
		NextRecoverAt: q.NextRecoverAt,
		BackoffLevel:  q.BackoffLevel,
	}
}

// applyCooldownFields writes only scheduler cooldown fields. ObservedAt and
// Signals are left untouched so a cooldown transition cannot erase the last
// upstream watermark.
func applyCooldownFields(dst *QuotaState, cooldown QuotaState) {
	if dst == nil {
		return
	}
	dst.Exceeded = cooldown.Exceeded
	dst.Reason = cooldown.Reason
	dst.NextRecoverAt = cooldown.NextRecoverAt
	dst.BackoffLevel = cooldown.BackoffLevel
	dst.cooldownSignals = maps.Clone(cooldown.cooldownSignals)
	dst.cooldownEvaluated = cooldown.Exceeded || cooldown.cooldownEvaluated
}

// rejectedClaudeSignalsForCooldown returns the response's quota signals when
// the provider's registry entry says they pin a cooldown. Only Claude has such
// a rule today.
func rejectedClaudeSignalsForCooldown(provider string, headers http.Header) map[string]string {
	rejects := providerQuotaFor(provider).rejects
	if rejects == nil {
		return nil
	}
	signals := collectQuotaSignals(provider, headers)
	if !rejects(signals) {
		return nil
	}
	return signals
}

func sanitizePersistedClaudeCooldownSignals(raw map[string]string) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	headers := make(http.Header, len(raw))
	for key, val := range raw {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(key)), "anthropic-ratelimit-unified-") {
			headers.Set(key, val)
		}
	}
	return rejectedClaudeSignalsForCooldown("claude", headers)
}

// collectQuotaSignals builds the bounded snapshot for a single response.
func collectQuotaSignals(provider string, headers http.Header) map[string]string {
	signalRank := providerQuotaFor(provider).signalRank
	if len(headers) == 0 || signalRank == nil {
		return nil
	}
	names := make([]string, 0, len(headers))
	values := make(map[string]string, len(headers))
	ranks := make(map[string]int, len(headers))
	for key, headerValues := range headers {
		canonicalKey := http.CanonicalHeaderKey(strings.TrimSpace(key))
		rank, ok := signalRank(strings.ToLower(canonicalKey))
		if !ok || len(headerValues) == 0 {
			continue
		}
		ranks[canonicalKey] = rank
		value := strings.TrimSpace(headerValues[len(headerValues)-1])
		if !validQuotaSignalValue(value) {
			continue
		}
		if _, exists := values[canonicalKey]; !exists {
			names = append(names, canonicalKey)
		}
		values[canonicalKey] = value
	}
	if len(names) == 0 {
		return nil
	}
	// Rank then sort so truncation is deterministic and keeps credential-level
	// watermarks (plan, credits, primary) ahead of additional-limit namespaces.
	sort.Slice(names, func(i, j int) bool {
		ri, rj := ranks[names[i]], ranks[names[j]]
		if ri != rj {
			return ri < rj
		}
		return names[i] < names[j]
	})
	if len(names) > maxQuotaSignalHeaders {
		names = names[:maxQuotaSignalHeaders]
	}
	signals := make(map[string]string, len(names))
	for _, name := range names {
		signals[name] = values[name]
	}
	return signals
}

// validQuotaSignalValue rejects empty, oversized, and control-character values.
// Observed values reach the plain-text upstream request log, so a value
// containing CR/LF could otherwise forge a header line there.
func validQuotaSignalValue(value string) bool {
	if value == "" || len(value) > maxQuotaSignalValue {
		return false
	}
	for _, char := range value {
		if char < 0x20 || char == 0x7f {
			return false
		}
	}
	return true
}

// claudeSignalRank is Claude's quota-signal allowlist (registry signalRank).
// Every signal ranks equally, so past the cap they are kept by name.
func claudeSignalRank(name string) (int, bool) {
	return 0, name == "retry-after" || strings.HasPrefix(name, "anthropic-ratelimit-unified-")
}

// codexSignalRank is Codex's quota-signal allowlist (registry signalRank).
// Case order is load-bearing: the reject guards for non-x-codex names and for
// x-codex names without a limit marker sit between rank cases, so moving a case
// across them changes which headers are kept.
func codexSignalRank(name string) (int, bool) {
	switch {
	case name == "retry-after":
		return 0, true
	case strings.HasPrefix(name, "x-ratelimit-"):
		// Observed Codex responses do not carry x-ratelimit-* headers; the only
		// upstream seen emitting them is Grok, which is excluded from quota
		// observation. The rule is kept so a future Codex rollout is captured
		// without another change, but it is expected to be inert today.
		return 6, true
	case !strings.HasPrefix(name, "x-codex-"):
		return 0, false
	case name == "x-codex-plan-type", name == "x-codex-active-limit", strings.HasPrefix(name, "x-codex-credits-"):
		return 1, true
	case !hasCodexLimitMarker(name):
		return 0, false
	case name == "x-codex-allowed", name == "x-codex-limit-reached",
		strings.HasPrefix(name, "x-codex-primary-"), strings.HasPrefix(name, "x-codex-secondary-"):
		return 2, true
	case strings.HasPrefix(name, "x-codex-code-review-"):
		return 3, true
	case strings.HasPrefix(name, "x-codex-additional-"):
		return 5, true
	default:
		return 4, true
	}
}

func hasCodexLimitMarker(name string) bool {
	// Codex namespaces each additional limit by a short name on the HTTP path
	// (x-codex-bengalfox-primary-used-percent) and by limit name on the
	// websocket path (x-codex-additional-<limit>-primary-used-percent), so the
	// suffix is matched instead of an exhaustive header list.
	for _, marker := range []string{
		"-allowed",
		"-limit-reached",
		"-limit-name",
		"-used-percent",
		"-window-minutes",
		"-reset-after-seconds",
		"-reset-at",
		"-over-secondary-limit-percent",
	} {
		if strings.Contains(name, marker) {
			return true
		}
	}
	return false
}

// mergeQuotaObservation keeps the newest observation snapshot instead of
// unioning signals captured at different times, so merging an older snapshot
// can never resurrect a stale watermark.
func mergeQuotaObservation(target, source QuotaState) QuotaState {
	if source.ObservedAt.IsZero() || source.ObservedAt.Before(target.ObservedAt) {
		return target
	}
	target.ObservedAt = source.ObservedAt
	target.Signals = source.Clone().Signals
	return target
}
