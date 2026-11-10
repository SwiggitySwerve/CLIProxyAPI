package auth

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestQuotaWindowResetAt(t *testing.T) {
	t.Parallel()

	observedAt := time.Unix(1787279282, 0)
	tests := []struct {
		name       string
		signals    map[string]string
		unobserved bool
		want       int64 // epoch seconds; 0 means no usable reset time
	}{
		{
			name: "claude prefers the 7d window",
			signals: map[string]string{
				"Anthropic-Ratelimit-Unified-5h-Reset": "1787296800",
				"Anthropic-Ratelimit-Unified-7d-Reset": "1787695200",
				"Anthropic-Ratelimit-Unified-Reset":    "1787296800",
			},
			want: 1787695200,
		},
		{
			name: "claude falls back to the unified reset",
			signals: map[string]string{
				"Anthropic-Ratelimit-Unified-5h-Reset": "1787296800",
				"Anthropic-Ratelimit-Unified-Reset":    "1787300000",
			},
			want: 1787300000,
		},
		{
			name:    "claude 5h reset alone is not the long window",
			signals: map[string]string{"Anthropic-Ratelimit-Unified-5h-Reset": "1787296800"},
		},
		{
			name: "claude unparsable 7d falls back",
			signals: map[string]string{
				"Anthropic-Ratelimit-Unified-7d-Reset": "soon",
				"Anthropic-Ratelimit-Unified-Reset":    "1787296800",
			},
			want: 1787296800,
		},
		{
			name: "claude zero 7d falls back",
			signals: map[string]string{
				"Anthropic-Ratelimit-Unified-7d-Reset": "0",
				"Anthropic-Ratelimit-Unified-Reset":    "1787296800",
			},
			want: 1787296800,
		},
		{
			name: "claude millisecond 7d is absurd and falls back",
			signals: map[string]string{
				"Anthropic-Ratelimit-Unified-7d-Reset": "1787695200000",
				"Anthropic-Ratelimit-Unified-Reset":    "1787296800",
			},
			want: 1787296800,
		},
		{
			name:    "claude negative reset is rejected",
			signals: map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": "-5"},
		},
		{
			name:    "reset long before the observation is rejected",
			signals: map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": "1"},
		},
		{
			name:    "codex secondary reset-at",
			signals: map[string]string{"X-Codex-Secondary-Reset-At": "1787588999", "X-Codex-Primary-Reset-At": "1787290000"},
			want:    1787588999,
		},
		{
			name:    "codex reset-at wins over reset-after-seconds in the same window",
			signals: map[string]string{"X-Codex-Secondary-Reset-At": "1787588999", "X-Codex-Secondary-Reset-After-Seconds": "100"},
			want:    1787588999,
		},
		{
			name:    "codex secondary reset-after-seconds is relative to the observation",
			signals: map[string]string{"X-Codex-Secondary-Reset-After-Seconds": "309718", "X-Codex-Primary-Reset-At": "1787290000"},
			want:    1787279282 + 309718,
		},
		{
			name:    "codex falls back to the primary reset-at",
			signals: map[string]string{"X-Codex-Primary-Window-Minutes": "10080", "X-Codex-Primary-Reset-At": "1787588999"},
			want:    1787588999,
		},
		{
			name:    "codex falls back to the primary reset-after-seconds",
			signals: map[string]string{"X-Codex-Primary-Reset-After-Seconds": "600"},
			want:    1787279282 + 600,
		},
		{
			name: "codex invalid secondary values fall back to primary",
			signals: map[string]string{
				"X-Codex-Secondary-Reset-At":            "0",
				"X-Codex-Secondary-Reset-After-Seconds": "x",
				"X-Codex-Primary-Reset-At":              "1787290000",
			},
			want: 1787290000,
		},
		{
			name:    "codex absurd reset-after-seconds is rejected",
			signals: map[string]string{"X-Codex-Secondary-Reset-After-Seconds": "99999999"},
		},
		{
			name:    "codex zero reset-after-seconds is a reset at the observation",
			signals: map[string]string{"X-Codex-Primary-Reset-After-Seconds": "0"},
			want:    1787279282,
		},
		{
			name:    "codex zero secondary reset-after-seconds wins over the primary window",
			signals: map[string]string{"X-Codex-Secondary-Reset-After-Seconds": "0", "X-Codex-Primary-Reset-At": "1787290000"},
			want:    1787279282,
		},
		{
			name:    "codex negative reset-after-seconds is rejected",
			signals: map[string]string{"X-Codex-Primary-Reset-After-Seconds": "-1"},
		},
		{
			name:    "codex zero reset-at is rejected",
			signals: map[string]string{"X-Codex-Primary-Reset-At": "0"},
		},
		{
			name: "codex additional and code-review windows are ignored",
			signals: map[string]string{
				"X-Codex-Bengalfox-Secondary-Reset-At":      "1787588999",
				"X-Codex-Code-Review-Secondary-Reset-At":    "1787588999",
				"X-Codex-Additional-Spark-Primary-Reset-At": "1787588999",
			},
		},
		{
			name:       "no observation time means no reset time",
			signals:    map[string]string{"X-Codex-Secondary-Reset-At": "1787588999"},
			unobserved: true,
		},
		{name: "nil signals"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			observe := observedAt
			if tt.unobserved {
				observe = time.Time{}
			}
			got, ok := quotaWindowResetAt(tt.signals, observe)
			if tt.want == 0 {
				if ok {
					t.Fatalf("quotaWindowResetAt() = %v, true; want no reset time", got)
				}
				return
			}
			if !ok || got.Unix() != tt.want {
				t.Fatalf("quotaWindowResetAt() = %v (%d), %v; want %d", got, got.Unix(), ok, tt.want)
			}
		})
	}
}

