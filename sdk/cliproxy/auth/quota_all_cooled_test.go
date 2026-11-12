package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// allCooledQuotaError is the error a Claude executor returns for a unified
// 5h/7d rejection: a credential-scoped 429 with a long Retry-After.
type allCooledQuotaError struct{ retryAfter time.Duration }

func (e *allCooledQuotaError) Error() string              { return "rate_limit_error: 7d limit reached" }
func (e *allCooledQuotaError) StatusCode() int            { return http.StatusTooManyRequests }
func (e *allCooledQuotaError) RetryAfter() *time.Duration { d := e.retryAfter; return &d }
func (e *allCooledQuotaError) IsCredentialScoped() bool   { return true }

func rejectedClaudeHeaders() http.Header {
	reset := strconv.FormatInt(time.Now().Add(100*time.Hour).Unix(), 10)
	return http.Header{
		"Anthropic-Ratelimit-Unified-Status":    {"rejected"},
		"Anthropic-Ratelimit-Unified-7d-Status": {"rejected"},
		"Anthropic-Ratelimit-Unified-7d-Reset":  {reset},
		"Anthropic-Ratelimit-Unified-Reset":     {reset},
	}
}

func allowedClaudeHeaders() http.Header {
	return http.Header{
		"Anthropic-Ratelimit-Unified-Status":    {"allowed"},
		"Anthropic-Ratelimit-Unified-7d-Status": {"allowed"},
	}
}

// allCooledExecutor is a fake Claude upstream. A credential in lifted answers
// 200 with allowed headers; any other credential answers a quota 429.
type allCooledExecutor struct {
	provider string
	mu       sync.Mutex
	lifted   map[string]bool
	calls    map[string]int
	// served overrides the headers of a lifted credential's 200.
	served http.Header
	// transient counts upcoming non-quota failures (a dropped connection) per
	// credential; each Execute call on it consumes one.
	transient map[string]int
}

func newAllCooledExecutor(provider string) *allCooledExecutor {
	return &allCooledExecutor{provider: provider, lifted: map[string]bool{}, calls: map[string]int{}}
}

func (e *allCooledExecutor) Identifier() string { return e.provider }

func (e *allCooledExecutor) attempt(ctx context.Context, id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls[id]++
	if e.lifted[id] {
		internallogging.SetResponseHeaders(ctx, e.servedHeadersLocked())
		return true
	}
	internallogging.SetResponseHeaders(ctx, rejectedClaudeHeaders())
	return false
}

func (e *allCooledExecutor) servedHeadersLocked() http.Header {
	if e.served != nil {
		return e.served.Clone()
	}
	return allowedClaudeHeaders()
}

func (e *allCooledExecutor) servedHeaders() http.Header {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.servedHeadersLocked()
}

func (e *allCooledExecutor) lift(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lifted[id] = true
}

func (e *allCooledExecutor) callCount(id string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls[id]
}

func (e *allCooledExecutor) takeTransient(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.transient[id] == 0 {
		return false
	}
	e.transient[id]--
	e.calls[id]++
	return true
}

func (e *allCooledExecutor) Execute(ctx context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if e.takeTransient(auth.ID) {
		return cliproxyexecutor.Response{}, fmt.Errorf("claude tls: handshake upstream: read: %w", syscall.ECONNRESET)
	}
	if e.attempt(ctx, auth.ID) {
		return cliproxyexecutor.Response{Payload: []byte(`{"served_by":"` + auth.ID + `"}`), Headers: e.servedHeaders()}, nil
	}
	return cliproxyexecutor.Response{}, &allCooledQuotaError{retryAfter: 100 * time.Hour}
}

