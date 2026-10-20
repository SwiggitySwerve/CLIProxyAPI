package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestUltrafastTransportRejectsBeforeUpstream(t *testing.T) {
	for _, transport := range []string{"http", "http-stream", "websocket", "websocket-stream", "compact"} {
		for _, mode := range []string{"requested", "override", "header", "duplicate-last", "duplicate-first", "duplicate-escaped"} {
			t.Run(transport+"/"+mode, func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					w.WriteHeader(http.StatusInternalServerError)
				}))
				defer server.Close()
				credential := codexOAuthTestAuth(server.URL)
				credential.ID = "other-account"
				credential.Metadata["email"] = "wrollings@astrabit.io"
				cfg := &config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}}
				payload := []byte(`{"model":"gpt-5.5","service_tier":"ultrafast","input":"hi"}`)
				switch mode {
				case "duplicate-last":
					payload = []byte(`{"model":"gpt-5.5","service_tier":"priority","service_tier":"ultrafast","input":"hi"}`)
				case "duplicate-first":
					payload = []byte(`{"model":"gpt-5.5","service_tier":"ultrafast","service_tier":"priority","input":"hi"}`)
				case "duplicate-escaped":
					payload = []byte(`{"model":"gpt-5.5","service_tier":"priority","service_\u0074ier":"ultrafast","input":"hi"}`)
				}
				if mode == "override" {
					payload = []byte(`{"model":"gpt-5.5","input":"hi"}`)
					cfg.Payload = config.PayloadConfig{Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "*"}}, Params: map[string]any{"service_tier": "ultrafast"}}}}
				}
				if mode == "header" {
					payload = []byte(`{"model":"gpt-5.5","input":"hi"}`)
					credential.Attributes["header:X-Codex-Routing-Hint"] = "model=gpt-5.5;tier=ultrafast"
				}
				req := cliproxyexecutor.Request{Model: "gpt-5.5", Payload: payload}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")}
				var err error
				switch transport {
				case "http":
					_, err = NewCodexExecutor(cfg).Execute(context.Background(), credential, req, opts)
				case "http-stream":
					_, err = NewCodexExecutor(cfg).ExecuteStream(context.Background(), credential, req, opts)
				case "compact":
					opts.Alt = "responses/compact"
					_, err = NewCodexExecutor(cfg).Execute(context.Background(), credential, req, opts)
				case "websocket":
					_, err = NewCodexWebsocketsExecutor(cfg).Execute(context.Background(), credential, req, opts)
				case "websocket-stream":
					_, err = NewCodexWebsocketsExecutor(cfg).ExecuteStream(context.Background(), credential, req, opts)
				}
				if err == nil {
					t.Fatal("expected research-only policy rejection")
				}
				if calls.Load() != 0 {
					t.Fatalf("disallowed account reached upstream %d times", calls.Load())
				}
			})
		}
	}
}

func TestUltrafastWebsocketToggleRejectsBoundOtherAccount(t *testing.T) {
	var rec codexWebsocketHintRecorder
	server := newCodexWebsocketHintServer(t, &rec)
	defer server.Close()
	exec := NewCodexWebsocketsExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	credential := codexOAuthTestAuth(server.URL)
	credential.ID = "bound-other-account"
	credential.Metadata["email"] = "wrollings@astrabit.io"
	const sessionID = "ultrafast-bound-other"
	t.Cleanup(func() { exec.CloseExecutionSession(sessionID) })
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response"), Metadata: map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: sessionID}}
	if _, err := exec.Execute(context.Background(), credential, cliproxyexecutor.Request{Model: "gpt-5.5", Payload: []byte(`{"model":"gpt-5.5","input":"hi"}`)}, opts); err != nil {
		t.Fatal(err)
	}
	_, err := exec.Execute(context.Background(), credential, cliproxyexecutor.Request{Model: "gpt-5.5", Payload: []byte(`{"model":"gpt-5.5","service_tier":"ultrafast","previous_response_id":"resp-1","input":"again"}`)}, opts)
	if err == nil {
		t.Fatal("tier change on the existing other-account socket must fail")
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.frames) != 1 || len(rec.handshakes) != 1 {
		t.Fatalf("ultrafast escaped on a bound socket: frames=%d handshakes=%d", len(rec.frames), len(rec.handshakes))
	}
}
