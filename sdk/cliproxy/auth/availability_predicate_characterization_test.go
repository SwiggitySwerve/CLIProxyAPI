package auth

import (
	"net/http"
	"testing"
	"time"
)

// availabilityAnswers records, for one auth state and the requested model,
// what each availability consumer answers.
//
//   - pick: selection (selectors, session affinity, the scheduler's per-model
//     shard state) through isAuthBlockedForModel.
//   - cooled: the all-cooled warning (warnLogAuthUnavailable) counts the
//     credential as cooling (blocked with reason cooldown).
//   - credBlocked: the scheduler's credential-level transition check
//     (isCredentialBlocked) that decides whether every model shard is resynced.
//     It must never claim blocked while selection can pick the model.
//   - listed: the /v1/models projection keeps the model visible (not Suspended).
type availabilityAnswers struct {
	pick        bool
	cooled      bool
	credBlocked bool
	listed      bool
}

// The credential supports charModel (M, the requested model) and charOther (N).
const (
	charModel    = "char-model-m"
	charOther    = "char-model-n"
	charProvider = "char-provider"
)

func availabilityAnswersFor(t *testing.T, m *Manager, auth *Auth, now time.Time) availabilityAnswers {
	t.Helper()
	blocked, reason, _ := isAuthBlockedForModel(auth, charModel, now)
	projection := m.clientModelProjectionForAuth(auth, charModel, now)
	return availabilityAnswers{
		pick:        !blocked,
		cooled:      blocked && reason == blockReasonCooldown,
		credBlocked: isCredentialBlocked(auth, now),
		listed:      !projection.Suspended,
	}
}

func charHealthyState(now time.Time) *ModelState {
	return &ModelState{Status: StatusActive, UpdatedAt: now}
}

// charModelQuotaState is the shape MarkResult writes for a model-scoped 429.
func charModelQuotaState(now, until time.Time) *ModelState {
	return &ModelState{
		Status:         StatusError,
		StatusMessage:  "quota exhausted",
		Unavailable:    true,
		NextRetryAfter: until,
		Quota:          QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: until},
		LastError:      &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota exhausted"},
		UpdatedAt:      now,
	}
}

// charCredentialQuotaState is the shape a credential-scoped 429 propagates to
// every model state.
func charCredentialQuotaState(now, until time.Time) *ModelState {
	state := charModelQuotaState(now, until)
	state.Quota.Reason = "credential_quota"
	state.credentialQuotaPropagated = true
	return state
}

func charAuth(mutate func(*Auth)) *Auth {
	auth := &Auth{ID: "char-auth", Provider: charProvider, Status: StatusActive}
	if mutate != nil {
		mutate(auth)
	}
	return auth
}

// availabilityCharacterizationRow is one auth state. changed names why the
// shared predicate deliberately changed a cell; every changed cell was
// credBlocked: true before the scheduler read the shared predicate.
type availabilityCharacterizationRow struct {
	name    string
	auth    func(now time.Time) *Auth
	want    availabilityAnswers
	changed string
}

