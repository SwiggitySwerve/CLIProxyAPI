package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

// TestExecutionNeverRecordsModelLessResult pins that no Execute, ExecuteStream or ExecuteCount
// path records a failed Result without a model. The model-less branch of MarkResult has no
// live emitter, and several cooldown defects are reachable only through it; if this test starts
// failing, those defects become live.
func TestExecutionNeverRecordsModelLessResult(t *testing.T) {
	previous := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })

	type pathCase struct {
		name      string
		midStream bool // the executor returns a stream that fails after its first payload
		run       func(m *Manager, provider, model string) error
	}
	paths := []pathCase{
		{"Execute", false, func(m *Manager, provider, model string) error {
			_, err := m.Execute(context.Background(), []string{provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
			return err
		}},
		{"ExecuteStream", false, func(m *Manager, provider, model string) error {
			_, err := m.ExecuteStream(context.Background(), []string{provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
			return err
		}},
		{"ExecuteStreamMidStream", true, func(m *Manager, provider, model string) error {
			stream, err := m.ExecuteStream(context.Background(), []string{provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
			if err != nil {
				return err
			}
			// The wrapper records the failure before it forwards the error chunk and closes
			// the channel, so draining to closure makes the assertion deterministic.
			var chunkErr error
			for chunk := range stream.Chunks {
				if chunk.Err != nil {
					chunkErr = chunk.Err
				}
			}
			return chunkErr
		}},
		{"ExecuteCount", false, func(m *Manager, provider, model string) error {
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
					hook := &resultCaptureHook{}
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
							if !path.midStream {
								return nil, customStatusError{code: status, msg: "upstream refused"}
							}
							chunks := make(chan cliproxyexecutor.StreamChunk, 2)
							chunks <- cliproxyexecutor.StreamChunk{Payload: []byte(`data: {"type":"response.output_item.added"}`)}
							chunks <- cliproxyexecutor.StreamChunk{Err: customStatusError{code: status, msg: "upstream refused"}}
							close(chunks)
							return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
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

					results := hook.Results()
					failed := false
					for _, result := range results {
						failed = failed || !result.Success
					}
					if !failed {
						t.Fatal("no failed Result was recorded; the pin would be vacuous")
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