// ExecuteStream reports a rejection inside the stream, before any payload,
// which is the harder case: the HTTP call itself returned a stream.
func (e *allCooledExecutor) ExecuteStream(ctx context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	ch := make(chan cliproxyexecutor.StreamChunk, 2)
	if e.attempt(ctx, auth.ID) {
		ch <- cliproxyexecutor.StreamChunk{Payload: []byte("data: served_by " + auth.ID + "\n\n")}
		close(ch)
		return &cliproxyexecutor.StreamResult{Headers: e.servedHeaders(), Chunks: ch}, nil
	}
	ch <- cliproxyexecutor.StreamChunk{Err: &allCooledQuotaError{retryAfter: 100 * time.Hour}}
	close(ch)
	return &cliproxyexecutor.StreamResult{Headers: rejectedClaudeHeaders(), Chunks: ch}, nil
}

func (e *allCooledExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) { return auth, nil }

func (e *allCooledExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusNotImplemented, Message: "not implemented"}
}

func (e *allCooledExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, &Error{HTTPStatus: http.StatusNotImplemented, Message: "not implemented"}
}

type allCooledFixture struct {
	manager  *Manager
	executor *allCooledExecutor
	clock    *fakeClock
	model    string
	provider string
}

// newAllCooledFixture registers Claude credentials ids for one model, all
// healthy, with quota-reconcile set to mode for claude.
func newAllCooledFixture(t *testing.T, selector Selector, mode internalconfig.QuotaReconcileMode, ids ...string) *allCooledFixture {
	t.Helper()
	return newAllCooledFixtureFor(t, "claude", selector, mode, ids...)
}

