package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// These tests pin what selection, the session-affinity rebind check, the
// /v1/models projection, the cooldown listing and persistence report for a
// credential-scoped 429 (a Claude unified 5h/7d rejection) and for a
// model-scoped 429. They observe answers only, never the stored model states,
// so they hold whether the credential quota is copied into each model state
// or kept at the credential level alone.

const (
	scopeM = "claude-scope-m" // reports the credential-scoped 429
	scopeN = "claude-scope-n" // healthy state
	scopeP = "claude-scope-p" // own 404, 12h: outlasts the credential quota
	scopeQ = "claude-scope-q" // own model-scoped 429, 10m: inside the credential quota
	scopeR = "claude-scope-r" // registered, no state
)

var scopeModels = []string{scopeM, scopeN, scopeP, scopeQ, scopeR}

// scopeTolerance absorbs the wall-clock time between a MarkResult call and the
// test's own time.Now(); deadlines are hours away.
const scopeTolerance = 30 * time.Second

type scopeExecutor struct {
	mu    sync.Mutex
	calls map[string]int
}

func (e *scopeExecutor) Identifier() string { return "claude" }

func (e *scopeExecutor) Execute(_ context.Context, auth *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.mu.Lock()
	e.calls[req.Model]++
	e.mu.Unlock()
	return cliproxyexecutor.Response{Payload: []byte(`{"served_by":"` + auth.ID + `"}`)}, nil
}

func (e *scopeExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, &Error{HTTPStatus: http.StatusNotImplemented, Message: "not implemented"}
}

func (e *scopeExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) { return auth, nil }

func (e *scopeExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusNotImplemented, Message: "not implemented"}
}

func (e *scopeExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, &Error{HTTPStatus: http.StatusNotImplemented, Message: "not implemented"}
}

func (e *scopeExecutor) callCount(model string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls[model]
}

type scopeFixture struct {
	manager  *Manager
	executor *scopeExecutor
	id       string
}

func newScopeFixture(t *testing.T, id string) *scopeFixture {
	t.Helper()
	manager := NewManager(nil, nil, nil)
	executor := &scopeExecutor{calls: map[string]int{}}
	manager.RegisterExecutor(executor)
	registerScopeAuth(t, manager, id)
	return &scopeFixture{manager: manager, executor: executor, id: id}
}

func registerScopeAuth(t *testing.T, manager *Manager, id string) {
	t.Helper()
	infos := make([]*registry.ModelInfo, 0, len(scopeModels))
	for _, model := range scopeModels {
		infos = append(infos, &registry.ModelInfo{ID: model})
	}
	registry.GetGlobalRegistry().RegisterClient(id, "claude", infos)
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	auth := &Auth{ID: id, Provider: "claude", Status: StatusActive, Attributes: map[string]string{"api_key": "k"}}
	if _, err := manager.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("register %s: %v", id, err)
	}
}

func (f *scopeFixture) mark(result Result) {
	result.AuthID = f.id
	result.Provider = "claude"
	f.manager.MarkResult(context.Background(), result)
}

// seedSiblings gives N a healthy state, P its own 12h 404 and Q its own 10m
// model-scoped 429.
func (f *scopeFixture) seedSiblings() {
	f.mark(Result{Model: scopeN, Success: true})
	f.mark(Result{Model: scopeP, Error: &Error{HTTPStatus: http.StatusNotFound, Message: "not found"}})
	tenMinutes := 10 * time.Minute
	f.mark(Result{Model: scopeQ, RetryAfter: &tenMinutes, Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "model rate limit"}})
}

// credential429 records a Claude unified rejection on model, as the executor
// reports it: a credential-scoped 429 with the rejected unified headers.
func (f *scopeFixture) credential429(model string, retryAfter time.Duration) {
	ctx := internallogging.WithResponseHeadersHolder(context.Background())
	internallogging.SetResponseHeaders(ctx, rejectedClaudeHeaders())
	f.manager.MarkResult(ctx, Result{
		AuthID: f.id, Provider: "claude", Model: model, CredentialScope: true, RetryAfter: &retryAfter,
		Error: &Error{Code: ErrorCodeCredentialQuota, Message: "rate_limit_error", HTTPStatus: http.StatusTooManyRequests},
	})
}

func (f *scopeFixture) auth(t *testing.T) *Auth {
	t.Helper()
	auth, ok := f.manager.GetByID(f.id)
	if !ok || auth == nil {
		t.Fatalf("auth %s missing", f.id)
	}
	return auth
}