func soonestResetTestAuth(id string, observedAt time.Time, signals map[string]string) *Auth {
	return &Auth{ID: id, Provider: "claude", Quota: QuotaState{ObservedAt: observedAt, Signals: signals}}
}

func claude7dReset(reset time.Time) map[string]string {
	return map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": strconv.FormatInt(reset.Unix(), 10)}
}

func TestSoonestResetSelectorPickRanking(t *testing.T) {
	t.Parallel()

	now := time.Now()
	observed := now.Add(-10 * time.Minute)
	const model = "test-model"
	cooling := map[string]*ModelState{model: {
		Status:         StatusActive,
		Unavailable:    true,
		NextRetryAfter: now.Add(30 * time.Minute),
		Quota:          QuotaState{Exceeded: true, NextRecoverAt: now.Add(30 * time.Minute)},
	}}

	tests := []struct {
		name     string
		provider string
		auths    []*Auth
		want     string
	}{
		{
			name: "unobserved credential is tried first",
			auths: []*Auth{
				soonestResetTestAuth("a", observed, claude7dReset(now.Add(time.Hour))),
				soonestResetTestAuth("b", time.Time{}, nil),
			},
			want: "b",
		},
		{
			name: "soonest future reset wins",
			auths: []*Auth{
				soonestResetTestAuth("a", observed, claude7dReset(now.Add(48*time.Hour))),
				soonestResetTestAuth("b", observed, claude7dReset(now.Add(2*time.Hour))),
				soonestResetTestAuth("c", observed, claude7dReset(now.Add(24*time.Hour))),
			},
			want: "b",
		},
		{
			name: "passed reset counts as unobserved and ties by ID",
			auths: []*Auth{
				soonestResetTestAuth("a", observed, claude7dReset(now.Add(time.Hour))),
				soonestResetTestAuth("c", time.Time{}, nil),
				soonestResetTestAuth("b", now.Add(-48*time.Hour), claude7dReset(now.Add(-time.Hour))),
			},
			want: "b",
		},
		{
			name: "observed without reset time goes last",
			auths: []*Auth{
				soonestResetTestAuth("a", observed, map[string]string{"Anthropic-Ratelimit-Unified-Status": "allowed"}),
				soonestResetTestAuth("b", observed, claude7dReset(now.Add(72*time.Hour))),
			},
			want: "b",
		},
		{
			name: "only observed without reset falls back to ID order",
			auths: []*Auth{
				soonestResetTestAuth("b", observed, map[string]string{"Anthropic-Ratelimit-Unified-Status": "allowed"}),
				soonestResetTestAuth("a", observed, map[string]string{"Anthropic-Ratelimit-Unified-Status": "allowed"}),
			},
			want: "a",
		},
		{
			name: "equal resets tie by ID",
			auths: []*Auth{
				soonestResetTestAuth("b", observed, claude7dReset(now.Add(5*time.Hour))),
				soonestResetTestAuth("a", observed, claude7dReset(now.Add(5*time.Hour))),
			},
			want: "a",
		},
		{
			name: "cooling credential is skipped even with the soonest reset",
			auths: []*Auth{
				{ID: "a", Provider: "claude", ModelStates: cooling, Quota: QuotaState{ObservedAt: observed, Signals: claude7dReset(now.Add(time.Hour))}},
				soonestResetTestAuth("b", observed, claude7dReset(now.Add(96*time.Hour))),
			},
			want: "b",
		},
		{
			name: "disabled credential is skipped",
			auths: []*Auth{
				{ID: "a", Provider: "claude", Disabled: true, Quota: QuotaState{ObservedAt: observed, Signals: claude7dReset(now.Add(time.Hour))}},
				soonestResetTestAuth("b", observed, claude7dReset(now.Add(96*time.Hour))),
			},
			want: "b",
		},
		{
			name: "higher priority tier wins regardless of reset",
			auths: []*Auth{
				{ID: "a", Provider: "claude", Attributes: map[string]string{"priority": "0"}, Quota: QuotaState{ObservedAt: observed, Signals: claude7dReset(now.Add(time.Hour))}},
				{ID: "b", Provider: "claude", Attributes: map[string]string{"priority": "10"}, Quota: QuotaState{ObservedAt: observed, Signals: claude7dReset(now.Add(144 * time.Hour))}},
			},
			want: "b",
		},
		{
			name: "codex relative reset competes with claude absolute reset",
			auths: []*Auth{
				soonestResetTestAuth("a", observed, claude7dReset(now.Add(2*time.Hour))),
				{ID: "b", Provider: "codex", Quota: QuotaState{ObservedAt: observed, Signals: map[string]string{"X-Codex-Secondary-Reset-After-Seconds": "3600"}}},
			},
			want: "b",
		},
		{
			name: "codex zero reset-after-seconds is tried before a known future reset",
			auths: []*Auth{
				soonestResetTestAuth("a", observed, claude7dReset(now.Add(time.Hour))),
				{ID: "b", Provider: "codex", Quota: QuotaState{ObservedAt: observed, Signals: map[string]string{"X-Codex-Secondary-Reset-After-Seconds": "0"}}},
			},
			want: "b",
		},
		{
			name:     "providers without quota observation behave like fill-first",
			provider: "gemini",
			auths: []*Auth{
				{ID: "c", Provider: "gemini"},
				{ID: "a", Provider: "gemini"},
				{ID: "b", Provider: "gemini"},
			},
			want: "a",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := tt.provider
			if provider == "" {
				provider = "mixed"
			}
			got, err := (&SoonestResetSelector{}).Pick(context.Background(), provider, model, cliproxyexecutor.Options{}, tt.auths)
			if err != nil {
				t.Fatalf("Pick() error = %v", err)
			}
			if got == nil || got.ID != tt.want {
				t.Fatalf("Pick() = %v, want %s", got, tt.want)
			}
		})
	}
}

