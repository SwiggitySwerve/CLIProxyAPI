package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// newBlockedPoolBesideCooledManager registers two OpenAI-compatible
// credentials for one alias pool. The pooled credential passes selection
// (its route model has no state) but every pool member is blocked, and the
// other credential is in a credential_quota cooldown.
func newBlockedPoolBesideCooledManager(t *testing.T) (*Manager, *openAICompatPoolExecutor, string) {
	t.Helper()
	alias := "no-silent-skip-" + t.Name()
	m := NewManager(nil, nil, nil)
	m.SetConfig(&internalconfig.Config{
		OpenAICompatibility: []internalconfig.OpenAICompatibility{{
			Name: "pool",
			Models: []internalconfig.OpenAICompatibilityModel{
				{Name: "deepseek-v3.1", Alias: alias},
				{Name: "glm-5", Alias: alias},
			},
		}},
	})
	executor := &openAICompatPoolExecutor{id: openAICompatPoolProviderKey}
	m.RegisterExecutor(executor)

	reg := registry.GetGlobalRegistry()
	for _, id := range []string{"aa-pool-blocked", "bb-cooled"} {
		auth := &Auth{
			ID:       id,
			Provider: openAICompatPoolProviderKey,
			Status:   StatusActive,
			Attributes: map[string]string{
				"api_key":      id + "-key",
				"compat_name":  "pool",
				"provider_key": openAICompatPoolProviderKey,
			},
		}
		if _, err := m.Register(WithSkipPersist(context.Background()), auth); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
		reg.RegisterClient(id, openAICompatPoolProviderKey, []*registry.ModelInfo{{ID: alias}})
		t.Cleanup(func() { reg.UnregisterClient(id) })
	}

	modelSupportErr := &Error{HTTPStatus: http.StatusBadRequest, Message: "invalid_request_error: The requested model is not supported."}
	for _, upstreamModel := range []string{"deepseek-v3.1", "glm-5"} {
		m.MarkResult(context.Background(), Result{AuthID: "aa-pool-blocked", Provider: openAICompatPoolProviderKey, Model: upstreamModel, Error: modelSupportErr})
	}
	retry := 100 * time.Hour
	m.MarkResult(context.Background(), Result{
		AuthID: "bb-cooled", Provider: openAICompatPoolProviderKey, Model: alias, CredentialScope: true, RetryAfter: &retry,
		Error: &Error{Code: ErrorCodeCredentialQuota, Message: "rate_limit_error", HTTPStatus: http.StatusTooManyRequests},
	})
	if got, _ := m.GetByID("bb-cooled"); got == nil || !got.Quota.Exceeded {
		t.Fatal("bb-cooled is not in a credential_quota cooldown")
	}
	return m, executor, alias
}

// A credential the attempt loop marks tried must have been sent the request
// or have recorded an error. A selected credential whose upstream models are
// all blocked used to be skipped with neither, so the client got the next
// pick's "all credentials cooling down" error, which ignored that credential.
func TestPickedCredentialWithoutExecutionModelsRecordsError(t *testing.T) {
	calls := map[string]func(*Manager, string) error{
		"execute": func(m *Manager, model string) error {
			_, err := m.Execute(context.Background(), []string{openAICompatPoolProviderKey}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
			return err
		},
		"count": func(m *Manager, model string) error {
			_, err := m.ExecuteCount(context.Background(), []string{openAICompatPoolProviderKey}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
			return err
		},
		"stream": func(m *Manager, model string) error {
			_, err := m.ExecuteStream(context.Background(), []string{openAICompatPoolProviderKey}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			m, executor, alias := newBlockedPoolBesideCooledManager(t)
			err := call(m, alias)
			if err == nil {
				t.Fatal("expected an error, every credential is blocked")
			}
			var cooldownErr *modelCooldownError
			if errors.As(err, &cooldownErr) {
				t.Fatalf("error = %v, want the pooled credential's own block, not the cooled credential's cooldown", err)
			}
			if !isAuthUnavailableError(err) || !strings.Contains(err.Error(), "not supported") {
				t.Fatalf("error = %v, want auth_unavailable carrying the pool's model-support error", err)
			}
			if got := len(executor.ExecuteModels()) + len(executor.CountModels()) + len(executor.StreamModels()); got != 0 {
				t.Fatalf("upstream calls = %d, want none", got)
			}
		})
	}
}