func near(got, want time.Time) bool {
	diff := got.Sub(want)
	return diff > -scopeTolerance && diff < scopeTolerance
}

// requireGate checks the availability predicate for model at at: blocked
// until want (zero means available).
func requireGate(t *testing.T, auth *Auth, model string, at, want time.Time) {
	t.Helper()
	blocked, reason, next := isAuthBlockedForModel(auth, model, at)
	if want.IsZero() {
		if blocked {
			t.Fatalf("%s at +%s: blocked (reason %v, until %v), want available", model, at.Sub(time.Now()).Round(time.Minute), reason, next)
		}
		return
	}
	if !blocked || !near(next, want) {
		t.Fatalf("%s at +%s: blocked=%v until %v, want blocked until %v", model, at.Sub(time.Now()).Round(time.Minute), blocked, next, want)
	}
}

func viewsByModel(views []CooldownView) (credential *CooldownView, models map[string]CooldownView) {
	models = map[string]CooldownView{}
	for i := range views {
		if views[i].Scope == "credential" {
			credential = &views[i]
			continue
		}
		models[views[i].ModelKey] = views[i]
	}
	return credential, models
}

func sortedKeys(m map[string]CooldownView) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func isQuotaFamilyReason(reason string) bool {
	return reason == "quota" || reason == "credential_quota"
}

func requireCooldownError(t *testing.T, err error, want time.Duration) {
	t.Helper()
	var cooldownErr *modelCooldownError
	if !errors.As(err, &cooldownErr) {
		t.Fatalf("error = %T (%v), want the cooling-down 429", err, err)
	}
	if cooldownErr.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", cooldownErr.StatusCode())
	}
	if diff := cooldownErr.resetIn - want; diff < -scopeTolerance || diff > scopeTolerance {
		t.Fatalf("resetIn = %v, want ~%v", cooldownErr.resetIn, want)
	}
}

