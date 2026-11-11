package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

const (
	claudeUsageSnapshotURL      = "https://api.anthropic.com/api/oauth/usage"
	claudeUsageSnapshotBeta     = "oauth-2025-04-20"
	claudeUsageSnapshotInterval = 10 * time.Minute
	// claudeUsageResetTolerance absorbs sub-minute jitter in reported reset times.
	claudeUsageResetTolerance = time.Minute
)

// claudeQuotaRecheckAdapter rechecks Claude credentials held by a unified
// rate-limit rejection (5h, 7d or a model-specific weekly window).
//
// The verdict comes only from the rate-limit headers of a minimal real
// inference request. The oauth/usage endpoint is advisory: it has reported 71%
// for a window that inference headers reported as rejected at 100%, so it only
// triggers an earlier verdict and never clears a cooldown.
type claudeQuotaRecheckAdapter struct{}

func (claudeQuotaRecheckAdapter) Provider() string { return "claude" }

func (claudeQuotaRecheckAdapter) snapshotInterval() time.Duration { return claudeUsageSnapshotInterval }

func (claudeQuotaRecheckAdapter) snapshotChanged(prev, next map[string]string) bool {
	return claudeUsageSnapshotChanged(prev, next)
}

// UsageSnapshot reads the oauth/usage endpoint through the credential's
// executor, which injects the token. Any non-200 (including the occasional 429)
// is an error, so the loop records an unknown reading and backs off.
func (claudeQuotaRecheckAdapter) UsageSnapshot(ctx context.Context, host QuotaRecheckHost, auth *Auth) (QuotaUsageSnapshot, error) {
	if auth.AuthKind() == AuthKindAPIKey {
		return QuotaUsageSnapshot{}, errors.New("usage snapshot needs an OAuth credential")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, claudeUsageSnapshotURLFor(auth), nil)
	if err != nil {
		return QuotaUsageSnapshot{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Anthropic-Beta", claudeUsageSnapshotBeta)
	resp, err := host.HttpRequest(ctx, auth, req)
	if err != nil {
		return QuotaUsageSnapshot{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, quotaProbeMaxBody))
	if err != nil {
		return QuotaUsageSnapshot{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return QuotaUsageSnapshot{}, fmt.Errorf("usage snapshot returned HTTP %d", resp.StatusCode)
	}
	signals := claudeUsageSignals(body)
	if len(signals) == 0 {
		return QuotaUsageSnapshot{}, errors.New("usage snapshot has no usage fields")
	}
	return QuotaUsageSnapshot{Signals: signals, ObservedAt: time.Now()}, nil
}

// Verdict sends one minimal inference request on the credential and judges the
// unified rate-limit headers of the response:
//   - allowed: a completed 200 whose unified status is allowed or
//     allowed_warning, with every window that set the cooldown reported as
//     allowed and no window rejected;
//   - limited: a 429, or a rejected status on a 200;
//   - anything else is an error (unknown) and keeps the cooldown.
//
// VerifyInference records the result through MarkResult, so a 429 re-derives
// the cooldown deadline there; the verdict only reports it.
func (claudeQuotaRecheckAdapter) Verdict(ctx context.Context, host QuotaRecheckHost, auth *Auth) (QuotaVerdict, error) {
	now := time.Now()
	windows, ok := claudeCooldownRejectedWindows(auth, now)
	if !ok {
		return QuotaVerdict{}, errors.New("no unified rate-limit cooldown to recheck")
	}
	model, err := claudeRecheckProbeModel(auth, now)
	if err != nil {
		return QuotaVerdict{}, err
	}
	started := time.Now()
	updated, errProbe := host.VerifyInference(ctx, auth.ID, model)
	return claudeProbeVerdict(auth, updated, errProbe, started, model, windows)
}

func claudeProbeVerdict(auth, updated *Auth, errProbe error, started time.Time, model string, windows []string) (QuotaVerdict, error) {
	verdict := QuotaVerdict{
		State:      QuotaVerdictUnknown,
		Signals:    map[string]string{"probe-model": model},
		ObservedAt: time.Now(),
		Source:     "inference-headers",
	}
	// Only a snapshot recorded by this probe (or by traffic that ran alongside
	// it, which is just as current) counts; an older one is stale.
	var signals map[string]string
	if updated != nil && !updated.Quota.ObservedAt.Before(started) {
		signals = updated.Quota.Signals
	}
	for key, val := range signals {
		verdict.Signals[key] = val
	}
	if statusCodeFromError(errProbe) == http.StatusTooManyRequests {
		verdict.State = QuotaVerdictLimited
		return verdict, nil
	}
	if errProbe != nil && !claudeProbeCompleted(auth, updated, errProbe) {
		return QuotaVerdict{}, fmt.Errorf("inference probe on %s: %w", model, errProbe)
	}
	if len(signals) == 0 {
		return QuotaVerdict{}, fmt.Errorf("inference probe on %s returned no rate-limit headers", model)
	}
	if hasRejectedClaudeUnifiedSignals(signals) {
		verdict.State = QuotaVerdictLimited
		return verdict, nil
	}
	if status := signals["Anthropic-Ratelimit-Unified-Status"]; !claudeUnifiedStatusAllows(status) {
		return QuotaVerdict{}, fmt.Errorf("inference probe on %s: unified status %q", model, status)
	}
	for _, window := range windows {
		if status := signals[claudeUnifiedWindowHeader(window, "status")]; !claudeUnifiedStatusAllows(status) {
			return QuotaVerdict{}, fmt.Errorf("inference probe on %s: window %s that set the cooldown reported %q", model, window, status)
		}
	}
	verdict.State = QuotaVerdictAllowed
	return verdict, nil
}

// claudeProbeCompleted reports whether VerifyInference's error still means the
// request completed with a 200. VerifyInference returns credential_superseded
// after a completed response whenever the credential is not inference-verified,
// and a live credential-scoped cooldown keeps that flag false. With the same
// credential version and epoch the credential was not actually replaced.
func claudeProbeCompleted(auth, updated *Auth, errProbe error) bool {
	var authErr *Error
	if !errors.As(errProbe, &authErr) || authErr == nil || authErr.Code != "credential_superseded" || updated == nil {
		return false
	}
	return updated.CredentialVersion == auth.CredentialVersion && updated.RegistrationEpoch == auth.RegistrationEpoch
}

func claudeUnifiedStatusAllows(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "allowed", "allowed_warning":
		return true
	default:
		return false
	}
}

// claudeCooldownRejectedWindows returns the unified windows whose rejection set
// the credential's live cooldowns. ok is false when no live cooldown comes from
// a unified rejection: ordinary model-level 429s keep their own short backoff.
func claudeCooldownRejectedWindows(auth *Auth, now time.Time) (windows []string, ok bool) {
	seen := make(map[string]bool)
	collect := func(signals map[string]string) {
		for key, val := range signals {
			if window, isWindow := claudeUnifiedWindowName(key); isWindow && strings.EqualFold(strings.TrimSpace(val), "rejected") && !seen[window] {
				seen[window] = true
				windows = append(windows, window)
			}
		}
	}
	if claudeCredentialCooldownLive(auth, now) {
		ok = true
		collect(auth.Quota.cooldownSignals)
	}
	for _, state := range auth.ModelStates {
		if state != nil && state.Quota.Exceeded && state.Quota.NextRecoverAt.After(now) && hasRejectedClaudeUnifiedSignals(state.Quota.cooldownSignals) {
			ok = true
			collect(state.Quota.cooldownSignals)
		}
	}
	sort.Strings(windows)
	return windows, ok
}

func claudeCredentialCooldownLive(auth *Auth, now time.Time) bool {
	return auth.Quota.Exceeded && auth.Quota.Reason == "credential_quota" && auth.Quota.NextRecoverAt.After(now)
}

// claudeRecheckProbeModel picks the cheapest model registered for the
// credential. Under a live credential-scoped cooldown a successful probe leaves
// every cooldown in place, so any model will do. Otherwise (model-level
// cooling) a success on a model clears that model's own cooldown, so a model
// without one is required.
func claudeRecheckProbeModel(auth *Auth, now time.Time) (string, error) {
	var ids []string
	for _, info := range registry.GetGlobalRegistry().GetModelsForClient(auth.ID) {
		if info != nil && strings.TrimSpace(info.ID) != "" {
			ids = append(ids, strings.TrimSpace(info.ID))
		}
	}
	if len(ids) == 0 {
		return "", errors.New("no model registered for the credential")
	}
	sort.SliceStable(ids, func(i, j int) bool { return claudeProbeModelRank(ids[i]) < claudeProbeModelRank(ids[j]) })
	if claudeCredentialCooldownLive(auth, now) {
		return ids[0], nil
	}
	for _, id := range ids {
		if !isModelStateActiveCooldown(existingModelState(auth, id), now) {
			return id, nil
		}
	}
	return "", errors.New("every model of the credential has its own cooldown")
}

func claudeProbeModelRank(model string) int {
	lower := strings.ToLower(model)
	switch {
	case strings.Contains(lower, "haiku"):
		return 0
	case strings.Contains(lower, "sonnet"):
		return 1
	default:
		return 2
	}
}

// claudeUsageSnapshotURLFor follows a configured base URL, else the default.
func claudeUsageSnapshotURLFor(auth *Auth) string {
	if auth != nil && auth.Attributes != nil {
		if base := strings.TrimRight(strings.TrimSpace(auth.Attributes["base_url"]), "/"); base != "" {
			return base + "/api/oauth/usage"
		}
	}
	return claudeUsageSnapshotURL
}

// claudeUsageSignals flattens the oauth/usage fields the change trigger uses.
// Absent and null fields are omitted.
func claudeUsageSignals(body []byte) map[string]string {
	root := ojson.ParseBytes(body)
	if !root.IsObject() {
		return nil
	}
	signals := make(map[string]string)
	put := func(key string, val ojson.Result) {
		if val.Exists() && val.Type != ojson.Null {
			if s := strings.TrimSpace(val.String()); s != "" {
				signals[key] = s
			}
		}
	}
	for _, window := range []string{"five_hour", "seven_day"} {
		put(window+".utilization", root.Get(window+".utilization"))
		put(window+".resets_at", root.Get(window+".resets_at"))
	}
	for i, limit := range root.Get("limits").Array() {
		kind := strings.ToLower(strings.TrimSpace(limit.Get("kind").String()))
		if kind == "" {
			kind = strconv.Itoa(i)
		}
		if _, dup := signals["limits."+kind+".is_active"]; dup {
			kind += "#" + strconv.Itoa(i)
		}
		for _, field := range []string{"percent", "severity", "resets_at", "is_active"} {
			put("limits."+kind+"."+field, limit.Get(field))
		}
	}
	for _, field := range []string{"is_enabled", "user_disabled", "spend_limit_reached"} {
		put("extra_usage."+field, root.Get("extra_usage."+field))
	}
	put("spend.enabled", root.Get("spend.enabled"))
	return signals
}

// claudeUsageSnapshotChanged reports a change that may mean a limit lifted: a
// reset time moved, a percentage dropped, a limit's severity or activity
// changed, or extra usage or spend settings toggled. A rising percentage is not
// a trigger. prev empty means there is no baseline yet.
func claudeUsageSnapshotChanged(prev, next map[string]string) bool {
	if len(prev) == 0 {
		return false
	}
	keys := make(map[string]struct{}, len(prev)+len(next))
	for key := range prev {
		keys[key] = struct{}{}
	}
	for key := range next {
		keys[key] = struct{}{}
	}
	for key := range keys {
		p, inPrev := prev[key]
		n, inNext := next[key]
		switch {
		case strings.HasSuffix(key, ".utilization"), strings.HasSuffix(key, ".percent"):
			pf, errP := strconv.ParseFloat(p, 64)
			nf, errN := strconv.ParseFloat(n, 64)
			if inPrev && inNext && errP == nil && errN == nil && nf < pf {
				return true
			}
		case strings.HasSuffix(key, ".resets_at"):
			if inPrev != inNext {
				return true
			}
			pt, errP := time.Parse(time.RFC3339, p)
			nt, errN := time.Parse(time.RFC3339, n)
			if errP != nil || errN != nil {
				if p != n {
					return true
				}
				continue
			}
			if diff := nt.Sub(pt); diff > claudeUsageResetTolerance || diff < -claudeUsageResetTolerance {
				return true
			}
		default:
			if inPrev != inNext || p != n {
				return true
			}
		}
	}
	return false
}