func availabilityCharacterizationRows() []availabilityCharacterizationRow {
	return []availabilityCharacterizationRow{
		{
			name: "active, no model states",
			auth: func(now time.Time) *Auth { return charAuth(nil) },
			want: availabilityAnswers{pick: true, cooled: false, credBlocked: false, listed: true},
		},
		{
			name: "active, healthy state for M and N",
			auth: func(now time.Time) *Auth {
				return charAuth(func(a *Auth) {
					a.ModelStates = map[string]*ModelState{charModel: charHealthyState(now), charOther: charHealthyState(now)}
				})
			},
			want: availabilityAnswers{pick: true, cooled: false, credBlocked: false, listed: true},
		},
		{
			name: "disabled",
			auth: func(now time.Time) *Auth {
				return charAuth(func(a *Auth) { a.Disabled = true; a.Status = StatusDisabled })
			},
			want: availabilityAnswers{pick: false, cooled: false, credBlocked: true, listed: false},
		},
		{
			name: "auth Unavailable, live NextRetryAfter, no model states",
			auth: func(now time.Time) *Auth {
				return charAuth(func(a *Auth) {
					a.Status = StatusError
					a.Unavailable = true
					a.NextRetryAfter = now.Add(time.Minute)
				})
			},
			want: availabilityAnswers{pick: false, cooled: false, credBlocked: true, listed: false},
		},
		{
			name: "auth Unavailable, no NextRetryAfter, no model states",
			auth: func(now time.Time) *Auth {
				return charAuth(func(a *Auth) { a.Status = StatusError; a.Unavailable = true })
			},
			want: availabilityAnswers{pick: false, cooled: false, credBlocked: true, listed: false},
		},
		{
			name: "auth Unavailable, expired NextRetryAfter, no model states",
			auth: func(now time.Time) *Auth {
				return charAuth(func(a *Auth) {
					a.Status = StatusError
					a.Unavailable = true
					a.NextRetryAfter = now.Add(-time.Minute)
				})
			},
			want:    availabilityAnswers{pick: true, cooled: false, credBlocked: false, listed: true},
			changed: "expired cooldown",
		},
		{
			name: "auth Unavailable, live NextRetryAfter, healthy states for M and N",
			auth: func(now time.Time) *Auth {
				return charAuth(func(a *Auth) {
					a.Status = StatusError
					a.Unavailable = true
					a.NextRetryAfter = now.Add(time.Minute)
					a.ModelStates = map[string]*ModelState{charModel: charHealthyState(now), charOther: charHealthyState(now)}
				})
			},
			want:    availabilityAnswers{pick: true, cooled: false, credBlocked: false, listed: true},
			changed: "aggregate of model states",
		},
		{
			name: "auth Unavailable, live NextRetryAfter, healthy state for M only (states < supported)",
			auth: func(now time.Time) *Auth {
				return charAuth(func(a *Auth) {
					a.Status = StatusError
					a.Unavailable = true
					a.NextRetryAfter = now.Add(time.Minute)
					a.ModelStates = map[string]*ModelState{charModel: charHealthyState(now)}
				})
			},
			want: availabilityAnswers{pick: true, cooled: false, credBlocked: false, listed: true},
		},
		{
			name: "auth Unavailable, no NextRetryAfter, healthy states for M and N",
			auth: func(now time.Time) *Auth {
				return charAuth(func(a *Auth) {
					a.Status = StatusError
					a.Unavailable = true
					a.ModelStates = map[string]*ModelState{charModel: charHealthyState(now), charOther: charHealthyState(now)}
				})
			},
			want:    availabilityAnswers{pick: true, cooled: false, credBlocked: false, listed: true},
			changed: "aggregate of model states",
		},
		{
			name: "credential cooldown from a model-less failure, healthy states for M and N",
			auth: func(now time.Time) *Auth {
				until := now.Add(time.Minute)
				return charAuth(func(a *Auth) {
					a.Status = StatusError
					a.Unavailable = true
					a.NextRetryAfter = until
					a.credentialCooldownUntil = until
					a.ModelStates = map[string]*ModelState{charModel: charHealthyState(now), charOther: charHealthyState(now)}
				})
			},
			want: availabilityAnswers{pick: false, cooled: false, credBlocked: true, listed: false},
		},
		{
			name: "credential quota cooldown from a model-less 429, healthy states for M and N",
			auth: func(now time.Time) *Auth {
				until := now.Add(time.Minute)
				return charAuth(func(a *Auth) {
					a.Status = StatusError
					a.Unavailable = true
					a.NextRetryAfter = until
					a.Quota = QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: until}
					a.credentialCooldownUntil = until
					a.ModelStates = map[string]*ModelState{charModel: charHealthyState(now), charOther: charHealthyState(now)}
				})
			},
			want: availabilityAnswers{pick: false, cooled: true, credBlocked: true, listed: false},
		},
		{
			name: "credential cooldown, M's own quota cooldown outlasts it",
			auth: func(now time.Time) *Auth {
				until := now.Add(time.Minute)
				return charAuth(func(a *Auth) {
					a.Status = StatusError
					a.Unavailable = true
					a.NextRetryAfter = until
					a.credentialCooldownUntil = until
					a.ModelStates = map[string]*ModelState{charModel: charModelQuotaState(now, now.Add(10*time.Minute)), charOther: charHealthyState(now)}
				})
			},
			want: availabilityAnswers{pick: false, cooled: true, credBlocked: true, listed: false},
		},
		{
			name: "credential cooldown ending with M's own quota cooldown",
			auth: func(now time.Time) *Auth {
				until := now.Add(time.Minute)
				return charAuth(func(a *Auth) {
					a.Status = StatusError
					a.Unavailable = true
					a.NextRetryAfter = until
					a.credentialCooldownUntil = until
					a.ModelStates = map[string]*ModelState{charModel: charModelQuotaState(now, until), charOther: charHealthyState(now)}
				})
			},
			want: availabilityAnswers{pick: false, cooled: true, credBlocked: true, listed: false},
		},
		{
			name: "credential cooldown marker after Unavailable was cleared, healthy states for M and N",
			auth: func(now time.Time) *Auth {
				return charAuth(func(a *Auth) {
					a.credentialCooldownUntil = now.Add(time.Minute)
					a.ModelStates = map[string]*ModelState{charModel: charHealthyState(now), charOther: charHealthyState(now)}
				})
			},
			want: availabilityAnswers{pick: true, cooled: false, credBlocked: false, listed: true},
		},
		{
			name: "credential_quota live, no model states",
			auth: func(now time.Time) *Auth {
				until := now.Add(time.Hour)
				return charAuth(func(a *Auth) {
					a.Status = StatusError
					a.Unavailable = true
					a.NextRetryAfter = until
					a.Quota = QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: until}
				})
			},
			want: availabilityAnswers{pick: false, cooled: true, credBlocked: true, listed: false},
		},
		{
			name: "credential_quota live, propagated to M and N",
			auth: func(now time.Time) *Auth {
				until := now.Add(time.Hour)
				return charAuth(func(a *Auth) {
					a.Status = StatusError
					a.Unavailable = true
					a.NextRetryAfter = until
					a.Quota = QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: until}
					a.ModelStates = map[string]*ModelState{charModel: charCredentialQuotaState(now, until), charOther: charCredentialQuotaState(now, until)}
				})
			},
			want: availabilityAnswers{pick: false, cooled: true, credBlocked: true, listed: false},
		},
		{
			name: "credential_quota expired, no model states",
			auth: func(now time.Time) *Auth {
				until := now.Add(-time.Minute)
				return charAuth(func(a *Auth) {
					a.Status = StatusError
					a.Unavailable = true
					a.NextRetryAfter = until
					a.Quota = QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: until}
				})
			},
			want:    availabilityAnswers{pick: true, cooled: false, credBlocked: false, listed: true},
			changed: "expired cooldown",
		},
		{
			name: "credential_quota expired, propagated to M and N",
			auth: func(now time.Time) *Auth {
				until := now.Add(-time.Minute)
				return charAuth(func(a *Auth) {
					a.Status = StatusError
					a.Unavailable = true
					a.NextRetryAfter = until
					a.Quota = QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: until}
					a.ModelStates = map[string]*ModelState{charModel: charCredentialQuotaState(now, until), charOther: charCredentialQuotaState(now, until)}
				})
			},
			want:    availabilityAnswers{pick: true, cooled: false, credBlocked: false, listed: true},
			changed: "expired cooldown",
		},
		{
			name: "model cooldown on M, N healthy",
			auth: func(now time.Time) *Auth {
				until := now.Add(5 * time.Minute)
				return charAuth(func(a *Auth) {
					a.Status = StatusError
					a.Quota = QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: until}
					a.ModelStates = map[string]*ModelState{charModel: charModelQuotaState(now, until), charOther: charHealthyState(now)}
				})
			},
			want: availabilityAnswers{pick: false, cooled: true, credBlocked: false, listed: false},
		},
		{
			name: "model cooldown on N, M healthy",
			auth: func(now time.Time) *Auth {
				until := now.Add(5 * time.Minute)
				return charAuth(func(a *Auth) {
					a.Status = StatusError
					a.Quota = QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: until}
					a.ModelStates = map[string]*ModelState{charModel: charHealthyState(now), charOther: charModelQuotaState(now, until)}
				})
			},
			want: availabilityAnswers{pick: true, cooled: false, credBlocked: false, listed: true},
		},
		{
			name: "model cooldown on N, no state for M (states < supported, aggregate Unavailable)",
			auth: func(now time.Time) *Auth {
				until := now.Add(5 * time.Minute)
				return charAuth(func(a *Auth) {
					a.Status = StatusError
					a.Unavailable = true
					a.NextRetryAfter = until
					a.Quota = QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: until}
					a.ModelStates = map[string]*ModelState{charOther: charModelQuotaState(now, until)}
				})
			},
			want: availabilityAnswers{pick: true, cooled: false, credBlocked: false, listed: true},
		},
		{
			name: "model cooldown on M and N (aggregate Unavailable)",
			auth: func(now time.Time) *Auth {
				until := now.Add(5 * time.Minute)
				return charAuth(func(a *Auth) {
					a.Status = StatusError
					a.Unavailable = true
					a.NextRetryAfter = until
					a.Quota = QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: until}
					a.ModelStates = map[string]*ModelState{charModel: charModelQuotaState(now, until), charOther: charModelQuotaState(now, until)}
				})
			},
			want:    availabilityAnswers{pick: false, cooled: true, credBlocked: false, listed: false},
			changed: "aggregate of model states",
		},
		{
			name: "model cooldown expired on M and N (aggregate Unavailable not yet recomputed)",
			auth: func(now time.Time) *Auth {
				until := now.Add(-time.Minute)
				return charAuth(func(a *Auth) {
					a.Status = StatusError
					a.Unavailable = true
					a.NextRetryAfter = until
					a.Quota = QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: until}
					a.ModelStates = map[string]*ModelState{charModel: charModelQuotaState(now, until), charOther: charModelQuotaState(now, until)}
				})
			},
			want:    availabilityAnswers{pick: true, cooled: false, credBlocked: false, listed: true},
			changed: "expired cooldown",
		},
		{
			name: "model disabled on M",
			auth: func(now time.Time) *Auth {
				return charAuth(func(a *Auth) {
					a.ModelStates = map[string]*ModelState{charModel: {Status: StatusDisabled}, charOther: charHealthyState(now)}
				})
			},
			want: availabilityAnswers{pick: false, cooled: false, credBlocked: false, listed: false},
		},
		{
			name: "terminal unauthorized",
			auth: func(now time.Time) *Auth {
				return charAuth(func(a *Auth) {
					a.Status = StatusError
					a.Unavailable = true
					a.StatusMessage = "unauthorized"
					a.LastError = &Error{Code: "unauthorized", HTTPStatus: http.StatusUnauthorized, Message: "unauthorized"}
				})
			},
			want: availabilityAnswers{pick: false, cooled: false, credBlocked: true, listed: false},
		},
		{
			name: "terminal unauthorized, healthy states for M and N",
			auth: func(now time.Time) *Auth {
				return charAuth(func(a *Auth) {
					a.Status = StatusError
					a.Unavailable = true
					a.StatusMessage = "unauthorized"
					a.LastError = &Error{Code: "unauthorized", HTTPStatus: http.StatusUnauthorized, Message: "unauthorized"}
					a.ModelStates = map[string]*ModelState{charModel: charHealthyState(now), charOther: charHealthyState(now)}
				})
			},
			want: availabilityAnswers{pick: false, cooled: false, credBlocked: true, listed: false},
		},
		{
			name: "rejected access token awaiting refresh",
			auth: func(now time.Time) *Auth {
				return charAuth(func(a *Auth) {
					a.Metadata = map[string]any{"access_token": "char-token"}
					a.RejectedAccessToken = "char-token"
					a.NextRefreshAfter = now
				})
			},
			want: availabilityAnswers{pick: false, cooled: false, credBlocked: true, listed: true},
		},
	}
}