// requireCredentialQuotaAnswers checks every observable answer while the
// credential quota (until credUntil) is live and after it ends.
func requireCredentialQuotaAnswers(t *testing.T, f *scopeFixture, start, credUntil, pUntil time.Time) {
	t.Helper()
	auth := f.auth(t)
	now := time.Now()

	// Selection: every model, including one with no state, waits for the
	// credential quota; P's own longer block extends its deadline.
	for _, model := range []string{scopeM, scopeN, scopeQ, scopeR} {
		requireGate(t, auth, model, now, credUntil)
	}
	requireGate(t, auth, scopeP, now, pUntil)
	for _, model := range scopeModels {
		if _, reason, _ := isAuthBlockedForModel(auth, model, now); reason != blockReasonCooldown {
			t.Fatalf("%s block reason = %v, want cooldown", model, reason)
		}
		if !isAuthBlockedByQuotaOrAuthForModel(auth, model, now) {
			t.Fatalf("%s: session-affinity rebind check says not blocked under the credential quota", model)
		}
		if ok, next := retryRoundAvailabilityForAuth(auth, model, now); !ok || next.IsZero() {
			t.Fatalf("%s: retry-round eligibility = %v until %v, want eligible with a deadline", model, ok, next)
		}
	}

	// /v1/models projection: suspended for a quota reason until the gate's deadline.
	for _, model := range scopeModels {
		want := credUntil
		if model == scopeP {
			want = pUntil
		}
		projection := f.manager.clientModelProjectionForAuth(auth, model, now)
		if !projection.Suspended || !isQuotaFamilyReason(projection.SuspendReason) || !near(projection.SuspendUntil, want) {
			t.Fatalf("%s projection = %+v, want suspended for a quota reason until %v", model, projection, want)
		}
		if model != scopeR && !projection.QuotaExceeded {
			t.Fatalf("%s projection QuotaExceeded = false, want true", model)
		}
	}

	// Listing: the credential view, and one model view per model state.
	credential, models := viewsByModel(CooldownSnapshotForAuth(auth, now))
	if credential == nil || credential.Reason != "credential_quota" || !near(credential.RetryAt, credUntil) {
		t.Fatalf("credential view = %+v, want credential_quota until %v", credential, credUntil)
	}
	if got := sortedKeys(models); len(got) != 4 || got[0] != scopeM || got[1] != scopeN || got[2] != scopeP || got[3] != scopeQ {
		t.Fatalf("model views for %v, want M, N, P and Q", got)
	}
	for _, model := range []string{scopeM, scopeN, scopeQ} {
		if view := models[model]; !isQuotaFamilyReason(view.Reason) || !near(view.RetryAt, credUntil) {
			t.Fatalf("%s view = %+v, want a quota reason until %v", model, view, credUntil)
		}
	}
	for _, model := range []string{scopeN, scopeQ} {
		if view := models[model]; view.Reason != "credential_quota" {
			t.Fatalf("%s view reason = %q, want credential_quota", model, view.Reason)
		}
	}
	if view := models[scopeP]; view.Reason != "not_found" || !near(view.RetryAt, pUntil) {
		t.Fatalf("P view = %+v, want not_found until %v", view, pUntil)
	}

	// The credential-level fields the management listing reads.
	if !auth.Unavailable || auth.Status != StatusError || !near(auth.NextRetryAfter, credUntil) {
		t.Fatalf("auth unavailable=%v status=%s next=%v, want unavailable error until %v", auth.Unavailable, auth.Status, auth.NextRetryAfter, credUntil)
	}
	if !auth.Quota.Exceeded || auth.Quota.Reason != "credential_quota" || !near(auth.Quota.NextRecoverAt, credUntil) {
		t.Fatalf("auth quota = %+v, want credential_quota until %v", auth.Quota, credUntil)
	}

	// The Claude cooldown headers on the 429 the client sees.
	for _, model := range []string{scopeM, scopeN, scopeQ, scopeR} {
		if cooled, signals := claudeUnifiedCooldownForAuth(auth, model, now); !cooled || len(signals) == 0 {
			t.Fatalf("%s: claude unified cooldown = %v %v, want the rejected credential signals", model, cooled, signals)
		}
	}
	if cooled, signals := claudeUnifiedCooldownForAuth(auth, scopeP, now); cooled || signals != nil {
		t.Fatalf("P: claude unified cooldown = %v %v, want none (its own 404 outlasts the quota)", cooled, signals)
	}

	// The recheck loop and the all-cooled trial.
	if !authHasQuotaCooldown(auth) {
		t.Fatal("recheck loop does not see the credential quota")
	}
	if got := soonestKnownReset(auth, now); !near(got, credUntil) {
		t.Fatalf("soonest known reset = %v, want the credential quota end %v", got, credUntil)
	}
	if windows, ok := claudeCooldownRejectedWindows(auth, now); !ok || len(windows) != 1 || windows[0] != "7d" {
		t.Fatalf("rejected windows = %v %v, want [7d]", windows, ok)
	}

	// Once the credential quota ends, only P's own block remains.
	after := credUntil.Add(time.Minute)
	for _, model := range []string{scopeM, scopeN, scopeQ, scopeR} {
		requireGate(t, auth, model, after, time.Time{})
		if projection := f.manager.clientModelProjectionForAuth(auth, model, after); projection.Suspended {
			t.Fatalf("%s projection after the quota = %+v, want not suspended", model, projection)
		}
	}
	requireGate(t, auth, scopeP, after, pUntil)
	credential, models = viewsByModel(CooldownSnapshotForAuth(auth, after))
	if credential != nil || len(models) != 1 || !near(models[scopeP].RetryAt, pUntil) {
		t.Fatalf("views after the quota: credential=%+v models=%+v, want only P until %v", credential, models, pUntil)
	}
	_ = start
}

func TestScopeCharCredential429BlocksEveryModel(t *testing.T) {
	f := newScopeFixture(t, "scope-cred-429")
	start := time.Now()
	f.seedSiblings()
	f.credential429(scopeM, 5*time.Hour)

	requireCredentialQuotaAnswers(t, f, start, start.Add(5*time.Hour), start.Add(12*time.Hour))

	// Execute: no model is dispatched; the client gets the cooling-down 429.
	for _, model := range scopeModels {
		_, err := f.manager.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
		want := 5 * time.Hour
		if model == scopeP {
			want = 12 * time.Hour
		}
		requireCooldownError(t, err, want)
		if calls := f.executor.callCount(model); calls != 0 {
			t.Fatalf("%s dispatched %d times under the credential quota", model, calls)
		}
	}
}

