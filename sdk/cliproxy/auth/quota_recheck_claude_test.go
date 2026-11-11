package auth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// claudeProbeExecutor is a fake Claude executor. Inference requests answer with
// the configured rate-limit headers (and error, if any); HttpRequest serves the
// usage endpoint.
type claudeProbeExecutor struct {
	mu          sync.Mutex
	headers     http.Header
	err         error
	during      func() // runs inside the inference request, before it returns
	inferModels []string

	usageStatus int
	usageBody   string
	usageQueue  []string // when set, each call consumes one body
	usageReqs   []*http.Request
}

func (e *claudeProbeExecutor) Identifier() string { return "claude" }

func (e *claudeProbeExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, errors.New("not used")
}

func (e *claudeProbeExecutor) ExecuteStream(ctx context.Context, _ *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.mu.Lock()
	e.inferModels = append(e.inferModels, req.Model)
	headers, err, during := e.headers, e.err, e.during
	e.mu.Unlock()
	if during != nil {
		during()
	}
	internallogging.SetResponseHeaders(ctx, headers)
	if err != nil {
		return nil, err
	}
	ch := make(chan cliproxyexecutor.StreamChunk, 1)
	ch <- cliproxyexecutor.StreamChunk{Payload: []byte(`data: {"type":"response.completed","response":{"status":"completed"}}`)}
	close(ch)
	return &cliproxyexecutor.StreamResult{Chunks: ch}, nil
}

func (e *claudeProbeExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) { return auth, nil }

func (e *claudeProbeExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *claudeProbeExecutor) HttpRequest(_ context.Context, _ *Auth, req *http.Request) (*http.Response, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.usageReqs = append(e.usageReqs, req)
	body := e.usageBody
	if len(e.usageQueue) > 0 {
		body, e.usageQueue = e.usageQueue[0], e.usageQueue[1:]
	}
	return &http.Response{StatusCode: e.usageStatus, Body: io.NopCloser(strings.NewReader(body))}, nil
}

func (e *claudeProbeExecutor) set(fn func(e *claudeProbeExecutor)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	fn(e)
}

func (e *claudeProbeExecutor) inferCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.inferModels)
}

func (e *claudeProbeExecutor) usageCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.usageReqs)
}

// claudeRateLimit429 mimics the Claude executor's unified-rejection 429.
type claudeRateLimit429 struct{ retry time.Duration }

func (e claudeRateLimit429) Error() string              { return "rate_limit_error: rejected" }
func (e claudeRateLimit429) StatusCode() int            { return http.StatusTooManyRequests }
func (e claudeRateLimit429) IsCredentialScoped() bool   { return true }
func (e claudeRateLimit429) RetryAfter() *time.Duration { d := e.retry; return &d }

func claudeHeaders(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}

var (
	claudeRejected7d = claudeHeaders(
		"Anthropic-Ratelimit-Unified-Status", "rejected",
		"Anthropic-Ratelimit-Unified-7d-Status", "rejected",
		"Anthropic-Ratelimit-Unified-5h-Status", "allowed",
	)
	claudeAllowedWarning = claudeHeaders(
		"Anthropic-Ratelimit-Unified-Status", "allowed_warning",
		"Anthropic-Ratelimit-Unified-7d-Status", "allowed_warning",
		"Anthropic-Ratelimit-Unified-5h-Status", "allowed",
	)
)

const (
	claudeTestHaiku = "claude-haiku-test"
	claudeTestOpus  = "claude-opus-test"
)