// TestSoonestResetSelectorKeepsPickingPassedResetUntilRefreshed documents that the
// ranking is stateless: a credential whose stored reset has passed keeps the first
// rank on every pick until its snapshot is replaced.
func TestSoonestResetSelectorKeepsPickingPassedResetUntilRefreshed(t *testing.T) {
	t.Parallel()

	now := time.Now()
	auths := []*Auth{
		soonestResetTestAuth("a", now.Add(-10*time.Minute), claude7dReset(now.Add(time.Hour))),
		soonestResetTestAuth("b", now.Add(-48*time.Hour), claude7dReset(now.Add(-time.Hour))),
		soonestResetTestAuth("c", now.Add(-10*time.Minute), claude7dReset(now.Add(2*time.Hour))),
	}
	selector := &SoonestResetSelector{}
	for i := 0; i < 5; i++ {
		got, err := selector.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatalf("Pick() #%d error = %v", i, err)
		}
		if got.ID != "b" {
			t.Fatalf("Pick() #%d = %s, want b while its snapshot is not refreshed", i, got.ID)
		}
	}
}

func TestSoonestResetSelectorPickPrefersCodexWebsocketAuths(t *testing.T) {
	t.Parallel()

	now := time.Now()
	observed := now.Add(-time.Minute)
	auths := []*Auth{
		{ID: "a", Provider: "codex", Quota: QuotaState{ObservedAt: observed, Signals: map[string]string{"X-Codex-Secondary-Reset-After-Seconds": "60"}}},
		{ID: "b", Provider: "codex", Attributes: map[string]string{"websockets": "true"}, Quota: QuotaState{ObservedAt: observed, Signals: map[string]string{"X-Codex-Secondary-Reset-After-Seconds": "86400"}}},
	}
	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	got, err := (&SoonestResetSelector{}).Pick(ctx, "codex", "", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if got.ID != "b" {
		t.Fatalf("Pick() = %s, want websocket credential b", got.ID)
	}
}

func TestSoonestResetSelectorPickDoesNotReorderPrevalidatedCandidates(t *testing.T) {
	t.Parallel()

	now := time.Now()
	observed := now.Add(-time.Minute)
	auths := []*Auth{
		soonestResetTestAuth("a", observed, claude7dReset(now.Add(48*time.Hour))),
		soonestResetTestAuth("b", observed, claude7dReset(now.Add(time.Hour))),
	}
	ctx := context.WithValue(context.Background(), prevalidatedAuthCandidatesKey{}, true)
	got, err := (&SoonestResetSelector{}).Pick(ctx, "claude", "", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if got.ID != "b" {
		t.Fatalf("Pick() = %s, want b", got.ID)
	}
	if auths[0].ID != "a" || auths[1].ID != "b" {
		t.Fatalf("Pick() reordered its input: %s, %s", auths[0].ID, auths[1].ID)
	}
}

func TestSessionAffinityWithSoonestResetFallback(t *testing.T) {
	t.Parallel()

	now := time.Now()
	observed := now.Add(-time.Minute)
	selector := NewSessionAffinitySelector(&SoonestResetSelector{})
	defer selector.Stop()
	auths := []*Auth{
		soonestResetTestAuth("a", observed, claude7dReset(now.Add(48*time.Hour))),
		soonestResetTestAuth("b", observed, claude7dReset(now.Add(time.Hour))),
	}
	got, err := selector.Pick(context.Background(), "claude", "claude-model", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if got.ID != "b" {
		t.Fatalf("Pick() without session = %s, want soonest reset b", got.ID)
	}

	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Claude-Code-Session-Id": []string{"soonest-reset-session"}}}
	first, err := selector.Pick(context.Background(), "claude", "claude-model", opts, auths)
	if err != nil {
		t.Fatalf("Pick() with session error = %v", err)
	}
	if first.ID != "b" {
		t.Fatalf("Pick() new session = %s, want soonest reset b", first.ID)
	}
	// The binding outranks a credential whose reset later becomes sooner.
	auths[0].Quota.Signals = claude7dReset(now.Add(10 * time.Minute))
	second, err := selector.Pick(context.Background(), "claude", "claude-model", opts, auths)
	if err != nil {
		t.Fatalf("Pick() bound session error = %v", err)
	}
	if second.ID != "b" {
		t.Fatalf("Pick() bound session = %s, want bound credential b", second.ID)
	}
}

func registerSoonestResetManagerAuths(t *testing.T, manager *Manager, model string, ids ...string) {
	t.Helper()
	for _, id := range ids {
		registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: model}})
		authID := id
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })
		if _, errRegister := manager.Register(context.Background(), &Auth{ID: id, Provider: "codex", Status: StatusActive}); errRegister != nil {
			t.Fatal(errRegister)
		}
	}
}