func TestScopeCharModelScoped429BlocksOnlyThatModel(t *testing.T) {
	f := newScopeFixture(t, "scope-model-429")
	start := time.Now()
	f.mark(Result{Model: scopeN, Success: true})
	tenMinutes := 10 * time.Minute
	f.mark(Result{Model: scopeM, RetryAfter: &tenMinutes, Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "model rate limit"}})

	auth := f.auth(t)
	now := time.Now()
	requireGate(t, auth, scopeM, now, start.Add(10*time.Minute))
	for _, model := range []string{scopeN, scopeR} {
		requireGate(t, auth, model, now, time.Time{})
		if isAuthBlockedByQuotaOrAuthForModel(auth, model, now) {
			t.Fatalf("%s: rebind check blocked by another model's 429", model)
		}
	}
	if !isAuthBlockedByQuotaOrAuthForModel(auth, scopeM, now) {
		t.Fatal("M: rebind check not blocked by its own 429")
	}
	credential, models := viewsByModel(CooldownSnapshotForAuth(auth, now))
	if credential != nil || len(models) != 1 || models[scopeM].Reason != "quota" {
		t.Fatalf("views = %+v %+v, want only M's quota view", credential, models)
	}
	_, err := f.manager.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: scopeM}, cliproxyexecutor.Options{})
	requireCooldownError(t, err, 10*time.Minute)
	if _, err = f.manager.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: scopeN}, cliproxyexecutor.Options{}); err != nil {
		t.Fatalf("N not served: %v", err)
	}
	if f.executor.callCount(scopeN) != 1 || f.executor.callCount(scopeM) != 0 {
		t.Fatalf("calls M=%d N=%d, want 0 and 1", f.executor.callCount(scopeM), f.executor.callCount(scopeN))
	}
}

func TestScopeCharLaterResultsNeverShortenTheCredentialQuota(t *testing.T) {
	f := newScopeFixture(t, "scope-later-results")
	start := time.Now()
	f.seedSiblings()
	f.credential429(scopeM, 5*time.Hour)
	credUntil := start.Add(5 * time.Hour)

	// In-flight results from requests dispatched before the 429.
	f.mark(Result{Model: scopeN, Success: true})
	f.mark(Result{Model: scopeQ, Success: true})
	f.mark(Result{Model: scopeN, Error: &Error{HTTPStatus: http.StatusBadGateway, Message: "bad gateway"}})
	auth := f.auth(t)
	for _, model := range []string{scopeM, scopeN, scopeQ, scopeR} {
		requireGate(t, auth, model, time.Now(), credUntil)
	}
	requireGate(t, auth, scopeN, credUntil.Add(time.Minute), time.Time{})

	// A shorter credential 429 on another model keeps the deadline.
	f.credential429(scopeN, time.Hour)
	auth = f.auth(t)
	for _, model := range scopeModels[:2] {
		requireGate(t, auth, model, time.Now(), credUntil)
	}
	if !near(auth.Quota.NextRecoverAt, credUntil) {
		t.Fatalf("credential quota shortened to %v", auth.Quota.NextRecoverAt)
	}

	// A model failure that outlasts the quota keeps its own deadline.
	f.mark(Result{Model: scopeR, Error: &Error{HTTPStatus: http.StatusNotFound, Message: "not found"}})
	auth = f.auth(t)
	requireGate(t, auth, scopeR, credUntil.Add(time.Minute), start.Add(12*time.Hour))

	// A longer credential 429 extends every model.
	f.credential429(scopeQ, 10*time.Hour)
	auth = f.auth(t)
	for _, model := range []string{scopeM, scopeN, scopeQ} {
		requireGate(t, auth, model, time.Now(), start.Add(10*time.Hour))
	}
	requireGate(t, auth, scopeP, time.Now(), start.Add(12*time.Hour))
}

func TestScopeCharPersistenceRoundTrip(t *testing.T) {
	f := newScopeFixture(t, "scope-persist")
	start := time.Now()
	f.seedSiblings()
	f.credential429(scopeM, 5*time.Hour)
	records := f.manager.cooldownStateRecordsSnapshot()
	if os.Getenv("SCOPE_CHAR_WRITE_LEGACY") != "" {
		writeScopeLegacyFixture(t, start, records)
	}

	restored := NewManager(nil, nil, nil)
	registerScopeAuth(t, restored, "scope-persist")
	restored.SetCooldownStateStore(&mockCooldownStateStore{records: records})
	if err := restored.RestoreCooldownStates(context.Background()); err != nil {
		t.Fatalf("restore: %v", err)
	}
	requireRestoredAnswers(t, restored, "scope-persist", start)
}