// newClaudeRecheckManager registers one Claude credential cooled by a real
// credential-scoped 7d rejection (2h ahead) and returns the manager, the fake
// executor and a fake clock driving the recheck schedule.
func newClaudeRecheckManager(t *testing.T, mode internalconfig.QuotaReconcileMode) (*Manager, *claudeProbeExecutor, *fakeClock) {
	t.Helper()
	const id = "claude-recheck"
	registry.GetGlobalRegistry().RegisterClient(id, "claude", []*registry.ModelInfo{{ID: claudeTestOpus}, {ID: claudeTestHaiku}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })

	exec := &claudeProbeExecutor{usageStatus: http.StatusOK, usageBody: claudeUsageBody("50", "2026-11-15T10:00:00Z")}
	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(exec)
	manager.SetConfig(&internalconfig.Config{QuotaReconcile: internalconfig.QuotaReconcileConfig{"claude": {Mode: mode}}})
	if _, err := manager.Register(context.Background(), &Auth{ID: id, Provider: "claude", Status: StatusActive}); err != nil {
		t.Fatalf("register: %v", err)
	}
	coolClaude(t, manager, id, 2*time.Hour)
	clock := &fakeClock{t: time.Now()}
	manager.quotaReconcile.schedule.clock = clock.now
	return manager, exec, clock
}

// coolClaude records a real credential-scoped 7d rejection through MarkResult.
func coolClaude(t *testing.T, manager *Manager, id string, retry time.Duration) {
	t.Helper()
	ctx := internallogging.WithFreshResponseHeadersHolder(context.Background())
	internallogging.SetResponseHeaders(ctx, claudeRejected7d)
	manager.MarkResult(ctx, Result{
		AuthID: id, Provider: "claude", Model: claudeTestOpus, RouteModel: claudeTestOpus,
		Error:      &Error{HTTPStatus: http.StatusTooManyRequests, Message: "rejected"},
		RetryAfter: &retry, CredentialScope: true,
	})
	got, _ := manager.GetByID(id)
	if !got.Quota.Exceeded || got.Quota.Reason != "credential_quota" {
		t.Fatalf("setup: credential not cooled by a credential-scoped rejection: %+v", got.Quota)
	}
}

func claudeUsageBody(percent, resetsAt string) string {
	return `{"five_hour":{"utilization":3,"resets_at":"2026-11-12T10:00:00Z"},` +
		`"seven_day":{"utilization":` + percent + `,"resets_at":"` + resetsAt + `"},` +
		`"limits":[{"kind":"seven_day","percent":` + percent + `,"severity":"warning","resets_at":"` + resetsAt + `","is_active":true}],` +
		`"extra_usage":{"is_enabled":false,"user_disabled":false,"spend_limit_reached":false},` +
		`"spend":{"enabled":false}}`
}

func claudeCooled(t *testing.T, manager *Manager) bool {
	t.Helper()
	got, _ := manager.GetByID("claude-recheck")
	return got.Quota.Exceeded
}

func TestClaudeRecheckClearModeClearsOnAllowedHeaders(t *testing.T) {
	manager, exec, _ := newClaudeRecheckManager(t, internalconfig.QuotaReconcileClear)
	exec.headers = claudeAllowedWarning

	manager.reconcileQuotaCooldowns(context.Background())

	if exec.inferCount() != 1 {
		t.Fatalf("inference probes = %d, want 1", exec.inferCount())
	}
	if exec.inferModels[0] != claudeTestHaiku {
		t.Fatalf("probe model = %q, want the cheapest (%q)", exec.inferModels[0], claudeTestHaiku)
	}
	if claudeCooled(t, manager) {
		t.Fatalf("cooldown kept although the probe headers say allowed")
	}
	if exec.usageCount() != 0 {
		t.Fatalf("usage snapshot calls = %d, want 0 (the verdict was due anyway)", exec.usageCount())
	}
}

func TestClaudeRecheckObserveModeNeverClears(t *testing.T) {
	manager, exec, clock := newClaudeRecheckManager(t, internalconfig.QuotaReconcileObserve)
	exec.headers = claudeAllowedWarning

	manager.reconcileQuotaCooldowns(context.Background())
	clock.advance(31 * time.Minute)
	manager.reconcileQuotaCooldowns(context.Background())

	if exec.inferCount() != 2 {
		t.Fatalf("inference probes = %d, want 2", exec.inferCount())
	}
	if !claudeCooled(t, manager) {
		t.Fatalf("observe mode cleared the cooldown")
	}
}

