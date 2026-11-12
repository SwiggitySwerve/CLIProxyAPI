package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// rebindCharacterizationRow is one auth state and what the session-affinity
// rebind check (rebind-on quota and never) answers for charModel. changed
// names why the answer differs from the copy the check replaced.
type rebindCharacterizationRow struct {
	name    string
	auth    func(now time.Time) *Auth
	rebind  bool
	changed string
}

// rebindAnswers maps each availability characterization row to the rebind
// answer. A pickable credential rebinds only on a rebind-worthy recorded
// failure: the two expired-quota rows still carry their 429.
var rebindAnswers = map[string]bool{
	"active, no model states":           false,
	"active, healthy state for M and N": false,
	"disabled":                          true,
	"auth Unavailable, live NextRetryAfter, no model states":                               false,
	"auth Unavailable, no NextRetryAfter, no model states":                                 true,
	"auth Unavailable, expired NextRetryAfter, no model states":                            false,
	"auth Unavailable, live NextRetryAfter, healthy states for M and N":                    false,
	"auth Unavailable, live NextRetryAfter, healthy state for M only (states < supported)": false,
	"auth Unavailable, no NextRetryAfter, healthy states for M and N":                      false,
	"credential_quota live, no model states":                                               true,
	"credential_quota live, propagated to M and N":                                         true,
	"credential_quota expired, no model states":                                            false,
	"credential_quota expired, propagated to M and N":                                      true,
	"model cooldown on M, N healthy":                                                       true,
	"model cooldown on N, M healthy":                                                       false,
	"model cooldown on N, no state for M (states < supported, aggregate Unavailable)":      false,
	"model cooldown on M and N (aggregate Unavailable)":                                    true,
	"model cooldown expired on M and N (aggregate Unavailable not yet recomputed)":         true,
	"model disabled on M":                               true,
	"terminal unauthorized":                             true,
	"terminal unauthorized, healthy states for M and N": true,
	"rejected access token awaiting refresh":            true,
}

func rebindCharModelState(mutate func(*ModelState)) func(now time.Time) *Auth {
	return func(now time.Time) *Auth {
		state := &ModelState{Status: StatusError, UpdatedAt: now}
		mutate(state)
		return charAuth(func(a *Auth) { a.ModelStates = map[string]*ModelState{charModel: state} })
	}
}

func rebindCharacterizationRows() []rebindCharacterizationRow {
	rows := make([]rebindCharacterizationRow, 0, len(rebindAnswers)+7)
	for _, row := range availabilityCharacterizationRows() {
		rows = append(rows, rebindCharacterizationRow{name: row.name, auth: row.auth, rebind: rebindAnswers[row.name]})
	}
	return append(rows,
		rebindCharacterizationRow{
			name:   "transient 503 cooldown on M",
			rebind: false,
			auth: func(now time.Time) *Auth {
				return rebindCharModelState(func(s *ModelState) {
					s.Unavailable = true
					s.NextRetryAfter = now.Add(time.Minute)
					s.LastError = &Error{HTTPStatus: http.StatusServiceUnavailable}
				})(now)
			},
		},
		rebindCharacterizationRow{
			name:   "403 cooldown on M",
			rebind: true,
			auth: func(now time.Time) *Auth {
				return rebindCharModelState(func(s *ModelState) {
					s.Unavailable = true
					s.NextRetryAfter = now.Add(time.Minute)
					s.LastError = &Error{HTTPStatus: http.StatusForbidden}
				})(now)
			},
		},
		rebindCharacterizationRow{
			name:   "pickable M after a 429 with cooling disabled",
			rebind: true,
			auth: rebindCharModelState(func(s *ModelState) {
				s.LastError = &Error{HTTPStatus: http.StatusTooManyRequests}
			}),
		},
		rebindCharacterizationRow{
			name:    "M unavailable with no recovery time after a 503",
			rebind:  true,
			changed: "no recovery time: the pin cannot serve until something else clears it",
			auth: rebindCharModelState(func(s *ModelState) {
				s.Unavailable = true
				s.LastError = &Error{HTTPStatus: http.StatusServiceUnavailable}
			}),
		},
		rebindCharacterizationRow{
			name:    "credential_quota with no recovery time, M healthy",
			rebind:  false,
			changed: "selection picks it",
			auth: func(now time.Time) *Auth {
				return charAuth(func(a *Auth) {
					a.Quota = QuotaState{Exceeded: true, Reason: "credential_quota"}
					a.ModelStates = map[string]*ModelState{charModel: charHealthyState(now)}
				})
			},
		},
		rebindCharacterizationRow{
			name:   "credential_quota with no recovery time, no model states",
			rebind: true,
			auth: func(now time.Time) *Auth {
				return charAuth(func(a *Auth) { a.Quota = QuotaState{Exceeded: true, Reason: "credential_quota"} })
			},
		},
		rebindCharacterizationRow{
			name:    "M quota with no recovery time, NextRetryAfter expired",
			rebind:  false,
			changed: "selection picks it",
			auth: func(now time.Time) *Auth {
				return rebindCharModelState(func(s *ModelState) {
					s.Unavailable = true
					s.NextRetryAfter = now.Add(-time.Minute)
					s.Quota = QuotaState{Exceeded: true, Reason: "quota"}
				})(now)
			},
		},
	)
}