// requireRestoredAnswers checks the answers after a restart: the gate, the
// credential view and the credential-level fields are as before the restart.
func requireRestoredAnswers(t *testing.T, manager *Manager, id string, start time.Time) {
	t.Helper()
	auth, _ := manager.GetByID(id)
	now := time.Now()
	credUntil := start.Add(5 * time.Hour)
	pUntil := start.Add(12 * time.Hour)
	for _, model := range []string{scopeM, scopeN, scopeQ, scopeR} {
		requireGate(t, auth, model, now, credUntil)
	}
	requireGate(t, auth, scopeP, now, pUntil)
	for _, model := range scopeModels {
		if !isAuthBlockedByQuotaOrAuthForModel(auth, model, now) {
			t.Fatalf("%s: rebind check not blocked after restore", model)
		}
	}
	credential, models := viewsByModel(CooldownSnapshotForAuth(auth, now))
	if credential == nil || credential.Reason != "credential_quota" || !near(credential.RetryAt, credUntil) {
		t.Fatalf("restored credential view = %+v", credential)
	}
	if view, ok := models[scopeP]; !ok || !near(view.RetryAt, pUntil) {
		t.Fatalf("restored P view = %+v, want until %v", view, pUntil)
	}
	if !auth.Unavailable || !near(auth.NextRetryAfter, credUntil) || auth.Quota.Reason != "credential_quota" {
		t.Fatalf("restored auth fields unavailable=%v next=%v quota=%+v", auth.Unavailable, auth.NextRetryAfter, auth.Quota)
	}
	after := credUntil.Add(time.Minute)
	for _, model := range []string{scopeM, scopeN, scopeQ, scopeR} {
		requireGate(t, auth, model, after, time.Time{})
	}
	requireGate(t, auth, scopeP, after, pUntil)
}

// scopeLegacyFixture holds cooldown records written by the build that copied
// the credential quota into every model state, with times stored relative to
// the moment of the 429.
type scopeLegacyFixture struct {
	Start   time.Time             `json:"start"`
	Records []CooldownStateRecord `json:"records"`
}

const scopeLegacyFixturePath = "testdata/cooldown/legacy-credential-quota-propagation.json"