// The usage endpoint can read low while inference says rejected, so a low
// usage reading must never clear on its own: only the verdict can.
func TestClaudeRecheckLowUsageNeverClearsWithoutAllowedVerdict(t *testing.T) {
	cases := map[string]func(e *claudeProbeExecutor){
		"probe 429": func(e *claudeProbeExecutor) {
			e.headers, e.err = claudeRejected7d, claudeRateLimit429{retry: 5 * time.Hour}
		},
		"probe 200 but unified status rejected": func(e *claudeProbeExecutor) { e.headers = claudeRejected7d },
		"probe transport error": func(e *claudeProbeExecutor) {
			e.headers, e.err = nil, errors.New("connection reset")
		},
		"probe 200 without rate-limit headers": func(e *claudeProbeExecutor) { e.headers = nil },
		"probe 200 without the rejected window": func(e *claudeProbeExecutor) {
			e.headers = claudeHeaders("Anthropic-Ratelimit-Unified-Status", "allowed")
		},
	}
	for name, setProbe := range cases {
		t.Run(name, func(t *testing.T) {
			manager, exec, clock := newClaudeRecheckManager(t, internalconfig.QuotaReconcileClear)
			exec.set(setProbe)
			exec.set(func(e *claudeProbeExecutor) {
				e.usageQueue = []string{
					claudeUsageBody("50", "2026-11-15T10:00:00Z"),
					claudeUsageBody("5", "2026-11-15T10:00:00Z"),
				}
			})

			manager.reconcileQuotaCooldowns(context.Background()) // timer verdict
			clock.advance(time.Minute)
			manager.reconcileQuotaCooldowns(context.Background()) // snapshot baseline at 50%
			clock.advance(10 * time.Minute)
			manager.reconcileQuotaCooldowns(context.Background()) // snapshot at 5%: early verdict

			if exec.usageCount() != 2 {
				t.Fatalf("usage snapshots = %d, want 2", exec.usageCount())
			}
			if exec.inferCount() != 2 {
				t.Fatalf("inference probes = %d, want 2 (timer + snapshot trigger)", exec.inferCount())
			}
			if !claudeCooled(t, manager) {
				t.Fatalf("cooldown cleared without an allowed verdict")
			}
		})
	}
}

// A probe 429 is recorded once by VerifyInference's MarkResult; the loop does
// not apply it again.
func TestClaudeRecheckProbe429ReDerivesDeadlineOnce(t *testing.T) {
	manager, exec, _ := newClaudeRecheckManager(t, internalconfig.QuotaReconcileClear)
	exec.headers, exec.err = claudeRejected7d, claudeRateLimit429{retry: 5 * time.Hour}
	before, _ := manager.GetByID("claude-recheck")

	manager.reconcileQuotaCooldowns(context.Background())

	after, _ := manager.GetByID("claude-recheck")
	if !after.Quota.Exceeded || !after.Quota.NextRecoverAt.After(before.Quota.NextRecoverAt) {
		t.Fatalf("probe 429 did not re-derive the deadline: before=%v after=%v", before.Quota.NextRecoverAt, after.Quota.NextRecoverAt)
	}
	if want := time.Now().Add(5*time.Hour + time.Minute); after.Quota.NextRecoverAt.After(want) {
		t.Fatalf("deadline %v is past the probe's own 5h reset; it was applied more than once", after.Quota.NextRecoverAt)
	}
}