// TestAvailabilityPredicateCharacterization pins what each availability
// consumer answers for the same auth state. Agreement between consumers is
// checked by TestAvailabilityPredicateDisagreements.
func TestAvailabilityPredicateCharacterization(t *testing.T) {
	m := NewManager(nil, nil, nil)
	now := time.Now()
	for _, row := range availabilityCharacterizationRows() {
		t.Run(row.name, func(t *testing.T) {
			got := availabilityAnswersFor(t, m, row.auth(now), now)
			if got != row.want {
				t.Fatalf("answers = %+v, want %+v", got, row.want)
			}
		})
	}
}

// availabilityListingDivergences are the rows where the /v1/models projection
// still disagrees with selection. The projection is recomputed on results and
// refreshes, while token expiry is time-driven with no deadline to project, so
// it does not read the shared predicate yet.
var availabilityListingDivergences = map[string]bool{
	"rejected access token awaiting refresh": true,
}

// TestAvailabilityPredicateDisagreements asserts that selection, the all-cooled
// warning and the scheduler's credential-level check agree on every row, and
// that /v1/models agrees except for the documented divergences.
func TestAvailabilityPredicateDisagreements(t *testing.T) {
	m := NewManager(nil, nil, nil)
	now := time.Now()
	for _, row := range availabilityCharacterizationRows() {
		got := availabilityAnswersFor(t, m, row.auth(now), now)
		if got.cooled && got.pick {
			t.Errorf("%s: the warning counts it as cooled but selection can pick it", row.name)
		}
		// credBlocked is credential-wide: it may only claim blocked when the
		// requested model is unpickable too.
		if got.credBlocked && got.pick {
			t.Errorf("%s: scheduler credential check blocked, selection picks", row.name)
		}
		if got.pick != got.listed && !availabilityListingDivergences[row.name] {
			t.Errorf("%s: selection pick=%v, /v1/models listed=%v", row.name, got.pick, got.listed)
		}
	}
}
