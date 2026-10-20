package executor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestUltrafastGenericHTTPGuardPreservesBody(t *testing.T) {
	for _, replayReader := range []bool{false, true} {
		for _, scenario := range []string{"reject-body", "reject-header", "reject-stale-replay", "allow-research", "allow-default"} {
			t.Run(scenario+map[bool]string{false: "/no-replay", true: "/replay"}[replayReader], func(t *testing.T) {
				var calls atomic.Int32
				payload := []byte(`{"service_tier":"ultrafast"}`)
				if scenario == "reject-header" || scenario == "allow-default" {
					payload = []byte(`{"service_tier":"default"}`)
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					body, err := io.ReadAll(r.Body)
					if err != nil || !bytes.Equal(body, payload) {
						t.Errorf("forwarded body changed: %s error=%v", body, err)
					}
					w.WriteHeader(http.StatusOK)
				}))
				defer server.Close()
				credential := codexOAuthTestAuth(server.URL)
				credential.Metadata["email"] = "wrollings@astrabit.io"
				if scenario == "allow-research" {
					credential.Metadata["email"] = "research@astrabit.io"
				}
				req, err := http.NewRequest(http.MethodPost, server.URL+"/responses", bytes.NewReader(payload))
				if err != nil {
					t.Fatal(err)
				}
				if !replayReader {
					req.GetBody = nil
				}
				if scenario == "reject-stale-replay" {
					req.GetBody = func() (io.ReadCloser, error) {
						return io.NopCloser(bytes.NewReader([]byte(`{"service_tier":"default"}`))), nil
					}
				}
				if scenario == "reject-header" {
					req.Header.Set("X-Codex-Routing-Hint", "model=gpt-5.5;tier=ultrafast")
				}
				resp, err := NewCodexExecutor(&config.Config{}).HttpRequest(context.Background(), credential, req)
				if resp != nil {
					resp.Body.Close()
				}
				if scenario == "reject-body" || scenario == "reject-header" || scenario == "reject-stale-replay" {
					var policy *cliproxyauth.Error
					if !errors.As(err, &policy) || !policy.IsRequestScoped() {
						t.Fatalf("expected policy error, got %v", err)
					}
					if calls.Load() != 0 {
						t.Fatal("generic request reached disallowed upstream")
					}
					body, readErr := io.ReadAll(req.Body)
					if readErr != nil || !bytes.Equal(body, payload) {
						t.Fatalf("rejected request body not preserved: %s error=%v", body, readErr)
					}
				} else if err != nil || calls.Load() != 1 {
					t.Fatalf("allowed forwarding failed: calls=%d error=%v", calls.Load(), err)
				}
			})
		}
	}
}

func TestUltrafastDirectImageModelHeaderOverride(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "http", true: "stream"}[streaming], func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) }))
			defer server.Close()
			credential := codexOAuthTestAuth(server.URL)
			credential.ID = "image-header-policy"
			credential.Metadata["email"] = "wrollings@astrabit.io"
			const model = "gpt-image-1.5"
			reg := registry.GetGlobalRegistry()
			reg.RegisterClient(credential.ID, "codex", []*registry.ModelInfo{{ID: model, Config: &registry.ModelConfig{OverrideHeader: map[string]string{"X-Codex-Routing-Hint": "model=gpt-image-1.5;tier=ultrafast"}}}})
			t.Cleanup(func() { reg.UnregisterClient(credential.ID) })
			req := cliproxyexecutor.Request{Model: model, Payload: []byte(`{"model":"gpt-image-1.5","prompt":"synthetic test"}`)}
			opts := codexOpenAIImageTestOptions(codexImagesGenerationsPath, streaming)
			exec := NewCodexExecutor(&config.Config{})
			var err error
			if streaming {
				_, err = exec.ExecuteStream(context.Background(), credential, req, opts)
			} else {
				_, err = exec.Execute(context.Background(), credential, req, opts)
			}
			var policy *cliproxyauth.Error
			if !errors.As(err, &policy) || !policy.IsRequestScoped() {
				t.Fatalf("expected policy error, got %v", err)
			}
			if calls.Load() != 0 {
				t.Fatal("image model header override escaped to other account")
			}
		})
	}
}

func TestUltrafastDuplexSteerRejectsBoundOtherAccount(t *testing.T) {
	for _, override := range []bool{false, true} {
		t.Run(map[bool]string{false: "requested", true: "override"}[override], func(t *testing.T) {
			var forwarded atomic.Int32
			upstreamDone := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(upstreamDone)
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					t.Errorf("upgrade: %v", err)
					return
				}
				defer conn.Close()
				if _, _, err := conn.ReadMessage(); err != nil {
					t.Errorf("initial request: %v", err)
					return
				}
				if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"r1","output":[]}}`)); err != nil {
					t.Errorf("created: %v", err)
					return
				}
				if _, _, err := conn.ReadMessage(); err == nil {
					forwarded.Add(1)
				}
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			input := make(chan cliproxyexecutor.WebsocketInput, 1)
			ctx = cliproxyexecutor.WithWebsocketInput(cliproxyexecutor.WithDownstreamWebsocket(ctx), input)
			cfg := &config.Config{Codex: config.CodexConfig{ResponseSteering: true}, SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}}
			if override {
				cfg.Payload = config.PayloadConfig{Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "*", Match: []map[string]any{{"type": "response.steer"}}}}, Params: map[string]any{"service_tier": "ultrafast"}}}}
			}
			exec := NewCodexWebsocketsExecutor(cfg)
			exec.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
			credential := codexOAuthTestAuth(server.URL)
			credential.ID = "duplex-other"
			credential.Attributes["websockets"] = "true"
			credential.Metadata["email"] = "wrollings@astrabit.io"
			const session = "ultrafast-duplex-test"
			t.Cleanup(func() { exec.CloseExecutionSession(session) })
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("codex"), Metadata: map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: session}}
			result, err := exec.ExecuteStream(ctx, credential, cliproxyexecutor.Request{Model: "gpt-6-astra", Payload: []byte(`{"model":"gpt-6-astra","input":[],"instructions":"synthetic"}`)}, opts)
			if err != nil {
				t.Fatal(err)
			}
			var rejected bool
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					var policy *cliproxyauth.Error
					if errors.As(chunk.Err, &policy) && policy.IsRequestScoped() {
						rejected = true
					}
					break
				}
				if bytes.Contains(chunk.Payload, []byte(`"response.created"`)) {
					payload := []byte(`{"type":"response.steer","previous_response_id":"r1","input":"synthetic","service_tier":"ultrafast"}`)
					if override {
						payload = []byte(`{"type":"response.steer","previous_response_id":"r1","input":"synthetic"}`)
					}
					input <- cliproxyexecutor.WebsocketInput{Payload: payload}
				}
			}
			cancel()
			exec.CloseExecutionSession(session)
			select {
			case <-upstreamDone:
			case <-time.After(5 * time.Second):
				t.Fatal("upstream socket did not close")
			}
			if !rejected || forwarded.Load() != 0 {
				t.Fatalf("steer policy: rejected=%t forwarded=%d", rejected, forwarded.Load())
			}
		})
	}
}