func TestClaudeRecheckSnapshotChangeTriggersEarlyVerdict(t *testing.T) {
	manager, exec, clock := newClaudeRecheckManager(t, internalconfig.QuotaReconcileClear)
	// A long timer interval so only the snapshot can cause the second verdict.
	manager.SetConfig(&internalconfig.Config{QuotaReconcile: internalconfig.QuotaReconcileConfig{
		"claude": {Mode: internalconfig.QuotaReconcileClear, Interval: 2 * time.Hour},
	}})
	exec.headers, exec.err = claudeRejected7d, claudeRateLimit429{retry: 5 * time.Hour}
	same := claudeUsageBody("80", "2026-11-15T10:00:00Z")
	exec.usageQueue = []string{same, same, claudeUsageBody("90", "2026-11-15T10:00:00Z"), claudeUsageBody("90", "2026-11-12T12:00:00Z")}

	manager.reconcileQuotaCooldowns(context.Background()) // timer verdict
	clock.advance(time.Minute)
	manager.reconcileQuotaCooldowns(context.Background()) // baseline
	clock.advance(5 * time.Minute)
	manager.reconcileQuotaCooldowns(context.Background()) // snapshot not due yet
	if exec.usageCount() != 1 {
		t.Fatalf("usage snapshots = %d, want 1 (cadence is 10m)", exec.usageCount())
	}
	clock.advance(5 * time.Minute)
	manager.reconcileQuotaCooldowns(context.Background()) // unchanged
	clock.advance(10 * time.Minute)
	manager.reconcileQuotaCooldowns(context.Background()) // percent rose: not a trigger
	if exec.inferCount() != 1 {
		t.Fatalf("inference probes = %d, want 1 (no relevant change)", exec.inferCount())
	}

	clock.advance(10 * time.Minute)
	exec.err = nil
	exec.headers = claudeAllowedWarning
	manager.reconcileQuotaCooldowns(context.Background()) // resets_at moved: early verdict
	if exec.usageCount() != 4 || exec.inferCount() != 2 {
		t.Fatalf("snapshots=%d probes=%d, want 4 and 2", exec.usageCount(), exec.inferCount())
	}
	if claudeCooled(t, manager) {
		t.Fatalf("early verdict said allowed but the cooldown was kept")
	}
	req := exec.usageReqs[0]
	if req.URL.String() != "https://api.anthropic.com/api/oauth/usage" || req.Header.Get("Anthropic-Beta") != "oauth-2025-04-20" {
		t.Fatalf("usage request = %s beta=%q", req.URL, req.Header.Get("Anthropic-Beta"))
	}
}

func TestClaudeRecheckSnapshot429BacksOff(t *testing.T) {
	manager, exec, clock := newClaudeRecheckManager(t, internalconfig.QuotaReconcileClear)
	exec.headers, exec.err = claudeRejected7d, claudeRateLimit429{retry: 5 * time.Hour}
	exec.usageStatus = http.StatusTooManyRequests

	manager.reconcileQuotaCooldowns(context.Background()) // timer verdict
	clock.advance(time.Minute)
	manager.reconcileQuotaCooldowns(context.Background()) // snapshot 429: back off 20m
	clock.advance(10 * time.Minute)
	manager.reconcileQuotaCooldowns(context.Background())
	if exec.usageCount() != 1 {
		t.Fatalf("usage snapshots = %d, want 1 (backed off after a 429)", exec.usageCount())
	}
	clock.advance(10 * time.Minute)
	manager.reconcileQuotaCooldowns(context.Background())
	if exec.usageCount() != 2 {
		t.Fatalf("usage snapshots = %d, want 2 after the backoff", exec.usageCount())
	}
	if exec.inferCount() != 1 || !claudeCooled(t, manager) {
		t.Fatalf("a failed snapshot triggered a verdict or a clear: probes=%d", exec.inferCount())
	}
}

func TestClaudeRecheckGenerationGuardHolds(t *testing.T) {
	manager, exec, _ := newClaudeRecheckManager(t, internalconfig.QuotaReconcileClear)
	exec.headers = claudeAllowedWarning
	// Real traffic records a newer rejection while the probe is in flight.
	exec.during = func() { coolClaude(t, manager, "claude-recheck", 4*time.Hour) }

	manager.reconcileQuotaCooldowns(context.Background())

	if !claudeCooled(t, manager) {
		t.Fatalf("a cooldown set during the probe was cleared by the stale verdict")
	}
}

