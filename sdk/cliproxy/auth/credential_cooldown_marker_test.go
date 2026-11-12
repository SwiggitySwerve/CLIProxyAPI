package auth

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

const (
	credCooldownModelM = "cred-cooldown-model-m"
	credCooldownModelN = "cred-cooldown-model-n"
)

type credCooldownStatusError struct {
	status     int
	retryAfter time.Duration
}

func (e credCooldownStatusError) Error() string              { return http.StatusText(e.status) }
func (e credCooldownStatusError) StatusCode() int            { return e.status }
func (e credCooldownStatusError) RetryAfter() *time.Duration { return &e.retryAfter }

// credCooldownExecutor fails the models listed in errs and succeeds otherwise.
type credCooldownExecutor struct {
	mu    sync.Mutex
	errs  map[string]error
	calls []string
}

func (e *credCooldownExecutor) Identifier() string { return "codex" }

func (e *credCooldownExecutor) run(req cliproxyexecutor.Request) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, req.Model)
	return e.errs[req.Model]
}

func (e *credCooldownExecutor) Execute(_ context.Context, _ *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{Payload: []byte("ok")}, e.run(req)
}

func (e *credCooldownExecutor) ExecuteStream(_ context.Context, _ *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, e.run(req)
}

func (e *credCooldownExecutor) CountTokens(_ context.Context, _ *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, e.run(req)
}

func (*credCooldownExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) { return auth, nil }

func (*credCooldownExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func (e *credCooldownExecutor) callCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.calls)
}

func credCooldownSetup(t *testing.T, authID string) (*Manager, *credCooldownExecutor) {
	t.Helper()
	withQuotaCooldownEnabled(t)
	manager := NewManager(nil, nil, nil)
	executor := &credCooldownExecutor{errs: map[string]error{}}
	manager.RegisterExecutor(executor)
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: credCooldownModelM}, {ID: credCooldownModelN}})
	t.Cleanup(func() { reg.UnregisterClient(authID) })
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), &Auth{
		ID:       authID,
		Provider: "codex",
		Metadata: map[string]any{"type": "codex", "access_token": "token-1"},
	}); errRegister != nil {
		t.Fatalf("register %s: %v", authID, errRegister)
	}
	return manager, executor
}