func newAllCooledFixtureFor(t *testing.T, provider string, selector Selector, mode internalconfig.QuotaReconcileMode, ids ...string) *allCooledFixture {
	t.Helper()
	manager := NewManager(nil, selector, nil)
	manager.SetConfig(&internalconfig.Config{QuotaReconcile: internalconfig.QuotaReconcileConfig{provider: {Mode: mode}}})
	// request-retry 2, as on the operator's Claude instance: up to three trials.
	manager.SetRetryConfig(2, 0, 0)
	executor := newAllCooledExecutor(provider)
	manager.RegisterExecutor(executor)
	clock := &fakeClock{t: time.Now()}
	manager.quotaReconcile.schedule.clock = clock.now
	model := "all-cooled-" + t.Name()
	for _, id := range ids {
		if _, err := manager.Register(WithSkipPersist(context.Background()), &Auth{ID: id, Provider: provider, Status: StatusActive}); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
		registry.GetGlobalRegistry().RegisterClient(id, provider, []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	return &allCooledFixture{manager: manager, executor: executor, clock: clock, model: model, provider: provider}
}

// cool records a 7d credential-quota rejection for id, exactly as a real 429
// from a request would.
func (f *allCooledFixture) cool(t *testing.T, id string) {
	t.Helper()
	ctx := internallogging.WithResponseHeadersHolder(context.Background())
	internallogging.SetResponseHeaders(ctx, rejectedClaudeHeaders())
	retry := 100 * time.Hour
	f.manager.MarkResult(ctx, Result{
		AuthID: id, Provider: f.provider, Model: f.model, Success: false, CredentialScope: true, RetryAfter: &retry,
		Error: &Error{Code: ErrorCodeCredentialQuota, Message: "rate_limit_error", HTTPStatus: http.StatusTooManyRequests},
	})
	if got, _ := f.manager.GetByID(id); got == nil || !got.Quota.Exceeded {
		t.Fatalf("%s is not in a quota cooldown after the 429", id)
	}
}

func (f *allCooledFixture) cooled(id string) bool {
	got, _ := f.manager.GetByID(id)
	return got != nil && authHasQuotaCooldown(got)
}

func (f *allCooledFixture) execute(opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return f.manager.Execute(context.Background(), []string{f.provider}, cliproxyexecutor.Request{Model: f.model}, opts)
}

func requireModelCooldownError(t *testing.T, err error) {
	t.Helper()
	var cooldownErr *modelCooldownError
	if !errors.As(err, &cooldownErr) {
		t.Fatalf("error = %T (%v), want the all-credentials-cooling-down 429", err, err)
	}
	if cooldownErr.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", cooldownErr.StatusCode())
	}
}

func TestAllCooledTrialServesLiftedCredentialAndClearsCooldown(t *testing.T) {
	f := newAllCooledFixture(t, nil, internalconfig.QuotaReconcileClear, "claude-a", "claude-b")
	f.cool(t, "claude-a")
	f.cool(t, "claude-b")
	f.executor.lift("claude-b")

	resp, err := f.execute(cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("Execute error = %v, want the request served by the lifted credential", err)
	}
	if string(resp.Payload) != `{"served_by":"claude-b"}` {
		t.Fatalf("payload = %s, want served by claude-b", resp.Payload)
	}
	if f.cooled("claude-b") {
		t.Fatal("claude-b still cooled after a 200 with allowed headers")
	}
	if !f.cooled("claude-a") {
		t.Fatal("claude-a cooldown was cleared without an allowed response")
	}
	for _, id := range []string{"claude-a", "claude-b"} {
		if calls := f.executor.callCount(id); calls > 1 {
			t.Fatalf("%s tried %d times, want at most once", id, calls)
		}
	}

	// The cleared credential now serves through normal selection; the cooled
	// one is not trialled while a healthy candidate exists.
	callsA := f.executor.callCount("claude-a")
	if _, err = f.execute(cliproxyexecutor.Options{}); err != nil {
		t.Fatalf("second Execute error = %v", err)
	}
	if got := f.executor.callCount("claude-a"); got != callsA {
		t.Fatalf("claude-a trialled while claude-b was healthy (calls %d -> %d)", callsA, got)
	}
}

// A 200 is not enough to clear: overage can serve a request while the window
// that set the cooldown stays rejected, and a response that omits that window
// says nothing about it.
func TestAllCooledTrialKeepsCooldownUnlessCooldownWindowAllowed(t *testing.T) {
	cases := map[string]http.Header{
		"7d still rejected": {
			"Anthropic-Ratelimit-Unified-Status":    {"allowed"},
			"Anthropic-Ratelimit-Unified-7d-Status": {"rejected"},
		},
		"7d missing": {
			"Anthropic-Ratelimit-Unified-Status": {"allowed"},
		},
	}
	for name, served := range cases {
		t.Run(name, func(t *testing.T) {
			f := newAllCooledFixture(t, nil, internalconfig.QuotaReconcileClear, "claude-a")
			f.cool(t, "claude-a")
			f.executor.served = served
			f.executor.lift("claude-a")

			if _, err := f.execute(cliproxyexecutor.Options{}); err != nil {
				t.Fatalf("Execute error = %v, want the trial served", err)
			}
			if !f.cooled("claude-a") {
				t.Fatal("cooldown cleared although the window that set it was not reported allowed")
			}
		})
	}
}

// A clear by a half-open trial counts as a check: a quota 429 soon after it
// backs off the next recheck, as after a clear by the loop.
func TestAllCooledTrialClearFeedsFlapBackoff(t *testing.T) {
	f := newAllCooledFixture(t, nil, internalconfig.QuotaReconcileClear, "claude-a")
	f.cool(t, "claude-a")
	f.executor.lift("claude-a")
	if _, err := f.execute(cliproxyexecutor.Options{}); err != nil {
		t.Fatalf("Execute error = %v, want the trial served", err)
	}
	if f.cooled("claude-a") {
		t.Fatal("setup: trial did not clear the cooldown")
	}

	f.clock.advance(time.Minute)
	f.cool(t, "claude-a")

	schedule := &f.manager.quotaReconcile.schedule
	schedule.mu.Lock()
	st := schedule.states["claude-a"]
	schedule.mu.Unlock()
	if st == nil || st.backoffLevel != 1 {
		t.Fatalf("backoff level after a flap = %+v, want 1", st)
	}
	if want := st.probedAt.Add(backedOffInterval(st.baseInterval, 1)); !st.nextCheck.Equal(want) || st.baseInterval <= 0 {
		t.Fatalf("next check = %v (base %v), want %v from a booked trial", st.nextCheck, st.baseInterval, want)
	}
}

// A healthy credential that failed this request for a non-quota reason (the
// 2026-11-12 08:43 case: a TLS reset) means the pool is not all cooled. No
// half-open trial runs on the cooled credentials; the request retries on the
// healthy one instead.
func TestAllCooledTrialSkippedWhenTriedCredentialIsHealthy(t *testing.T) {
	f := newAllCooledFixture(t, nil, internalconfig.QuotaReconcileClear, "claude-a", "claude-b")
	// As on the operator's Claude instance: request-retry 2, max-retry-interval 30s.
	f.manager.SetRetryConfig(2, 30*time.Second, 0)
	f.cool(t, "claude-b")
	f.executor.lift("claude-a")
	f.executor.mu.Lock()
	f.executor.transient = map[string]int{"claude-a": 1}
	f.executor.mu.Unlock()

	resp, err := f.execute(cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("Execute error = %v, want the retry served by claude-a", err)
	}
	if string(resp.Payload) != `{"served_by":"claude-a"}` {
		t.Fatalf("payload = %s, want served by claude-a", resp.Payload)
	}
	if calls := f.executor.callCount("claude-b"); calls != 0 {
		t.Fatalf("cooled claude-b was trialled %d time(s) while claude-a was healthy", calls)
	}
}

func TestAllCooledTrialNoneLiftReturnsTodaysErrorAndHonoursGap(t *testing.T) {
	f := newAllCooledFixture(t, nil, internalconfig.QuotaReconcileClear, "claude-a", "claude-b")
	f.cool(t, "claude-a")
	f.cool(t, "claude-b")

	_, err := f.execute(cliproxyexecutor.Options{})
	requireModelCooldownError(t, err)
	for _, id := range []string{"claude-a", "claude-b"} {
		if calls := f.executor.callCount(id); calls != 1 {
			t.Fatalf("%s tried %d times, want exactly once", id, calls)
		}
		if !f.cooled(id) {
			t.Fatalf("%s cooldown cleared by a 429", id)
		}
	}

	// A second request inside the 60s gap behaves exactly as today.
	f.clock.advance(59 * time.Second)
	_, err = f.execute(cliproxyexecutor.Options{})
	requireModelCooldownError(t, err)
	for _, id := range []string{"claude-a", "claude-b"} {
		if calls := f.executor.callCount(id); calls != 1 {
			t.Fatalf("%s trialled again inside the gap (calls = %d)", id, calls)
		}
	}

	// After the gap each credential may be trialled once more.
	f.clock.advance(2 * time.Second)
	_, err = f.execute(cliproxyexecutor.Options{})
	requireModelCooldownError(t, err)
	for _, id := range []string{"claude-a", "claude-b"} {
		if calls := f.executor.callCount(id); calls != 2 {
			t.Fatalf("%s calls after the gap = %d, want 2", id, calls)
		}
	}
}

func TestAllCooledTrialBudgetFollowsRequestRetry(t *testing.T) {
	f := newAllCooledFixture(t, nil, internalconfig.QuotaReconcileClear, "claude-a", "claude-b")
	f.manager.SetRetryConfig(0, 0, 0)
	f.cool(t, "claude-a")
	f.cool(t, "claude-b")

	_, err := f.execute(cliproxyexecutor.Options{})
	requireModelCooldownError(t, err)
	if total := f.executor.callCount("claude-a") + f.executor.callCount("claude-b"); total != 1 {
		t.Fatalf("trials with request-retry 0 = %d, want 1", total)
	}
}

func TestAllCooledTrialSkippedWhileHealthyCandidateExists(t *testing.T) {
	f := newAllCooledFixture(t, nil, internalconfig.QuotaReconcileClear, "claude-a", "claude-c")
	f.cool(t, "claude-a")
	f.executor.lift("claude-a")
	f.executor.lift("claude-c")

	resp, err := f.execute(cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if string(resp.Payload) != `{"served_by":"claude-c"}` {
		t.Fatalf("payload = %s, want served by the healthy claude-c", resp.Payload)
	}
	if calls := f.executor.callCount("claude-a"); calls != 0 {
		t.Fatalf("cooled claude-a trialled %d times while a healthy candidate existed", calls)
	}
	if !f.cooled("claude-a") {
		t.Fatal("cooled claude-a was cleared without a trial")
	}
}

func TestAllCooledTrialOffModeKeepsTodaysBehaviour(t *testing.T) {
	f := newAllCooledFixture(t, nil, internalconfig.QuotaReconcileOff, "claude-a")
	f.cool(t, "claude-a")
	f.executor.lift("claude-a")

	_, err := f.execute(cliproxyexecutor.Options{})
	requireModelCooldownError(t, err)
	if calls := f.executor.callCount("claude-a"); calls != 0 {
		t.Fatalf("trial ran with quota-reconcile off (calls = %d)", calls)
	}
}

func TestAllCooledTrialObserveModeServesButKeepsCooldown(t *testing.T) {
	f := newAllCooledFixture(t, nil, internalconfig.QuotaReconcileObserve, "claude-a")
	f.cool(t, "claude-a")
	f.executor.lift("claude-a")

	if _, err := f.execute(cliproxyexecutor.Options{}); err != nil {
		t.Fatalf("Execute error = %v, want the trial served", err)
	}
	if !f.cooled("claude-a") {
		t.Fatal("observe mode cleared the cooldown")
	}
}

func TestAllCooledTrialStreamSendsNoBytesFromFailedTrial(t *testing.T) {
	t.Run("none lift", func(t *testing.T) {
		f := newAllCooledFixture(t, nil, internalconfig.QuotaReconcileClear, "claude-a", "claude-b")
		f.cool(t, "claude-a")
		f.cool(t, "claude-b")

		result, err := f.manager.ExecuteStream(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: f.model}, cliproxyexecutor.Options{Stream: true})
		if result != nil {
			t.Fatalf("stream result = %+v, want none (no bytes on a failed trial)", result)
		}
		requireModelCooldownError(t, err)
		for _, id := range []string{"claude-a", "claude-b"} {
			if calls := f.executor.callCount(id); calls != 1 {
				t.Fatalf("%s tried %d times, want exactly once", id, calls)
			}
		}
	})

	t.Run("second lifts", func(t *testing.T) {
		f := newAllCooledFixture(t, nil, internalconfig.QuotaReconcileClear, "claude-a", "claude-b")
		f.cool(t, "claude-a")
		f.cool(t, "claude-b")
		f.executor.lift("claude-b")

		result, err := f.manager.ExecuteStream(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: f.model}, cliproxyexecutor.Options{Stream: true})
		if err != nil {
			t.Fatalf("ExecuteStream error = %v", err)
		}
		var body string
		for chunk := range result.Chunks {
			if chunk.Err != nil {
				t.Fatalf("stream carried an error chunk: %v", chunk.Err)
			}
			body += string(chunk.Payload)
		}
		if body != "data: served_by claude-b\n\n" {
			t.Fatalf("stream body = %q, want only claude-b's bytes", body)
		}
		if f.cooled("claude-b") {
			t.Fatal("claude-b still cooled after a streamed 200 with allowed headers")
		}
	})
}