// TestRebindPredicateCharacterization pins the rebind check's answer for every
// availability characterization state plus the rebind-specific ones.
func TestRebindPredicateCharacterization(t *testing.T) {
	now := time.Now()
	for _, row := range rebindCharacterizationRows() {
		t.Run(row.name, func(t *testing.T) {
			if got := isAuthBlockedByQuotaOrAuthForModel(row.auth(now), charModel, now); got != row.rebind {
				t.Fatalf("rebind = %v, want %v", got, row.rebind)
			}
		})
	}
	for _, row := range availabilityCharacterizationRows() {
		if _, ok := rebindAnswers[row.name]; !ok {
			t.Errorf("availability row %q has no rebind answer", row.name)
		}
	}
}

// TestRebindPredicateAgreesWithAvailability asserts the rebind check never
// contradicts the availability predicate: a credential it blocks for a quota
// cooldown, for being disabled, or with no recovery time always rebinds, and
// a pickable or transiently cooled one rebinds only on a rebind-worthy failure.
func TestRebindPredicateAgreesWithAvailability(t *testing.T) {
	now := time.Now()
	for _, row := range rebindCharacterizationRows() {
		auth := row.auth(now)
		blocked, reason, next := isAuthBlockedForModel(auth, charModel, now)
		rebind := isAuthBlockedByQuotaOrAuthForModel(auth, charModel, now)
		hard := blocked && (reason != blockReasonOther || next.IsZero())
		if hard && !rebind {
			t.Errorf("%s: selection cannot serve it until something clears it, rebind keeps the pin", row.name)
		}
		if !hard && rebind != hasRebindWorthyFailureForModel(auth, charModel) {
			t.Errorf("%s: rebind = %v without a hard block, want the recorded failure's answer", row.name, rebind)
		}
	}
}

// A bound credential with no recovery time is not transient: rebind-on quota
// must move the session instead of failing over forever without rebinding,
// and rebind-on never must report it unavailable instead of "retry".
func TestSessionAffinityRebindsWhenBoundAuthHasNoRecoveryTime(t *testing.T) {
	t.Parallel()

	stuck := func(a *Auth) {
		a.ModelStates = map[string]*ModelState{
			"model": {Status: StatusError, Unavailable: true, LastError: &Error{HTTPStatus: http.StatusServiceUnavailable}},
		}
	}
	opts := cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.DerivedSessionIDMetadataKey: "no-recovery-session"}}

	t.Run("quota", func(t *testing.T) {
		selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &FillFirstSelector{}, RebindOn: SessionAffinityRebindOnQuota})
		t.Cleanup(selector.Stop)
		authA := &Auth{ID: "auth-a", Provider: "codex"}
		authB := &Auth{ID: "auth-b", Provider: "codex"}
		guardianTestPick(t, selector, "codex", "model", opts, []*Auth{authB}, "auth-b")
		stuck(authB)
		guardianTestPick(t, selector, "codex", "model", opts, []*Auth{authA, authB}, "auth-a")
		authB.ModelStates = nil
		// Rebound: the session stays on auth-a once auth-b recovers.
		guardianTestPick(t, selector, "codex", "model", opts, []*Auth{authA, authB}, "auth-a")
	})

	t.Run("never", func(t *testing.T) {
		selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &FillFirstSelector{}, RebindOn: SessionAffinityRebindOnNever})
		t.Cleanup(selector.Stop)
		authA := &Auth{ID: "auth-a", Provider: "codex"}
		authB := &Auth{ID: "auth-b", Provider: "codex"}
		guardianTestPick(t, selector, "codex", "model", opts, []*Auth{authB}, "auth-b")
		stuck(authB)
		_, errPick := selector.Pick(context.Background(), "codex", "model", opts, []*Auth{authA, authB})
		var authErr *Error
		if !errors.As(errPick, &authErr) || authErr.Code != "session_auth_unavailable" {
			t.Fatalf("pick err = %v, want session_auth_unavailable", errPick)
		}
	})
}