// A rejection during the probe that leaves the existing, later deadline in
// place still makes the probe's verdict stale.
func TestClaudeRecheckGenerationGuardHoldsWhenDeadlineUnchanged(t *testing.T) {
	manager, exec, _ := newClaudeRecheckManager(t, internalconfig.QuotaReconcileClear)
	exec.headers = claudeAllowedWarning
	exec.during = func() { coolClaude(t, manager, "claude-recheck", time.Hour) }

	manager.reconcileQuotaCooldowns(context.Background())

	if !claudeCooled(t, manager) {
		t.Fatalf("a rejection during the probe was ignored because it kept the deadline")
	}
}

// An ordinary model-level 429 is not a unified window rejection; the adapter
// leaves it to its short backoff and spends no probe on it.
func TestClaudeRecheckSkipsNonUnifiedCooldown(t *testing.T) {
	const id = "claude-model-429"
	manager := NewManager(nil, nil, nil)
	exec := &claudeProbeExecutor{headers: claudeAllowedWarning}
	manager.RegisterExecutor(exec)
	manager.SetConfig(&internalconfig.Config{QuotaReconcile: internalconfig.QuotaReconcileConfig{"claude": {Mode: internalconfig.QuotaReconcileClear}}})
	if _, err := manager.Register(context.Background(), &Auth{ID: id, Provider: "claude", Status: StatusActive}); err != nil {
		t.Fatalf("register: %v", err)
	}
	retry := 10 * time.Minute
	manager.MarkResult(context.Background(), Result{
		AuthID: id, Provider: "claude", Model: claudeTestOpus, RouteModel: claudeTestOpus,
		Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "rate limited"}, RetryAfter: &retry,
	})
	if got, _ := manager.GetByID(id); !authHasQuotaCooldown(got) {
		t.Fatalf("setup: model-level 429 left no quota cooldown")
	}

	manager.reconcileQuotaCooldowns(context.Background())

	if exec.inferCount() != 0 {
		t.Fatalf("probed a non-unified cooldown %d times", exec.inferCount())
	}
}

func TestClaudeUsageSnapshotSignals(t *testing.T) {
	body := `{"five_hour":{"utilization":3,"resets_at":"2026-11-12T10:00:00Z"},` +
		`"seven_day":{"utilization":71,"resets_at":"2026-11-15T10:00:00Z"},` +
		`"limits":[{"kind":"seven_day","percent":71,"severity":"warning","resets_at":"2026-11-15T10:00:00Z","is_active":true},` +
		`{"kind":"seven_day_opus","percent":100,"severity":"critical","resets_at":null,"is_active":false}],` +
		`"extra_usage":{"is_enabled":true,"user_disabled":false,"spend_limit_reached":false},"spend":{"enabled":true}}`
	got := claudeUsageSignals([]byte(body))
	want := map[string]string{
		"five_hour.utilization":           "3",
		"five_hour.resets_at":             "2026-11-12T10:00:00Z",
		"seven_day.utilization":           "71",
		"seven_day.resets_at":             "2026-11-15T10:00:00Z",
		"limits.seven_day.percent":        "71",
		"limits.seven_day.severity":       "warning",
		"limits.seven_day.resets_at":      "2026-11-15T10:00:00Z",
		"limits.seven_day.is_active":      "true",
		"limits.seven_day_opus.percent":   "100",
		"limits.seven_day_opus.severity":  "critical",
		"limits.seven_day_opus.is_active": "false",
		"extra_usage.is_enabled":          "true",
		"extra_usage.user_disabled":       "false",
		"extra_usage.spend_limit_reached": "false",
		"spend.enabled":                   "true",
	}
	if len(got) != len(want) {
		t.Fatalf("signals = %v, want %v", got, want)
	}
	for key, val := range want {
		if got[key] != val {
			t.Fatalf("signal %s = %q, want %q (all: %v)", key, got[key], val, got)
		}
	}
}

