package auth

import (
	"context"
	"strconv"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// SoonestResetSelector picks the available credential whose longest quota window
// resets soonest, so quota that is about to expire is spent first.
//
// It considers the same candidates as FillFirstSelector and ranks them:
//  1. no current quota observation, or the observed reset time has passed: tried
//     first. "No current observation" means never observed, or observed longer ago
//     than soonestResetStaleAfter (such a snapshot is Unknown, not trusted). A
//     response without quota headers leaves the old snapshot in place, so such a
//     credential keeps this rank on every pick until a response with headers
//     arrives (meanwhile the strategy keeps choosing it, like fill-first);
//  2. a known future reset time: soonest first;
//  3. observed recently but without a usable reset time: last.
//
// The reset time comes from a per-provider function (providerQuota.resetWindow) that
// names the quota windows it reads. Claude: 7d_oi (only for models it covers), 7d,
// then unified; 5h is named but never ranked on. Codex: secondary, then primary.
// A provider without an entry has no reset time, so its credentials are ranked
// like fill-first.
//
// Ties keep the ID order, so credentials without quota observations behave like
// fill-first. It only reads Auth.Quota and keeps no state.
type SoonestResetSelector struct{}

const (
	soonestResetRankRefresh = iota
	soonestResetRankKnown
	soonestResetRankUnknown
)

// soonestResetStaleAfter is the age beyond which a quota observation counts as
// Unknown and the credential ranks in the refresh bucket, so it gets real traffic
// and a new observation. It is a little longer than the 5h Claude window: an older
// snapshot has outlived a whole short window, so the utilisation it came with no
// longer says anything. Observations are in memory only, so an idle credential
// would otherwise keep ranking on a snapshot of unbounded age.
const soonestResetStaleAfter = 6 * time.Hour

// maxQuotaResetHorizon bounds how far a reset time may be from its observation.
// Real windows are at most a week; anything beyond this is treated as garbage.
const maxQuotaResetHorizon = 31 * 24 * time.Hour

// soonestResetWindowFunc returns the reset time a credential is ranked on, and the
// name of the window it came from, given the credential's observed signals and the
// requested model. The bool is false when no usable reset time exists.
type soonestResetWindowFunc func(signals map[string]string, observedAt time.Time, model string) (time.Time, string, bool)

// Claude windows (Anthropic-Ratelimit-Unified-<window>-Reset, epoch seconds).
const (
	claudeWindow5h      = "5h"
	claudeWindow7d      = "7d"
	claudeWindow7dOI    = "7d_oi"
	claudeWindowUnified = "unified"
)

type claudeResetWindowSpec struct {
	name   string
	header string
	// ranked is false for a window that is read but never ranked on.
	ranked bool
	// appliesTo limits a model-scoped window to the models it covers; nil means all.
	appliesTo func(model string) bool
}

// claudeResetWindows is in preference order: the first ranked window that applies
// to the model and carries a usable reset wins. The default (7d, else unified) is
// today's choice. The 5h window is named but not ranked on: its reset says nothing
// about the weekly allowance this strategy spends.
var claudeResetWindows = []claudeResetWindowSpec{
	{name: claudeWindow7dOI, header: "Anthropic-Ratelimit-Unified-7d_oi-Reset", ranked: true, appliesTo: claudeModelHas7dOIWindow},
	{name: claudeWindow7d, header: "Anthropic-Ratelimit-Unified-7d-Reset", ranked: true},
	{name: claudeWindowUnified, header: "Anthropic-Ratelimit-Unified-Reset", ranked: true},
	{name: claudeWindow5h, header: "Anthropic-Ratelimit-Unified-5h-Reset"},
}

// claudeModelHas7dOIWindow reports whether the 7d_oi window covers the model. The
// executor treats 7d_oi through response headers only, never by model, so there is
// nothing to reuse: this is the one place the selector decides it. 7d_oi is the
// model-scoped Fable weekly window, so only Fable models match. The roadmap lists
// confirming that mapping as a measure-first item.
func claudeModelHas7dOIWindow(model string) bool {
	return strings.Contains(strings.ToLower(model), "fable")
}

func claudeResetWindow(signals map[string]string, observedAt time.Time, model string) (time.Time, string, bool) {
	for _, window := range claudeResetWindows {
		if !window.ranked || (window.appliesTo != nil && !window.appliesTo(model)) {
			continue
		}
		if resetAt, ok := epochQuotaReset(signals[window.header], observedAt); ok {
			return resetAt, window.name, true
		}
	}
	return time.Time{}, "", false
}

// Codex windows: X-Codex-<Window>-Reset-At (epoch seconds) or else
// X-Codex-<Window>-Reset-After-Seconds (relative to observedAt; zero means the
// window just reset). The secondary window is the longer one.
var codexResetWindows = []struct{ name, prefix string }{
	{name: "secondary", prefix: "X-Codex-Secondary-"},
	{name: "primary", prefix: "X-Codex-Primary-"},
}

func codexResetWindow(signals map[string]string, observedAt time.Time, _ string) (time.Time, string, bool) {
	for _, window := range codexResetWindows {
		if resetAt, ok := epochQuotaReset(signals[window.prefix+"Reset-At"], observedAt); ok {
			return resetAt, window.name, true
		}
		if seconds, ok := parseQuotaInt(signals[window.prefix+"Reset-After-Seconds"], 0); ok && seconds <= int64(maxQuotaResetHorizon/time.Second) {
			return observedAt.Add(time.Duration(seconds) * time.Second), window.name, true
		}
	}
	return time.Time{}, "", false
}

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
		rank, resetAt := soonestResetRank(candidate, model, now)
		if picked == nil || rank < pickedRank || (rank == pickedRank && resetAt.Before(pickedReset)) {
			picked, pickedRank, pickedReset = candidate, rank, resetAt
		}
	}
	return picked, nil
}

