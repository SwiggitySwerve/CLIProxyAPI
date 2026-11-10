package auth

import (
	"context"
	"strconv"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// SoonestResetSelector picks the available credential whose longest quota window
// resets soonest, so quota that is about to expire is spent first.
//
// It considers the same candidates as FillFirstSelector and ranks them:
//  1. no quota observation yet, or the observed reset time has passed: tried first
//     so one request refreshes the snapshot;
//  2. a known future reset time: soonest first;
//  3. observed but without a usable reset time: last.
//
// Ties keep the ID order, so credentials without quota observations behave like
// fill-first. It only reads Auth.Quota and keeps no state.
type SoonestResetSelector struct{}

const (
	soonestResetRankRefresh = iota
	soonestResetRankKnown
	soonestResetRankUnknown
)

// maxQuotaResetHorizon bounds how far a reset time may be from its observation.
// Real windows are at most a week; anything beyond this is treated as garbage.
const maxQuotaResetHorizon = 31 * 24 * time.Hour

// Pick selects the available auth with the soonest quota reset.
func (s *SoonestResetSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = opts
	now := time.Now()
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	var picked *Auth
	pickedRank := 0
	var pickedReset time.Time
	for _, candidate := range available {
		rank, resetAt := soonestResetRank(candidate, now)
		if picked == nil || rank < pickedRank || (rank == pickedRank && resetAt.Before(pickedReset)) {
			picked, pickedRank, pickedReset = candidate, rank, resetAt
		}
	}
	return picked, nil
}

func soonestResetRank(auth *Auth, now time.Time) (int, time.Time) {
	if auth.Quota.ObservedAt.IsZero() {
		return soonestResetRankRefresh, time.Time{}
	}
	resetAt, ok := quotaWindowResetAt(auth.Quota.Signals, auth.Quota.ObservedAt)
	if !ok {
		return soonestResetRankUnknown, time.Time{}
	}
	if !resetAt.After(now) {
		return soonestResetRankRefresh, time.Time{}
	}
	return soonestResetRankKnown, resetAt
}

// quotaWindowResetAt returns the reset time of the longest quota window in an
// observed signal snapshot. Signals are collected per provider, so the Claude and
// Codex header names never appear on the same credential.
//
// Claude: Anthropic-Ratelimit-Unified-7d-Reset, else Anthropic-Ratelimit-Unified-Reset
// (epoch seconds). Codex: the secondary window, else the primary window, each
// X-Codex-<Window>-Reset-At (epoch seconds) or else X-Codex-<Window>-Reset-After-Seconds
// (relative to observedAt). Unparsable, non-positive or out-of-horizon values are skipped.
func quotaWindowResetAt(signals map[string]string, observedAt time.Time) (time.Time, bool) {
	if len(signals) == 0 || observedAt.IsZero() {
		return time.Time{}, false
	}
	for _, header := range []string{"Anthropic-Ratelimit-Unified-7d-Reset", "Anthropic-Ratelimit-Unified-Reset"} {
		if resetAt, ok := epochQuotaReset(signals[header], observedAt); ok {
			return resetAt, true
		}
	}
	for _, window := range []string{"X-Codex-Secondary-", "X-Codex-Primary-"} {
		if resetAt, ok := epochQuotaReset(signals[window+"Reset-At"], observedAt); ok {
			return resetAt, true
		}
		if seconds, ok := positiveQuotaInt(signals[window+"Reset-After-Seconds"]); ok && seconds <= int64(maxQuotaResetHorizon/time.Second) {
			return observedAt.Add(time.Duration(seconds) * time.Second), true
		}
	}
	return time.Time{}, false
}

func epochQuotaReset(value string, observedAt time.Time) (time.Time, bool) {
	seconds, ok := positiveQuotaInt(value)
	if !ok {
		return time.Time{}, false
	}
	resetAt := time.Unix(seconds, 0)
	if resetAt.Sub(observedAt) > maxQuotaResetHorizon || observedAt.Sub(resetAt) > maxQuotaResetHorizon {
		return time.Time{}, false
	}
	return resetAt, true
}

func positiveQuotaInt(value string) (int64, bool) {
	if value == "" {
		return 0, false
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return 0, false
	}
	return parsed, true
}