func TestClaudeUsageSnapshotChanged(t *testing.T) {
	base := map[string]string{
		"seven_day.utilization": "80", "seven_day.resets_at": "2026-11-15T10:00:00Z",
		"limits.seven_day.severity": "critical", "limits.seven_day.is_active": "true",
		"extra_usage.is_enabled": "false",
	}
	with := func(key, val string) map[string]string {
		next := make(map[string]string, len(base))
		for k, v := range base {
			next[k] = v
		}
		if val == "" {
			delete(next, key)
		} else {
			next[key] = val
		}
		return next
	}
	cases := []struct {
		name string
		next map[string]string
		want bool
	}{
		{"unchanged", with("x", ""), false},
		{"percent rose", with("seven_day.utilization", "95"), false},
		{"percent dropped", with("seven_day.utilization", "10"), true},
		{"reset moved", with("seven_day.resets_at", "2026-11-12T10:00:00Z"), true},
		{"reset jitter under a minute", with("seven_day.resets_at", "2026-11-15T10:00:30Z"), false},
		{"severity changed", with("limits.seven_day.severity", "warning"), true},
		{"limit went inactive", with("limits.seven_day.is_active", "false"), true},
		{"limit disappeared", with("limits.seven_day.is_active", ""), true},
		{"extra usage enabled", with("extra_usage.is_enabled", "true"), true},
	}
	for _, tc := range cases {
		if got := claudeUsageSnapshotChanged(base, tc.next); got != tc.want {
			t.Errorf("%s: changed = %v, want %v", tc.name, got, tc.want)
		}
	}
	if claudeUsageSnapshotChanged(nil, base) {
		t.Errorf("the first snapshot must only set the baseline")
	}
}

func TestQuotaRecheckTriggeredVerdictHonoursMinimumGap(t *testing.T) {
	var s quotaRecheckSchedule
	now := time.Now()
	policy := quotaRecheckPolicy{interval: 30 * time.Minute, nearInterval: 5 * time.Minute, nearWindow: time.Hour}
	if !s.begin("a", now, policy, time.Time{}) {
		t.Fatalf("first check not due")
	}
	if s.beginTriggered("a", now.Add(quotaTriggerMinGap-time.Second), policy, time.Time{}) {
		t.Fatalf("triggered verdict ran inside the minimum gap")
	}
	if !s.beginTriggered("a", now.Add(quotaTriggerMinGap), policy, time.Time{}) {
		t.Fatalf("triggered verdict refused after the minimum gap")
	}
}

func TestCodexUsageProbeURLFollowsBaseURL(t *testing.T) {
	cases := map[string]string{
		"":                                      codexUsageProbeURL,
		"https://chatgpt.com/backend-api/codex": "https://chatgpt.com/backend-api/wham/usage",
		"http://127.0.0.1:18330/":               "http://127.0.0.1:18330/wham/usage",
		"http://mock:9/backend-api/codex/":      "http://mock:9/backend-api/wham/usage",
	}
	for base, want := range cases {
		auth := &Auth{Provider: "codex", Attributes: map[string]string{"base_url": base}}
		if got := codexUsageProbeURLFor(auth); got != want {
			t.Errorf("base %q: probe URL = %q, want %q", base, got, want)
		}
	}
}

func TestReconcileCodexProbeUsesCredentialBaseURL(t *testing.T) {
	exec := &usageProbeExecutor{status: http.StatusOK, body: `{"rate_limit":{"allowed":false,"limit_reached":true}}`}
	manager, authID := newQuotaCooldownManager(t, exec)
	got, _ := manager.GetByID(authID)
	got.Attributes = map[string]string{"base_url": "http://127.0.0.1:18330"}
	if _, err := manager.Update(context.Background(), got); err != nil {
		t.Fatalf("update: %v", err)
	}

	manager.reconcileQuotaCooldowns(context.Background())

	if len(exec.urls) != 1 || exec.urls[0] != "http://127.0.0.1:18330/wham/usage" {
		t.Fatalf("probe URLs = %v, want the credential's base URL", exec.urls)
	}
}
