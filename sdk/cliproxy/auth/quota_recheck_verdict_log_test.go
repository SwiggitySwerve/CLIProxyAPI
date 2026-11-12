package auth

import (
	"context"
	"errors"
	"strings"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

// setupInfoLoggerHook captures info lines; setupTestLoggerHook restores the
// level on cleanup.
func setupInfoLoggerHook(t *testing.T) *logtest.Hook {
	t.Helper()
	hook := setupTestLoggerHook(t)
	log.SetLevel(log.InfoLevel)
	return hook
}

// recheckVerdictEntries returns the info lines the recheck loop wrote about a
// verdict that kept the cooldown.
func recheckVerdictEntries(entries []*log.Entry) []*log.Entry {
	var out []*log.Entry
	for _, entry := range entries {
		if entry.Level == log.InfoLevel && strings.Contains(entry.Message, "quota recheck: verdict") {
			out = append(out, entry)
		}
	}
	return out
}

// The production formatter prints only a fixed set of fields, so the verdict
// and the deadline it keeps must be in the message itself.
func TestQuotaRecheckLogsEveryVerdictThatKeepsTheCooldown(t *testing.T) {
	cases := map[string]struct {
		mode     internalconfig.QuotaReconcileMode
		verdict  QuotaVerdictState
		probeErr error
		want     string
	}{
		"clear mode, limited":       {mode: internalconfig.QuotaReconcileClear, verdict: QuotaVerdictLimited, want: "verdict=limited"},
		"clear mode, unknown":       {mode: internalconfig.QuotaReconcileClear, verdict: QuotaVerdictUnknown, want: "verdict=unknown"},
		"clear mode, probe failed":  {mode: internalconfig.QuotaReconcileClear, probeErr: errors.New("transport reset"), want: "verdict=unknown"},
		"observe mode, allowed":     {mode: internalconfig.QuotaReconcileObserve, verdict: QuotaVerdictAllowed, want: "verdict=allowed"},
		"observe mode, probe error": {mode: internalconfig.QuotaReconcileObserve, probeErr: errors.New("transport reset"), want: "verdict=unknown"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			hook := setupInfoLoggerHook(t)
			manager, adapter, _ := newRecheckManager(t, claudeConfig(tc.mode), tc.verdict)
			adapter.err = tc.probeErr
			registerCooledAuth(t, manager, "claude-1", "claude")

			manager.reconcileQuotaCooldowns(context.Background())

			entries := recheckVerdictEntries(hook.AllEntries())
			if len(entries) != 1 {
				t.Fatalf("verdict lines = %d, want 1", len(entries))
			}
			msg := entries[0].Message
			for _, want := range []string{tc.want, "cooldown kept until "} {
				if !strings.Contains(msg, want) {
					t.Fatalf("message %q lacks %q", msg, want)
				}
			}
			// reason is one of the fields the production formatter prints.
			if tc.probeErr != nil && entries[0].Data["reason"] != tc.probeErr.Error() {
				t.Fatalf("reason field = %v, want the probe error", entries[0].Data["reason"])
			}
			if got := entries[0].Data["mode"]; got != string(tc.mode) {
				t.Fatalf("mode field = %v, want %s", got, tc.mode)
			}
			if got, _ := manager.GetByID("claude-1"); !got.Quota.Exceeded {
				t.Fatalf("cooldown was cleared")
			}
		})
	}
}

func TestQuotaRecheckClearedVerdictWritesNoKeptLine(t *testing.T) {
	hook := setupInfoLoggerHook(t)
	manager, _, _ := newRecheckManager(t, claudeConfig(internalconfig.QuotaReconcileClear), QuotaVerdictAllowed)
	registerCooledAuth(t, manager, "claude-1", "claude")

	manager.reconcileQuotaCooldowns(context.Background())

	if entries := recheckVerdictEntries(hook.AllEntries()); len(entries) != 0 {
		t.Fatalf("a clearing verdict wrote %d kept lines", len(entries))
	}
}
