package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

const (
	testUltrafastAccountID = "acct-ultrafast-0001"
	testOtherAccountID     = "acct-other-0002"
	testUltrafastEmail     = "ultrafast-holder@example.test"
	testOtherEmail         = "other-holder@example.test"
)

// useUltrafastPolicy installs a policy for one test and restores the
// fail-closed default afterwards.
func useUltrafastPolicy(t *testing.T, cfg internalconfig.UltrafastConfig) {
	t.Helper()
	SetUltrafastPolicy(cfg)
	t.Cleanup(func() { SetUltrafastPolicy(internalconfig.UltrafastConfig{}) })
}

func ultrafastTestAuth(id, accountID, email string) *Auth {
	return &Auth{ID: id, Provider: "codex", Metadata: map[string]any{"account_id": accountID, "email": email}}
}

func TestUltrafastAccountPolicy(t *testing.T) {
	byAccount := internalconfig.UltrafastConfig{AccountIDs: []string{testUltrafastAccountID}}
	for _, tc := range []struct {
		name     string
		policy   internalconfig.UltrafastConfig
		provider string
		id       string
		account  string
		email    string
		tier     string
		allowed  bool
	}{
		{"configured account id", byAccount, "codex", "a", testUltrafastAccountID, "", "ultrafast", true},
		{"normalized", internalconfig.UltrafastConfig{AccountIDs: []string{" ACCT-Ultrafast-0001 "}}, "codex", "a", testUltrafastAccountID, "", " ULTRAFAST ", true},
		{"other account", byAccount, "codex", "a", testOtherAccountID, "", "ultrafast", false},
		{"missing identity", byAccount, "codex", "a", "", "", "ultrafast", false},
		{"other provider", byAccount, "claude", "a", testUltrafastAccountID, "", "ultrafast", false},
		{"configured auth id", internalconfig.UltrafastConfig{AuthIDs: []string{"codex-ultrafast.json"}}, "codex", "codex-ultrafast.json", testOtherAccountID, "", "ultrafast", true},
		{"other auth id", internalconfig.UltrafastConfig{AuthIDs: []string{"codex-ultrafast.json"}}, "codex", "codex-other.json", testUltrafastAccountID, "", "ultrafast", false},
		{"configured email", internalconfig.UltrafastConfig{Emails: []string{testUltrafastEmail}}, "codex", "a", testOtherAccountID, " ULTRAFAST-holder@example.test ", "ultrafast", true},
		{"email ignored unless configured", byAccount, "codex", "a", testOtherAccountID, testUltrafastEmail, "ultrafast", false},
		{"unset denies account id", internalconfig.UltrafastConfig{}, "codex", "a", testUltrafastAccountID, testUltrafastEmail, "ultrafast", false},
		{"blank entries never match", internalconfig.UltrafastConfig{AccountIDs: []string{" "}, AuthIDs: []string{""}, Emails: []string{""}}, "codex", "", "", "", "ultrafast", false},
		{"default unchanged when unset", internalconfig.UltrafastConfig{}, "codex", "a", testOtherAccountID, "", "default", true},
		{"priority unchanged when unset", internalconfig.UltrafastConfig{}, "codex", "a", testOtherAccountID, "", "priority", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useUltrafastPolicy(t, tc.policy)
			a := &Auth{ID: tc.id, Provider: tc.provider, Metadata: map[string]any{"account_id": tc.account, "email": tc.email}}
			if err := ValidateServiceTierAccount(a, tc.tier); (err == nil) != tc.allowed {
				t.Fatalf("allowed=%t, error=%v", tc.allowed, err)
			}
		})
	}
	useUltrafastPolicy(t, byAccount)
	if ValidateServiceTierAccount(nil, "ultrafast") == nil {
		t.Fatal("missing credential must reject ultrafast")
	}
}

func TestUltrafastDenialIsRequestScoped403(t *testing.T) {
	for name, policy := range map[string]internalconfig.UltrafastConfig{
		"other credential": {AccountIDs: []string{testUltrafastAccountID}},
		"unset":            {},
	} {
		t.Run(name, func(t *testing.T) {
			useUltrafastPolicy(t, policy)
			err := ValidateServiceTierAccount(ultrafastTestAuth("x", testOtherAccountID, testOtherEmail), "ultrafast")
			var authErr *Error
			if !errors.As(err, &authErr) || authErr.Code != ErrorCodeRequestScoped || authErr.HTTPStatus != http.StatusForbidden {
				t.Fatalf("want request-scoped 403, got %#v", err)
			}
			if strings.Contains(err.Error(), testOtherAccountID) || strings.Contains(err.Error(), testUltrafastAccountID) {
				t.Fatalf("denial message leaks an identity: %v", err)
			}
		})
	}
}

func TestSetUltrafastPolicyReplacesPreviousPolicy(t *testing.T) {
	useUltrafastPolicy(t, internalconfig.UltrafastConfig{AccountIDs: []string{testUltrafastAccountID}})
	a := ultrafastTestAuth("x", testUltrafastAccountID, "")
	if ValidateServiceTierAccount(a, "ultrafast") != nil {
		t.Fatal("configured credential must be allowed")
	}
	SetUltrafastPolicy(internalconfig.UltrafastConfig{AccountIDs: []string{testOtherAccountID}})
	if ValidateServiceTierAccount(a, "ultrafast") == nil {
		t.Fatal("a reload must revoke the previous entitlement")
	}
}

func TestUltrafastFinalPolicyFailureKeepsCredentialHealthy(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "http", true: "stream"}[streaming], func(t *testing.T) {
			model := "ultrafast-final-policy-" + uuid.NewString()
			useUltrafastPolicy(t, internalconfig.UltrafastConfig{AccountIDs: []string{testUltrafastAccountID}})
			m := NewManager(nil, &FillFirstSelector{}, nil)
			m.SetRetryConfig(0, 0, 0)
			other := &Auth{ID: "other-" + model, Provider: "codex", Metadata: map[string]any{"account_id": testOtherAccountID, "priority": 200}}
			research := &Auth{ID: "research-" + model, Provider: "codex", Metadata: map[string]any{"account_id": testUltrafastAccountID, "priority": 100}}
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
			for _, scenario := range []string{"eligible", "disabled", "missing", "cooldown", "pinned-other", "quota", "default", "unset"} {
				t.Run(name+"/"+scenario+"/"+map[bool]string{false: "http", true: "stream"}[streaming], func(t *testing.T) {
					policy := internalconfig.UltrafastConfig{AccountIDs: []string{testUltrafastAccountID}}
					if scenario == "unset" {
						policy = internalconfig.UltrafastConfig{}
					}
					useUltrafastPolicy(t, policy)
					model := "ultrafast-test-" + uuid.NewString()
					m := NewManager(nil, makeSelector(), nil)
					m.SetRetryConfig(0, 0, 0)
					e := &authFallbackExecutor{id: "codex"}
					m.RegisterExecutor(e)
					other := &Auth{ID: "other-" + model, Provider: "codex", Metadata: map[string]any{"account_id": testOtherAccountID, "priority": 200}}
					research := &Auth{ID: "research-" + model, Provider: "codex", Metadata: map[string]any{"account_id": testUltrafastAccountID, "priority": 100}}
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