func soonestResetRank(auth *Auth, model string, now time.Time) (int, time.Time) {
	observedAt := auth.Quota.ObservedAt
	if observedAt.IsZero() || now.Sub(observedAt) > soonestResetStaleAfter {
		return soonestResetRankRefresh, time.Time{}
	}
	resetAt, _, ok := soonestResetWindow(auth.Provider, auth.Quota.Signals, observedAt, model)
	if !ok {
		return soonestResetRankUnknown, time.Time{}
	}
	if !resetAt.After(now) {
		return soonestResetRankRefresh, time.Time{}
	}
	return soonestResetRankKnown, resetAt
}

// soonestResetWindow returns the reset time to rank a credential of the provider
// on and the window it came from. Unparsable, negative, zero epoch or
// out-of-horizon values are skipped; a provider without a resetWindow in the
// provider registry has no reset time. Signals are collected per provider, so
// each window function reads only its own header names.
func soonestResetWindow(provider string, signals map[string]string, observedAt time.Time, model string) (time.Time, string, bool) {
	if len(signals) == 0 || observedAt.IsZero() {
		return time.Time{}, "", false
	}
	windowFor := providerQuotaFor(provider).resetWindow
	if windowFor == nil {
		return time.Time{}, "", false
	}
	return windowFor(signals, observedAt, model)
}

func epochQuotaReset(value string, observedAt time.Time) (time.Time, bool) {
	seconds, ok := parseQuotaInt(value, 1)
	if !ok {
		return time.Time{}, false
	}
	resetAt := time.Unix(seconds, 0)
	if resetAt.Sub(observedAt) > maxQuotaResetHorizon || observedAt.Sub(resetAt) > maxQuotaResetHorizon {
		return time.Time{}, false
	}
	return resetAt, true
}

func parseQuotaInt(value string, min int64) (int64, bool) {
	if value == "" {
		return 0, false
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < min {
		return 0, false
	}
	return parsed, true
}
