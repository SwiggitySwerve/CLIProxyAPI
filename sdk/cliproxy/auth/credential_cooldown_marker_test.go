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
	if !after.Unavailable || !after.NextRetryAfter.After(now) {
		t.Fatalf("precondition: want Unavailable with a live NextRetryAfter after re-registration, got unavailable=%v next_retry_after=%v", after.Unavailable, after.NextRetryAfter)
	}
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