func TestAllCooledTrialDoesNotPinSessionToFailedTrial(t *testing.T) {
	affinity := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &RoundRobinSelector{}, TTL: time.Hour})
	t.Cleanup(affinity.Stop)
	f := newAllCooledFixture(t, affinity, internalconfig.QuotaReconcileClear, "claude-a", "claude-b")
	f.cool(t, "claude-a")
	f.cool(t, "claude-b")
	opts := cliproxyexecutor.Options{Headers: http.Header{"Session-Id": []string{"all-cooled-session"}}}

	// Neither lifts: both trials fail and the session is bound to neither.
	_, err := f.execute(opts)
	requireModelCooldownError(t, err)
	if bound, _ := affinity.LookupAffinity("mixed", f.model, "all-cooled-session"); bound != "" {
		t.Fatalf("session bound to %q after failed trials, want unbound", bound)
	}

	// After the gap one lifts. A trial bypasses the selector, so it creates no
	// binding at all: never one to the credential that failed.
	f.clock.advance(61 * time.Second)
	f.executor.lift("claude-a")
	resp, err := f.execute(opts)
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if string(resp.Payload) != `{"served_by":"claude-a"}` {
		t.Fatalf("payload = %s, want served by claude-a", resp.Payload)
	}
	if bound, _ := affinity.LookupAffinity("mixed", f.model, "all-cooled-session"); bound == "claude-b" {
		t.Fatal("session pinned to claude-b, whose trial failed")
	}

	// The next request selects normally and binds to the recovered credential.
	if _, err = f.execute(opts); err != nil {
		t.Fatalf("follow-up Execute error = %v", err)
	}
	if bound, _ := affinity.LookupAffinity("mixed", f.model, "all-cooled-session"); bound != "claude-a" {
		t.Fatalf("session bound to %q after normal selection, want claude-a", bound)
	}
}

