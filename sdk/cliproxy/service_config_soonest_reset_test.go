package cliproxy

import (
	"context"
	"strconv"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestSoonestResetRoutingSelector(t *testing.T) {
	for _, input := range []string{"soonest-reset", "soonestreset", "sr", " Soonest-Reset "} {
		state := normalizedRoutingRuntimeState(&internalconfig.Config{
			Routing: internalconfig.RoutingConfig{Strategy: input},
		})
		if state.strategy != "soonest-reset" {
			t.Fatalf("strategy for %q = %q, want soonest-reset", input, state.strategy)
		}
		if selector, ok := newRoutingSelector(state).(*coreauth.SoonestResetSelector); !ok {
			t.Fatalf("selector type for %q = %T, want *auth.SoonestResetSelector", input, selector)
		}
	}
}

func TestSoonestResetRoutingSelectorWithSessionAffinity(t *testing.T) {
	state := normalizedRoutingRuntimeState(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{Strategy: "soonest-reset", SessionAffinity: true},
	})
	selector, ok := newRoutingSelector(state).(*coreauth.SessionAffinitySelector)
	if !ok {
		t.Fatalf("selector type = %T, want *auth.SessionAffinitySelector", newRoutingSelector(state))
	}
	defer selector.Stop()

	// The fallback is unexported, so check it by behaviour: fill-first would pick "a".
	now := time.Now()
	reset := func(d time.Duration) map[string]string {
		return map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": strconv.FormatInt(now.Add(d).Unix(), 10)}
	}
	auths := []*coreauth.Auth{
		{ID: "a", Provider: "claude", Quota: coreauth.QuotaState{ObservedAt: now, Signals: reset(72 * time.Hour)}},
		{ID: "b", Provider: "claude", Quota: coreauth.QuotaState{ObservedAt: now, Signals: reset(time.Hour)}},
	}
	got, err := selector.Pick(context.Background(), "claude", "claude-model", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if got.ID != "b" {
		t.Fatalf("Pick() = %s, want b from the soonest-reset fallback", got.ID)
	}
}

func TestApplyManagerConfigSwapsFillFirstToSoonestReset(t *testing.T) {
	service := &Service{coreManager: coreauth.NewManager(nil, nil, nil)}
	apply := func(strategy string, sequence uint64) {
		t.Helper()
		commit := configCommit{
			cfg:      &internalconfig.Config{Routing: internalconfig.RoutingConfig{Strategy: strategy}},
			sequence: sequence,
		}
		if !service.applyManagerConfig(context.Background(), commit) {
			t.Fatalf("applyManagerConfig(%s) failed", strategy)
		}
	}
	apply("fill-first", 1)
	if _, ok := service.coreManager.Selector().(*coreauth.FillFirstSelector); !ok {
		t.Fatalf("selector = %T, want *auth.FillFirstSelector", service.coreManager.Selector())
	}
	apply("soonest-reset", 2)
	if _, ok := service.coreManager.Selector().(*coreauth.SoonestResetSelector); !ok {
		t.Fatalf("selector after reload = %T, want *auth.SoonestResetSelector", service.coreManager.Selector())
	}
}
