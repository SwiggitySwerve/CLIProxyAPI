package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

// resultRecorderHook collects every Result the manager records.
type resultRecorderHook struct {
	mu      sync.Mutex
	results []Result
}

func (*resultRecorderHook) OnAuthRegistered(context.Context, *Auth) {}

func (*resultRecorderHook) OnAuthUpdated(context.Context, *Auth) {}

func (h *resultRecorderHook) OnResult(_ context.Context, result Result) {
	h.mu.Lock()
	h.results = append(h.results, result)
	h.mu.Unlock()
}

func (h *resultRecorderHook) recorded() []Result {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Result(nil), h.results...)
}

// TestExecutionNeverRecordsModelLessResult pins that no Execute, ExecuteStream or ExecuteCount
// path records a failed Result without a model. The model-less branch of MarkResult has no
// live emitter, and several cooldown defects are reachable only through it; if this test starts
// failing, those defects become live.
func TestExecutionNeverRecordsModelLessResult(t *testing.T) {
	previous := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })

	type pathCase struct {
		name string
		run  func(m *Manager, provider, model string) error
	}
	paths := []pathCase{
		{"Execute", func(m *Manager, provider, model string) error {
			_, err := m.Execute(context.Background(), []string{provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
			return err
		}},
		{"ExecuteStream", func(m *Manager, provider, model string) error {
			_, err := m.ExecuteStream(context.Background(), []string{provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
			return err
		}},
		{"ExecuteCount", func(m *Manager, provider, model string) error {
			_, err := m.ExecuteCount(context.Background(), []string{provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
			return err
		}},
	}

	n := 0
	for _, provider := range []string{"claude", "codex"} {
		for _, status := range []int{http.StatusForbidden, http.StatusTooManyRequests} {
			for _, path := range paths {
				n++
				provider, status, path, id := provider, status, path, fmt.Sprintf("auth-modelless-pin-%d", n)
				t.Run(fmt.Sprintf("%s/%s/%d", provider, path.name, status), func(t *testing.T) {
					model := "modelless-pin-model"
					hook := &resultRecorderHook{}
					m := NewManager(nil, nil, hook)
					failing := func(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
						return cliproxyexecutor.Response{}, customStatusError{code: status, msg: "upstream refused"}
					}
					m.RegisterExecutor(&customStreamMockExecutor{
						identifier: provider,
						mockCustomErrorExecutor: mockCustomErrorExecutor{
							executeFn: failing,
							countFn:   failing,
						},
						streamFn: func(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
							return nil, customStatusError{code: status, msg: "upstream refused"}
						},
					})
					reg := registry.GetGlobalRegistry()
					reg.RegisterClient(id, provider, []*registry.ModelInfo{{ID: model}})
					t.Cleanup(func() { reg.UnregisterClient(id) })
					if _, err := m.Register(context.Background(), &Auth{ID: id, Provider: provider, Status: StatusActive}); err != nil {
						t.Fatalf("register: %v", err)
					}

					if err := path.run(m, provider, model); err == nil {
						t.Fatal("expected the executor failure to surface")
					}

					results := hook.recorded()
					if len(results) == 0 {
						t.Fatal("no Result was recorded; the pin would be vacuous")
					}
					for i, result := range results {
						if result.Model == "" && result.RouteModel == "" {
							t.Fatalf("result %d recorded without Model or RouteModel: %+v", i, result)
						}
					}
				})
			}
		}
	}
}

// TestMarkResultWarnsOnModelLessFailure covers the model-less failure branch directly: it must
// log a warning carrying provider, auth and status in the message text, and a failure that
// carries a model must not.
func TestMarkResultWarnsOnModelLessFailure(t *testing.T) {
	hook := setupTestLoggerHook(t)
	m := NewManager(nil, nil, nil)
	const id = "auth-modelless-warn"
	if _, err := m.Register(context.Background(), &Auth{ID: id, Provider: "claude", Status: StatusActive}); err != nil {
		t.Fatalf("register: %v", err)
	}

	m.MarkResult(context.Background(), Result{AuthID: id, Provider: "claude", Model: "modelless-warn-model", Success: false, Error: &Error{HTTPStatus: http.StatusForbidden, Message: "denied"}})
	for _, entry := range hook.AllEntries() {
		if strings.Contains(entry.Message, "model-less failed result") {
			t.Fatalf("unexpected warning for a result with a model: %s", entry.Message)
		}
	}

	m.MarkResult(context.Background(), Result{AuthID: id, Provider: "claude", Success: false, Error: &Error{HTTPStatus: http.StatusForbidden, Message: "denied"}})
	for _, entry := range hook.AllEntries() {
		if entry.Level != log.WarnLevel || !strings.Contains(entry.Message, "model-less failed result") {
			continue
		}
		// The auth reference is the credential index when assigned, else the id.
		authRef := m.auths[id].Index
		if authRef == "" {
			authRef = id
		}
		for _, want := range []string{"provider=claude", "auth=" + authRef + " ", "status=403"} {
			if !strings.Contains(entry.Message, want) {
				t.Fatalf("warning %q is missing %q", entry.Message, want)
			}
		}
		return
	}
	t.Fatalf("expected a model-less warning, got: %#v", hook.AllEntries())
}