func credCooldownExecute(manager *Manager, model string) error {
	_, err := manager.Execute(WithSkipPersist(context.Background()), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	return err
}

// A model-less failure (no Model, no RouteModel, e.g. a 502 reported by an SDK
// caller) records a credential-level cooldown. Selection must honour it even
// when the requested model has a healthy state of its own.
func TestModelLessFailureCooldownBlocksModelWithHealthyState(t *testing.T) {
	const authID = "cred-cooldown-modelless"
	manager, executor := credCooldownSetup(t, authID)

	if err := credCooldownExecute(manager, credCooldownModelM); err != nil {
		t.Fatalf("seed execute %s: %v", credCooldownModelM, err)
	}
	if err := credCooldownExecute(manager, credCooldownModelN); err != nil {
		t.Fatalf("seed execute %s: %v", credCooldownModelN, err)
	}

	manager.MarkResult(WithSkipPersist(context.Background()), Result{
		AuthID:   authID,
		Provider: "codex",
		Success:  false,
		Error:    &Error{HTTPStatus: http.StatusBadGateway, Message: "bad gateway"},
	})

	now := time.Now()
	current, ok := manager.GetByID(authID)
	if !ok || current == nil {
		t.Fatal("auth missing")
	}
	if !current.Unavailable || !current.NextRetryAfter.After(now) {
		t.Fatalf("precondition: want live credential cooldown, got unavailable=%v next_retry_after=%v", current.Unavailable, current.NextRetryAfter)
	}
	if state := current.ModelStates[credCooldownModelM]; state == nil || state.Unavailable {
		t.Fatalf("precondition: want healthy state for %s, got %+v", credCooldownModelM, state)
	}

	if IsAuthAvailableForModel(current, credCooldownModelM, now) {
		t.Errorf("predicate: %s still available during a credential-level cooldown", credCooldownModelM)
	}
	callsBefore := executor.callCount()
	if err := credCooldownExecute(manager, credCooldownModelM); err == nil {
		t.Errorf("Execute(%s) succeeded during a credential-level cooldown, want no available credential", credCooldownModelM)
	}
	if got := executor.callCount(); got != callsBefore {
		t.Errorf("executor called %d time(s) during a credential-level cooldown, want 0", got-callsBefore)
	}

	// A success on the credential clears the cooldown again.
	manager.MarkResult(WithSkipPersist(context.Background()), Result{AuthID: authID, Provider: "codex", Success: true})
	if err := credCooldownExecute(manager, credCooldownModelM); err != nil {
		t.Fatalf("Execute(%s) after success: %v", credCooldownModelM, err)
	}
}

// A disk snapshot applied during the cooldown carries it over with the
// auth-level fields, so the credential stays unpickable.
func TestFileSnapshotKeepsModelLessCredentialCooldown(t *testing.T) {
	const authID = "cred-cooldown-snapshot"
	manager, _ := credCooldownSetup(t, authID)
	ctx := WithSkipPersist(context.Background())

	if err := credCooldownExecute(manager, credCooldownModelM); err != nil {
		t.Fatalf("seed execute %s: %v", credCooldownModelM, err)
	}
	manager.MarkResult(ctx, Result{
		AuthID:   authID,
		Provider: "codex",
		Success:  false,
		Error:    &Error{HTTPStatus: http.StatusBadGateway, Message: "bad gateway"},
	})

	path := filepath.Join(t.TempDir(), authID+".json")
	data := []byte(`{"type":"codex","access_token":"token-1"}`)
	if errWrite := os.WriteFile(path, data, 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	snapshot := &Auth{
		ID:         authID,
		Provider:   "codex",
		Status:     StatusActive,
		Attributes: map[string]string{AttributePath: path},
		Metadata:   map[string]any{"type": "codex", "access_token": "token-1"},
	}
	snapshot.BindFileSnapshot(path, data)
	if _, errUpdate := manager.Update(ctx, snapshot); errUpdate != nil {
		t.Fatalf("snapshot rejected: %v", errUpdate)
	}

	now := time.Now()
	after, _ := manager.GetByID(authID)
	if IsAuthAvailableForModel(after, credCooldownModelM, now) {
		t.Fatalf("%s available after a snapshot during a credential-level cooldown (unavailable=%v next_retry_after=%v)", credCooldownModelM, after.Unavailable, after.NextRetryAfter)
	}
}

// Re-registration after an auth/grant failure sets auth.Unavailable from the
// aggregate of the model states when one model is quota-cooled. That must not
// block the healthy models on the credential.
func TestReRegistrationModelQuotaCooldownDoesNotBlockHealthyModels(t *testing.T) {
	const authID = "cred-cooldown-reregister"
	manager, executor := credCooldownSetup(t, authID)

	executor.errs[credCooldownModelM] = credCooldownStatusError{status: http.StatusTooManyRequests, retryAfter: 10 * time.Minute}
	if err := credCooldownExecute(manager, credCooldownModelM); err == nil {
		t.Fatalf("seed execute %s: want 429", credCooldownModelM)
	}
	delete(executor.errs, credCooldownModelM)
	if err := credCooldownExecute(manager, credCooldownModelN); err != nil {
		t.Fatalf("seed execute %s: %v", credCooldownModelN, err)
	}

	current, ok := manager.GetByID(authID)
	if !ok || current == nil {
		t.Fatal("auth missing")
	}
	updated := current.Clone()
	updated.Metadata["access_token"] = "token-2"
	updated.LastError = &Error{Code: "invalid_grant", HTTPStatus: http.StatusBadRequest, Message: "invalid_grant"}
	if _, errUpdate := manager.Update(WithSkipPersist(context.Background()), updated); errUpdate != nil {
		t.Fatalf("update: %v", errUpdate)
	}

	now := time.Now()
	after, _ := manager.GetByID(authID)
	if !after.Unavailable || !after.NextRetryAfter.After(now) {
		t.Fatalf("precondition: want aggregate Unavailable from %s's quota after re-registration, got unavailable=%v next_retry_after=%v", credCooldownModelM, after.Unavailable, after.NextRetryAfter)
	}
	if IsAuthAvailableForModel(after, credCooldownModelM, now) {
		t.Errorf("predicate: quota-cooled %s became available", credCooldownModelM)
	}
	if !IsAuthAvailableForModel(after, credCooldownModelN, now) {
		t.Errorf("predicate: %s's quota cooldown blocks healthy %s", credCooldownModelM, credCooldownModelN)
	}
	if err := credCooldownExecute(manager, credCooldownModelN); err != nil {
		t.Errorf("Execute(%s): %v, want the healthy model to stay pickable", credCooldownModelN, err)
	}
}

// A model-less 401 records a credential cooldown. Re-registration with a fresh
// token supersedes it; the quota cooldown that re-registration keeps for one
// model must not carry the 401's credential cooldown over to healthy models.
func TestReRegistrationAfterModelLess401DoesNotBlockHealthyModels(t *testing.T) {
	const authID = "cred-cooldown-reregister-401"
	manager, executor := credCooldownSetup(t, authID)

	executor.errs[credCooldownModelM] = credCooldownStatusError{status: http.StatusTooManyRequests, retryAfter: 10 * time.Minute}
	if err := credCooldownExecute(manager, credCooldownModelM); err == nil {
		t.Fatalf("seed execute %s: want 429", credCooldownModelM)
	}
	delete(executor.errs, credCooldownModelM)
	if err := credCooldownExecute(manager, credCooldownModelN); err != nil {
		t.Fatalf("seed execute %s: %v", credCooldownModelN, err)
	}

	manager.MarkResult(WithSkipPersist(context.Background()), Result{
		AuthID:   authID,
		Provider: "codex",
		Success:  false,
		Error:    &Error{Code: "unauthorized", HTTPStatus: http.StatusUnauthorized, Message: "unauthorized"},
	})
	current, ok := manager.GetByID(authID)
	if !ok || current == nil {
		t.Fatal("auth missing")
	}
	if IsAuthAvailableForModel(current, credCooldownModelN, time.Now()) {
		t.Fatalf("precondition: %s available right after a model-less 401", credCooldownModelN)
	}

	updated := current.Clone()
	updated.Metadata["access_token"] = "token-2"
	if _, errUpdate := manager.Update(WithSkipPersist(context.Background()), updated); errUpdate != nil {
		t.Fatalf("update: %v", errUpdate)
	}

	now := time.Now()
	after, _ := manager.GetByID(authID)
	if IsAuthAvailableForModel(after, credCooldownModelM, now) {
		t.Errorf("predicate: quota-cooled %s became available", credCooldownModelM)
	}
	if !IsAuthAvailableForModel(after, credCooldownModelN, now) {
		t.Errorf("predicate: healthy %s still blocked by the superseded 401 cooldown", credCooldownModelN)
	}
	if err := credCooldownExecute(manager, credCooldownModelN); err != nil {
		t.Errorf("Execute(%s): %v, want the healthy model to stay pickable", credCooldownModelN, err)
	}
}

func credCooldownMark(manager *Manager, authID, model string, status int, message string, retryAfter time.Duration, seq uint64) {
	result := Result{AuthID: authID, Provider: "codex", Model: model, Success: status == 0, ExecutionSeq: seq}
	if status != 0 {
		result.Error = &Error{HTTPStatus: status, Message: message}
		if retryAfter > 0 {
			result.RetryAfter = &retryAfter
		}
	}
	manager.MarkResult(WithSkipPersist(context.Background()), result)
}

func credCooldownAuth(t *testing.T, manager *Manager, authID string) *Auth {
	t.Helper()
	auth, ok := manager.GetByID(authID)
	if !ok || auth == nil {
		t.Fatal("auth missing")
	}
	return auth
}

// The credential cooldown lasts as long as the model-less failure says, not as
// long as another model's quota window that the auth-level fields aggregate.
func TestModelLessFailureCooldownIgnoresModelAggregate(t *testing.T) {
	t.Run("429 hint while M is quota-cooled and N healthy", func(t *testing.T) {
		const authID = "cred-cooldown-own-429"
		manager, _ := credCooldownSetup(t, authID)
		credCooldownMark(manager, authID, credCooldownModelM, http.StatusTooManyRequests, "quota", 10*time.Minute, 0)
		credCooldownMark(manager, authID, credCooldownModelN, 0, "", 0, 0)
		credCooldownMark(manager, authID, "", http.StatusTooManyRequests, "rate limited", 15*time.Second, 0)

		auth := credCooldownAuth(t, manager, authID)
		now := time.Now()
		if IsAuthAvailableForModel(auth, credCooldownModelN, now) {
			t.Fatalf("%s available right after the model-less 429", credCooldownModelN)
		}
		if !IsAuthAvailableForModel(auth, credCooldownModelN, now.Add(2*time.Minute)) {
			t.Errorf("%s still blocked 2m after a 15s model-less 429: %s's 10m quota leaked into the credential cooldown", credCooldownModelN, credCooldownModelM)
		}
		if IsAuthAvailableForModel(auth, credCooldownModelM, now.Add(2*time.Minute)) {
			t.Errorf("%s's own quota cooldown was lost", credCooldownModelM)
		}
	})
	t.Run("502 while only M has a state (quota-cooled)", func(t *testing.T) {
		const authID = "cred-cooldown-own-502"
		manager, _ := credCooldownSetup(t, authID)
		credCooldownMark(manager, authID, credCooldownModelM, http.StatusTooManyRequests, "quota", 10*time.Minute, 0)
		credCooldownMark(manager, authID, "", http.StatusBadGateway, "bad gateway", 0, 0)

		auth := credCooldownAuth(t, manager, authID)
		now := time.Now()
		if IsAuthAvailableForModel(auth, credCooldownModelN, now) {
			t.Fatalf("%s available right after the model-less 502", credCooldownModelN)
		}
		if !IsAuthAvailableForModel(auth, credCooldownModelN, now.Add(5*time.Minute)) {
			t.Errorf("%s still blocked 5m after a 1m model-less 502", credCooldownModelN)
		}
		if blocked, reason, _ := isAuthBlockedForModel(auth, credCooldownModelM, now); !blocked || reason != blockReasonCooldown {
			t.Errorf("%s: blocked=%v reason=%v, want its own quota cooldown", credCooldownModelM, blocked, reason)
		}
	})
}

// A model-scoped result from an execution dispatched before the credential
// cooldown proves nothing about recovery and must not end it.
func TestInFlightModelResultKeepsCredentialCooldown(t *testing.T) {
	for _, success := range []bool{true, false} {
		name := "failure"
		if success {
			name = "success"
		}
		t.Run(name, func(t *testing.T) {
			authID := "cred-cooldown-inflight-" + name
			manager, _ := credCooldownSetup(t, authID)
			credCooldownMark(manager, authID, credCooldownModelM, 0, "", 0, 0)
			credCooldownMark(manager, authID, credCooldownModelN, 0, "", 0, 0)
			inFlightSeq := manager.nextExecutionSeq()
			credCooldownMark(manager, authID, "", http.StatusTooManyRequests, "quota", 10*time.Minute, manager.nextExecutionSeq())

			status := 0
			if !success {
				status = http.StatusBadGateway
			}
			credCooldownMark(manager, authID, credCooldownModelM, status, "bad gateway", 0, inFlightSeq)
			auth := credCooldownAuth(t, manager, authID)
			if IsAuthAvailableForModel(auth, credCooldownModelN, time.Now()) {
				t.Errorf("in-flight %s on %s ended the credential cooldown", name, credCooldownModelM)
			}
		})
	}
}

// An in-flight success keeps the credential's last error too, so the
// session-affinity rebind predicate still sees the credential cooldown.
func TestInFlightModelSuccessKeepsCredentialCooldownError(t *testing.T) {
	const authID = "cred-cooldown-inflight-403"
	manager, _ := credCooldownSetup(t, authID)
	credCooldownMark(manager, authID, credCooldownModelM, 0, "", 0, 0)
	credCooldownMark(manager, authID, credCooldownModelN, 0, "", 0, 0)
	inFlightSeq := manager.nextExecutionSeq()
	credCooldownMark(manager, authID, "", http.StatusForbidden, "forbidden", 0, manager.nextExecutionSeq())
	credCooldownMark(manager, authID, credCooldownModelM, 0, "", 0, inFlightSeq)

	auth := credCooldownAuth(t, manager, authID)
	now := time.Now()
	if IsAuthAvailableForModel(auth, credCooldownModelN, now) {
		t.Fatalf("in-flight success ended the 403 credential cooldown")
	}
	if !isAuthBlockedByQuotaOrAuthForModel(auth, credCooldownModelN, now) {
		t.Errorf("rebind predicate: 403 credential cooldown lost after an in-flight success (last error %+v)", auth.LastError)
	}
}

// A model-less failure that applies no cooldown leaves the model aggregate as
// it was.
func TestModelLessFailureWithoutCooldownKeepsModelAggregate(t *testing.T) {
	const authID = "cred-cooldown-no-cooldown"
	manager, _ := credCooldownSetup(t, authID)
	prev := transientErrorCooldownSeconds.Load()
	SetTransientErrorCooldownSeconds(-1)
	t.Cleanup(func() { SetTransientErrorCooldownSeconds(int(prev)) })

	credCooldownMark(manager, authID, credCooldownModelM, http.StatusTooManyRequests, "quota", 10*time.Minute, 0)
	before := credCooldownAuth(t, manager, authID)
	credCooldownMark(manager, authID, "", http.StatusBadGateway, "bad gateway", 0, 0)
	after := credCooldownAuth(t, manager, authID)

	if after.Unavailable != before.Unavailable || !after.NextRetryAfter.Equal(before.NextRetryAfter) ||
		after.Quota.Exceeded != before.Quota.Exceeded || !after.Quota.NextRecoverAt.Equal(before.Quota.NextRecoverAt) {
		t.Errorf("aggregate changed: before unavailable=%v next=%v quota=%+v, after unavailable=%v next=%v quota=%+v",
			before.Unavailable, before.NextRetryAfter, before.Quota, after.Unavailable, after.NextRetryAfter, after.Quota)
	}
	if !IsAuthAvailableForModel(after, credCooldownModelN, time.Now()) {
		t.Errorf("%s blocked although the model-less failure applied no cooldown", credCooldownModelN)
	}
}

// The reason comes from the live quota, not from the last failure's status.
func TestCredentialCooldownReasonFollowsLiveQuota(t *testing.T) {
	t.Run("no states, cloudflare challenge", func(t *testing.T) {
		const authID = "cred-cooldown-reason-cf"
		manager, _ := credCooldownSetup(t, authID)
		credCooldownMark(manager, authID, "", http.StatusForbidden, "cloudflare challenge", 0, 0)
		auth := credCooldownAuth(t, manager, authID)
		if blocked, reason, _ := isAuthBlockedForModel(auth, credCooldownModelM, time.Now()); !blocked || reason != blockReasonCooldown {
			t.Errorf("blocked=%v reason=%v, want cooldown", blocked, reason)
		}
	})
	for _, withStates := range []bool{false, true} {
		name := "no states"
		if withStates {
			name = "healthy states"
		}
		t.Run(name+", quota 429 then 502", func(t *testing.T) {
			authID := "cred-cooldown-reason-429-502"
			if withStates {
				authID += "-states"
			}
			manager, _ := credCooldownSetup(t, authID)
			if withStates {
				credCooldownMark(manager, authID, credCooldownModelM, 0, "", 0, 0)
				credCooldownMark(manager, authID, credCooldownModelN, 0, "", 0, 0)
			}
			credCooldownMark(manager, authID, "", http.StatusTooManyRequests, "quota", 10*time.Minute, 0)
			credCooldownMark(manager, authID, "", http.StatusBadGateway, "bad gateway", 0, 0)
			auth := credCooldownAuth(t, manager, authID)
			now := time.Now()
			blocked, reason, next := isAuthBlockedForModel(auth, credCooldownModelM, now)
			if !blocked || reason != blockReasonCooldown {
				t.Errorf("blocked=%v reason=%v, want cooldown while the quota is live", blocked, reason)
			}
			if next.Before(now.Add(9 * time.Minute)) {
				t.Errorf("next=%v, want the 10m quota deadline", next.Sub(now))
			}
		})
	}
}

// Session-affinity rebinding (rebind-on=quota) and the management cooldown
// view read the credential cooldown too.
func TestCredentialCooldownRebindAndView(t *testing.T) {
	const authID = "cred-cooldown-rebind-view"
	manager, _ := credCooldownSetup(t, authID)
	credCooldownMark(manager, authID, credCooldownModelM, 0, "", 0, 0)
	credCooldownMark(manager, authID, credCooldownModelN, 0, "", 0, 0)
	credCooldownMark(manager, authID, "", http.StatusTooManyRequests, "quota", 10*time.Minute, 0)

	auth := credCooldownAuth(t, manager, authID)
	now := time.Now()
	if !isAuthBlockedByQuotaOrAuthForModel(auth, credCooldownModelM, now) {
		t.Errorf("rebind predicate: credential quota cooldown not treated as quota-blocked")
	}
	views := CooldownSnapshotForAuth(auth, now)
	if len(views) != 1 || views[0].Scope != "credential" || views[0].RetryAt.Before(now.Add(9*time.Minute)) {
		t.Errorf("cooldown views = %+v, want one credential view until the 10m deadline", views)
	}
}

// Re-registration after a model-less quota 429 and a later model-less 401
// supersedes the 401 but keeps the credential's quota cooldown.
func TestReRegistrationKeepsModelLessQuotaCooldown(t *testing.T) {
	const authID = "cred-cooldown-reregister-quota"
	manager, _ := credCooldownSetup(t, authID)
	credCooldownMark(manager, authID, credCooldownModelM, 0, "", 0, 0)
	credCooldownMark(manager, authID, credCooldownModelN, 0, "", 0, 0)
	credCooldownMark(manager, authID, "", http.StatusTooManyRequests, "quota", 10*time.Minute, 0)
	manager.MarkResult(WithSkipPersist(context.Background()), Result{
		AuthID:   authID,
		Provider: "codex",
		Success:  false,
		Error:    &Error{Code: "unauthorized", HTTPStatus: http.StatusUnauthorized, Message: "unauthorized"},
	})

	updated := credCooldownAuth(t, manager, authID).Clone()
	updated.Metadata["access_token"] = "token-2"
	if _, errUpdate := manager.Update(WithSkipPersist(context.Background()), updated); errUpdate != nil {
		t.Fatalf("update: %v", errUpdate)
	}

	auth := credCooldownAuth(t, manager, authID)
	now := time.Now()
	blocked, reason, next := isAuthBlockedForModel(auth, credCooldownModelN, now)
	if !blocked || reason != blockReasonCooldown {
		t.Fatalf("%s: blocked=%v reason=%v, want the credential quota cooldown kept", credCooldownModelN, blocked, reason)
	}
	if next.After(now.Add(11 * time.Minute)) {
		t.Errorf("%s blocked for %v, want at most the 10m quota (the superseded 401's 30m dropped)", credCooldownModelN, next.Sub(now))
	}
}

// /v1/models suspends a model with a shorter own cooldown until the
// credential cooldown ends.
func TestCredentialCooldownProjectionUsesLongerDeadline(t *testing.T) {
	now := time.Now()
	until := now.Add(10 * time.Minute)
	auth := charAuth(func(a *Auth) {
		a.Status = StatusError
		a.Unavailable = true
		a.NextRetryAfter = until
		a.credentialCooldownUntil = until
		a.ModelStates = map[string]*ModelState{charModel: charModelQuotaState(now, now.Add(time.Minute)), charOther: charHealthyState(now)}
	})
	projection := NewManager(nil, nil, nil).clientModelProjectionForAuth(auth, charModel, now)
	if !projection.Suspended || !projection.SuspendUntil.Equal(until) {
		t.Errorf("projection = %+v, want suspended until the credential cooldown %v", projection, until)
	}
}
