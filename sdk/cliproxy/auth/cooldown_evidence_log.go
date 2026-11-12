package auth

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"
)

// cooldownDeadline returns the moment a 429 cooldown at the given scope lifts.
// Credential scope reads the auth-level window; model scope reads the model state.
func cooldownDeadline(auth *Auth, state *ModelState, credentialScope bool) time.Time {
	if credentialScope {
		if auth == nil {
			return time.Time{}
		}
		deadline := auth.NextRetryAfter
		if auth.Quota.Exceeded && auth.Quota.NextRecoverAt.After(deadline) {
			deadline = auth.Quota.NextRecoverAt
		}
		return deadline
	}
	if state == nil {
		return time.Time{}
	}
	return state.NextRetryAfter
}

// logUpstreamCooldownEvidence records which upstream rate-limit window caused a
// cooldown, because the response headers otherwise live only in process memory
// and the proxy's own error log carries just its aggregate 429. The headers are
// restricted to the quota-signal allowlist and the credential is identified by
// its hashed auth_index, never its ID, file name, e-mail or key.
func logUpstreamCooldownEvidence(ctx context.Context, auth *Auth, provider, model string, credentialScope bool, state *ModelState, deadline time.Time, retryAfter *time.Duration, headers http.Header) {
	if auth == nil || !ProviderSupportsQuotaObservation(provider) {
		return
	}
	scope := "model"
	reason := "transient_rate_limit"
	if credentialScope {
		scope = "credential"
		reason = auth.Quota.Reason
	} else if state != nil && state.Quota.Exceeded {
		reason = state.Quota.Reason
	}
	var message strings.Builder
	message.WriteString("upstream 429 cooldown applied: scope=" + scope + " until=" + deadline.UTC().Format(time.RFC3339))
	if retryAfter != nil {
		message.WriteString(" retry_after=" + retryAfter.String())
	}
	if signals := collectQuotaSignals(provider, headers); len(signals) > 0 {
		names := make([]string, 0, len(signals))
		for name := range signals {
			names = append(names, name)
		}
		sort.Strings(names)
		pairs := make([]string, 0, len(names))
		for _, name := range names {
			pairs = append(pairs, strings.ToLower(name)+"="+signals[name])
		}
		message.WriteString(" ratelimit_headers=[" + strings.Join(pairs, " ") + "]")
	}
	logEntryWithRequestID(ctx).WithFields(map[string]any{
		"provider":   strings.ToLower(strings.TrimSpace(provider)),
		"model":      model,
		"auth_index": auth.EnsureIndex(),
		"reason":     reason,
	}).Warn(message.String())
}