func writeScopeLegacyFixture(t *testing.T, start time.Time, records []CooldownStateRecord) {
	t.Helper()
	data, err := json.MarshalIndent(scopeLegacyFixture{Start: start, Records: records}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Dir(scopeLegacyFixturePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(scopeLegacyFixturePath, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func shiftTime(value *time.Time, delta time.Duration) {
	if !value.IsZero() {
		*value = value.Add(delta)
	}
}

func shiftRecordTimes(record *CooldownStateRecord, delta time.Duration) {
	shiftTime(&record.NextRetryAfter, delta)
	shiftTime(&record.UpdatedAt, delta)
	shiftTime(&record.Quota.NextRecoverAt, delta)
	if record.IndependentState != nil {
		shiftTime(&record.IndependentState.NextRetryAfter, delta)
		shiftTime(&record.IndependentState.UpdatedAt, delta)
		shiftTime(&record.IndependentState.Quota.NextRecoverAt, delta)
	}
}

// TestScopeCharLegacyRecordsRestore restores the .cds records an older build
// wrote for the same scenario, as a deployment restart would.
func TestScopeCharLegacyRecordsRestore(t *testing.T) {
	data, err := os.ReadFile(scopeLegacyFixturePath)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var fixture scopeLegacyFixture
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	delta := start.Sub(fixture.Start)
	for i := range fixture.Records {
		shiftRecordTimes(&fixture.Records[i], delta)
	}
	manager := NewManager(nil, nil, nil)
	registerScopeAuth(t, manager, "scope-persist")
	manager.SetCooldownStateStore(&mockCooldownStateStore{records: fixture.Records})
	if err = manager.RestoreCooldownStates(context.Background()); err != nil {
		t.Fatalf("restore: %v", err)
	}
	requireRestoredAnswers(t, manager, "scope-persist", start)
	auth, _ := manager.GetByID("scope-persist")
	requireGate(t, auth, scopeQ, start.Add(5*time.Hour).Add(time.Minute), time.Time{})
}

func TestScopeCharRecheckGuardedClear(t *testing.T) {
	f := newScopeFixture(t, "scope-recheck")
	f.seedSiblings()
	f.credential429(scopeM, 5*time.Hour)

	observed := newQuotaCooldownIdentity(f.auth(t))
	guard := func(current *Auth) bool { return newQuotaCooldownIdentity(current) == observed }

	// A new rejection during the probe keeps the cooldown.
	f.credential429(scopeN, 6*time.Hour)
	if _, _, err := f.manager.resetQuota(context.Background(), f.id, guard); !errors.Is(err, errQuotaCooldownChanged) {
		t.Fatalf("guarded clear after a new rejection: err = %v, want errQuotaCooldownChanged", err)
	}
	for _, model := range scopeModels {
		if blocked, _, _ := isAuthBlockedForModel(f.auth(t), model, time.Now()); !blocked {
			t.Fatalf("%s released by a stale recheck", model)
		}
	}

	// An unchanged cooldown is cleared for every model.
	observed = newQuotaCooldownIdentity(f.auth(t))
	if _, _, err := f.manager.resetQuota(context.Background(), f.id, guard); err != nil {
		t.Fatalf("guarded clear: %v", err)
	}
	auth := f.auth(t)
	for _, model := range scopeModels {
		requireGate(t, auth, model, time.Now(), time.Time{})
		if isAuthBlockedByQuotaOrAuthForModel(auth, model, time.Now()) {
			t.Fatalf("%s still rebind-blocked after the clear", model)
		}
	}
	if views := CooldownSnapshotForAuth(auth, time.Now()); len(views) != 0 {
		t.Fatalf("views after the clear = %+v", views)
	}
	if authHasQuotaCooldown(auth) {
		t.Fatal("recheck loop still sees a quota cooldown after the clear")
	}
}

// TestScopeCharAllCooledTrialOnSiblingModel cools the credential through M and
// asks for N: the all-cooled half-open trial serves N on the lifted
// credential and the clear releases every model.
func TestScopeCharAllCooledTrialOnSiblingModel(t *testing.T) {
	f := newAllCooledFixture(t, nil, internalconfig.QuotaReconcileClear, "scope-trial")
	sibling := f.model + "-sibling"
	registry.GetGlobalRegistry().RegisterClient("scope-trial", "claude", []*registry.ModelInfo{{ID: f.model}, {ID: sibling}})
	f.manager.MarkResult(context.Background(), Result{AuthID: "scope-trial", Provider: "claude", Model: sibling, Success: true})
	f.cool(t, "scope-trial")
	auth, _ := f.manager.GetByID("scope-trial")
	if blocked, _, _ := isAuthBlockedForModel(auth, sibling, time.Now()); !blocked {
		t.Fatal("sibling not blocked by the credential quota")
	}
	f.executor.lift("scope-trial")

	resp, err := f.manager.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: sibling}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("Execute(sibling) = %v, want served by the half-open trial", err)
	}
	if string(resp.Payload) != `{"served_by":"scope-trial"}` {
		t.Fatalf("payload = %s", resp.Payload)
	}
	auth, _ = f.manager.GetByID("scope-trial")
	for _, model := range []string{f.model, sibling} {
		requireGate(t, auth, model, time.Now(), time.Time{})
	}
	if f.cooled("scope-trial") {
		t.Fatal("credential still cooled after the trial was served with allowed headers")
	}
}

// TestScopeCharObservationRestoreUnderCredentialQuota restores a passive
// quota observation for a model with no state under a live credential quota:
// the empty state it creates does not release that model.
func TestScopeCharObservationRestoreUnderCredentialQuota(t *testing.T) {
	f := newScopeFixture(t, "scope-observation")
	start := time.Now()
	f.mark(Result{Model: scopeN, Success: true})
	f.credential429(scopeM, 5*time.Hour)
	records := f.manager.cooldownStateRecordsSnapshot()
	records = append(records, CooldownStateRecord{
		Provider: "claude", AuthID: f.id, Model: scopeR,
		Observation: &QuotaObservationRecord{ObservedAt: start, Signals: map[string]string{"Anthropic-Ratelimit-Unified-5h-Utilization": "0.5"}},
	})

	restored := NewManager(nil, nil, nil)
	registerScopeAuth(t, restored, f.id)
	restored.SetCooldownStateStore(&mockCooldownStateStore{records: records})
	if err := restored.RestoreCooldownStates(context.Background()); err != nil {
		t.Fatalf("restore: %v", err)
	}
	auth, _ := restored.GetByID(f.id)
	if existingModelState(auth, scopeR) == nil {
		t.Fatal("observation restore did not create R's state; the scenario needs it")
	}
	for _, model := range scopeModels {
		requireGate(t, auth, model, time.Now(), start.Add(5*time.Hour))
	}
}
