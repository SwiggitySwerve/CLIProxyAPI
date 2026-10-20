package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestUltrafastAccountPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, provider, email, tier string
		allowed                     bool
	}{
		{"research", "codex", "research@astrabit.io", "ultrafast", true},
		{"normalized", "codex", " RESEARCH@astrabit.io ", " ULTRAFAST ", true},
		{"other account", "codex", "wrollings@astrabit.io", "ultrafast", false},
		{"missing identity", "codex", "", "ultrafast", false},
		{"other provider", "claude", "research@astrabit.io", "ultrafast", false},
		{"default unchanged", "codex", "wrollings@astrabit.io", "default", true},
		{"priority unchanged", "codex", "wrollings@astrabit.io", "priority", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &Auth{Provider: tc.provider, Metadata: map[string]any{"email": tc.email}}
			if err := ValidateServiceTierAccount(a, tc.tier); (err == nil) != tc.allowed {
				t.Fatalf("allowed=%t, error=%v", tc.allowed, err)
			}
		})
	}
	if ValidateServiceTierAccount(nil, "ultrafast") == nil {
		t.Fatal("missing credential must reject ultrafast")
	}
}

func TestUltrafastFinalPolicyFailureKeepsCredentialHealthy(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "http", true: "stream"}[streaming], func(t *testing.T) {
			model := "ultrafast-final-policy-" + uuid.NewString()
			m := NewManager(nil, &FillFirstSelector{}, nil)
			m.SetRetryConfig(0, 0, 0)
			other := &Auth{ID: "other-" + model, Provider: "codex", Metadata: map[string]any{"email": "wrollings@astrabit.io", "priority": 200}}
			research := &Auth{ID: "research-" + model, Provider: "codex", Metadata: map[string]any{"email": ultrafastAccountEmail, "priority": 100}}
			failure := ValidateServiceTierAccount(other, "ultrafast")
			e := &authFallbackExecutor{id: "codex", executeErrors: map[string]error{other.ID: failure}, streamFirstErrors: map[string]error{other.ID: failure}}
			m.RegisterExecutor(e)
			for _, a := range []*Auth{other, research} {
				if _, err := m.Register(context.Background(), a); err != nil {
					t.Fatal(err)
				}
				registry.GetGlobalRegistry().RegisterClient(a.ID, "codex", []*registry.ModelInfo{{ID: model}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(a.ID) })
			}
			var err error
			if streaming {
				var stream *cliproxyexecutor.StreamResult
				stream, err = m.ExecuteStream(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
				if stream != nil {
					for chunk := range stream.Chunks {
						if chunk.Err != nil {
							err = chunk.Err
						}
					}
				}
			} else {
				_, err = m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
			}
			if err == nil {
				t.Fatal("final policy failure must stop the request")
			}
			calls := e.ExecuteCalls()
			if streaming {
				calls = e.StreamCalls()
			}
			if len(calls) != 1 || calls[0] != other.ID {
				t.Fatalf("policy failure caused fallback: %v", calls)
			}
			updated, _ := m.GetByID(other.ID)
			if updated.Disabled || updated.Unavailable {
				t.Fatal("policy rejection suspended the credential")
			}
			if state := updated.ModelStates[model]; state != nil && state.Unavailable {
				t.Fatal("policy rejection put the model in cooldown")
			}
		})
	}
}

func TestUltrafastSelectionAndFallback(t *testing.T) {
	selectors := map[string]func() Selector{
		"fill-first":  func() Selector { return &FillFirstSelector{} },
		"round-robin": func() Selector { return &RoundRobinSelector{} },
		"affinity":    func() Selector { return NewSessionAffinitySelector(&FillFirstSelector{}) },
	}
	for name, makeSelector := range selectors {
		for _, streaming := range []bool{false, true} {
			for _, scenario := range []string{"eligible", "disabled", "missing", "cooldown", "pinned-other", "quota", "default"} {
				t.Run(name+"/"+scenario+"/"+map[bool]string{false: "http", true: "stream"}[streaming], func(t *testing.T) {
					model := "ultrafast-test-" + uuid.NewString()
					m := NewManager(nil, makeSelector(), nil)
					m.SetRetryConfig(0, 0, 0)
					e := &authFallbackExecutor{id: "codex"}
					m.RegisterExecutor(e)
					other := &Auth{ID: "other-" + model, Provider: "codex", Metadata: map[string]any{"email": "wrollings@astrabit.io", "priority": 200}}
					research := &Auth{ID: "research-" + model, Provider: "codex", Metadata: map[string]any{"email": ultrafastAccountEmail, "priority": 100}}
					if scenario == "disabled" {
						research.Disabled = true
					}
					if scenario == "cooldown" {
						research.ModelStates = map[string]*ModelState{model: {Unavailable: true, Status: StatusError, NextRetryAfter: time.Now().Add(time.Hour)}}
					}
					if scenario == "quota" {
						failure := &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota exhausted"}
						e.executeErrors = map[string]error{research.ID: failure}
						e.streamFirstErrors = map[string]error{research.ID: failure}
					}
					for _, a := range []*Auth{other, research} {
						if a == research && scenario == "missing" {
							continue
						}
						if _, err := m.Register(context.Background(), a); err != nil {
							t.Fatal(err)
						}
						registry.GetGlobalRegistry().RegisterClient(a.ID, "codex", []*registry.ModelInfo{{ID: model}})
						t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(a.ID) })
					}
					tier := "ultrafast"
					if scenario == "default" {
						tier = "default"
					}
					opts := cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.ServiceTierMetadataKey: tier}}
					if scenario == "pinned-other" {
						opts.Metadata[cliproxyexecutor.PinnedAuthMetadataKey] = other.ID
					}
					req := cliproxyexecutor.Request{Model: model}
					var err error
					if streaming {
						var stream *cliproxyexecutor.StreamResult
						stream, err = m.ExecuteStream(context.Background(), []string{"codex"}, req, opts)
						if stream != nil {
							for chunk := range stream.Chunks {
								if chunk.Err != nil {
									err = chunk.Err
								}
							}
						}
					} else {
						_, err = m.Execute(context.Background(), []string{"codex"}, req, opts)
					}
					wantSuccess := scenario == "eligible" || scenario == "default"
					if (err == nil) != wantSuccess {
						t.Fatalf("success=%t, error=%v", wantSuccess, err)
					}
					calls := e.ExecuteCalls()
					if streaming {
						calls = e.StreamCalls()
					}
					for _, id := range calls {
						if tier == "ultrafast" && id != research.ID {
							t.Fatalf("ultrafast escaped to %q", id)
						}
					}
					if wantSuccess && len(calls) == 0 {
						t.Fatal("successful request did not execute")
					}
					if !wantSuccess && scenario != "quota" && len(calls) != 0 {
						t.Fatalf("unavailable account executed: %v", calls)
					}
				})
			}
		}
	}
}