func TestAllCooledAdapterRecheckClearsAndReselects(t *testing.T) {
	f := newAllCooledFixtureFor(t, "codex", nil, internalconfig.QuotaReconcileClear, "codex-a", "codex-b")
	adapter := &fakeRecheckAdapter{provider: "codex", verdict: QuotaVerdict{State: QuotaVerdictAllowed, Source: "fake"}}
	f.manager.RegisterQuotaRecheckAdapter(adapter)
	f.cool(t, "codex-a")
	f.cool(t, "codex-b")
	f.executor.lift("codex-a")
	f.executor.lift("codex-b")

	if _, err := f.execute(cliproxyexecutor.Options{}); err != nil {
		t.Fatalf("Execute error = %v, want the request served after the recheck", err)
	}
	if adapter.calls != 2 {
		t.Fatalf("verdict calls = %d, want one per cooled credential", adapter.calls)
	}
	if f.cooled("codex-a") || f.cooled("codex-b") {
		t.Fatal("allowed verdicts did not clear the cooldowns")
	}
}

func TestAllCooledAdapterRecheckLimitedKeepsTodaysError(t *testing.T) {
	f := newAllCooledFixtureFor(t, "codex", nil, internalconfig.QuotaReconcileClear, "codex-a")
	adapter := &fakeRecheckAdapter{provider: "codex", verdict: QuotaVerdict{State: QuotaVerdictLimited, Source: "fake"}}
	f.manager.RegisterQuotaRecheckAdapter(adapter)
	f.cool(t, "codex-a")

	_, err := f.execute(cliproxyexecutor.Options{})
	requireModelCooldownError(t, err)
	if adapter.calls != 1 {
		t.Fatalf("verdict calls = %d, want 1", adapter.calls)
	}
	if calls := f.executor.callCount("codex-a"); calls != 0 {
		t.Fatalf("codex has no half-open path but its request ran %d times", calls)
	}

	// Inside the gap the adapter is not asked again.
	f.clock.advance(30 * time.Second)
	_, err = f.execute(cliproxyexecutor.Options{})
	requireModelCooldownError(t, err)
	if adapter.calls != 1 {
		t.Fatalf("verdict calls inside the gap = %d, want 1", adapter.calls)
	}
}