func markSoonestResetObservation(manager *Manager, authID, model string, resetAfter time.Duration) {
	ctx := logging.WithResponseHeadersHolder(context.Background())
	logging.SetResponseHeaders(ctx, http.Header{
		"X-Codex-Secondary-Reset-After-Seconds": []string{strconv.FormatInt(int64(resetAfter/time.Second), 10)},
	})
	manager.MarkResult(ctx, Result{AuthID: authID, Provider: "codex", Model: model, Success: true})
}

func TestManagerSoonestResetSelectsFromObservedHeaders(t *testing.T) {
	const model = "soonest-reset-model"
	manager := NewManager(nil, &SoonestResetSelector{}, nil)
	manager.RegisterExecutor(&aliasRoutingExecutor{id: "codex"})
	prefix := "soonest-reset-" + t.Name() + "-"
	registerSoonestResetManagerAuths(t, manager, model, prefix+"a", prefix+"b", prefix+"c")

	selected, err := manager.SelectAuth(context.Background(), "codex", model, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if selected.ID != prefix+"a" {
		t.Fatalf("before any observation = %s, want first credential", selected.ID)
	}

	markSoonestResetObservation(manager, prefix+"a", model, 96*time.Hour)
	selected, err = manager.SelectAuth(context.Background(), "codex", model, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if selected.ID != prefix+"b" {
		t.Fatalf("with unobserved credentials left = %s, want next unobserved credential", selected.ID)
	}

	markSoonestResetObservation(manager, prefix+"b", model, 24*time.Hour)
	markSoonestResetObservation(manager, prefix+"c", model, 48*time.Hour)
	selected, err = manager.SelectAuth(context.Background(), "codex", model, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if selected.ID != prefix+"b" {
		t.Fatalf("all observed = %s, want soonest reset credential", selected.ID)
	}
}

// TestManagerSoonestResetConcurrentObservation exercises Pick while MarkResult
// replaces quota signals, so the race detector can see unsynchronised reads.
func TestManagerSoonestResetConcurrentObservation(t *testing.T) {
	const model = "soonest-reset-race-model"
	manager := NewManager(nil, NewSessionAffinitySelector(&SoonestResetSelector{}), nil)
	manager.RegisterExecutor(&aliasRoutingExecutor{id: "codex"})
	prefix := "soonest-reset-race-"
	registerSoonestResetManagerAuths(t, manager, model, prefix+"a", prefix+"b")

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			markSoonestResetObservation(manager, prefix+"a", model, time.Duration(i+1)*time.Minute)
			markSoonestResetObservation(manager, prefix+"b", model, time.Duration(200-i)*time.Minute)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			if _, err := manager.SelectAuth(context.Background(), "codex", model, cliproxyexecutor.Options{}); err != nil {
				t.Errorf("SelectAuth() error = %v", err)
				return
			}
		}
	}()
	wg.Wait()
}

// TestManagerSoonestResetUsesConductorAvailability checks that a bare
// soonest-reset selector sees the candidates the conductor validated against
// each credential's upstream model, exactly like fill-first, instead of
// re-checking them against the route alias.
func TestManagerSoonestResetUsesConductorAvailability(t *testing.T) {
	for name, newSelector := range map[string]func() Selector{
		"fill-first":    func() Selector { return &FillFirstSelector{} },
		"soonest-reset": func() Selector { return &SoonestResetSelector{} },
	} {
		t.Run(name, func(t *testing.T) {
			const routeModel, targetModel = "soonest-availability-route", "soonest-availability-target"
			manager := NewManager(nil, newSelector(), nil)
			manager.RegisterExecutor(&aliasRoutingExecutor{id: "codex"})
			manager.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{
				"codex": {{Name: targetModel, Alias: routeModel, Fork: true}},
			})
			next := time.Now().Add(time.Hour)
			auth := &Auth{
				ID:       "soonest-availability-" + name,
				Provider: "codex",
				Status:   StatusActive,
				ModelStates: map[string]*ModelState{
					targetModel: {Status: StatusActive},
					routeModel: {
						Status:         StatusError,
						Unavailable:    true,
						NextRetryAfter: next,
						Quota:          QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: next},
					},
				},
			}
			registry.GetGlobalRegistry().RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: routeModel}, {ID: targetModel}})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
			if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
				t.Fatal(errRegister)
			}
			selected, errSelect := manager.SelectAuth(context.Background(), "codex", routeModel, cliproxyexecutor.Options{})
			if errSelect != nil {
				t.Fatalf("SelectAuth() error = %v", errSelect)
			}
			if selected == nil || selected.ID != auth.ID {
				t.Fatalf("SelectAuth() = %v, want %s", selected, auth.ID)
			}
		})
	}
}
