package claude

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

// modelLessCountingExecutor counts every executor entry point so a test can assert zero calls.
type modelLessCountingExecutor struct {
	calls atomic.Int64
}

func (*modelLessCountingExecutor) Identifier() string { return "claude" }

func (e *modelLessCountingExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	e.calls.Add(1)
	return coreexecutor.Response{}, errors.New("unexpected execute")
}

func (e *modelLessCountingExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.calls.Add(1)
	return nil, errors.New("unexpected stream")
}

func (e *modelLessCountingExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *modelLessCountingExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	e.calls.Add(1)
	return coreexecutor.Response{}, errors.New("unexpected count")
}

func (e *modelLessCountingExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	e.calls.Add(1)
	return nil, errors.New("unexpected http request")
}

// TestClaudeMessagesWithoutModelIsRejectedBeforeExecution pins the invariant the cooldown
// logic relies on: a request with no model never reaches an executor, so no model-less
// Result can be recorded. See TestExecutionNeverRecordsModelLessResult in sdk/cliproxy/auth.
func TestClaudeMessagesWithoutModelIsRejectedBeforeExecution(t *testing.T) {
	gin.SetMode(gin.TestMode)
	executor := &modelLessCountingExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	const authID = "modelless-pin-auth"
	if _, errRegister := manager.Register(context.Background(), &coreauth.Auth{ID: authID, Provider: executor.Identifier(), Status: coreauth.StatusActive}); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	registry.GetGlobalRegistry().RegisterClient(authID, executor.Identifier(), []*registry.ModelInfo{{ID: "modelless-pin-model"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })

	handler := NewClaudeCodeAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
	router := gin.New()
	router.POST("/v1/messages", handler.ClaudeMessages)

	for _, test := range []struct {
		name string
		body string
	}{
		{name: "missing model", body: `{"messages":[{"role":"user","content":"hi"}]}`},
		{name: "empty model", body: `{"model":"","messages":[{"role":"user","content":"hi"}]}`},
		{name: "missing model streaming", body: `{"stream":true,"messages":[{"role":"user","content":"hi"}]}`},
		{name: "empty model streaming", body: `{"model":"","stream":true,"messages":[{"role":"user","content":"hi"}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(test.body))
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", recorder.Code, recorder.Body.String())
			}
			if got := executor.calls.Load(); got != 0 {
				t.Fatalf("executor calls = %d, want 0", got)
			}
		})
	}
}
