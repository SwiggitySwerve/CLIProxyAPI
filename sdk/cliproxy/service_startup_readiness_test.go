package cliproxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	internalregistry "github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type startupReadinessStore struct {
	auths []*coreauth.Auth
}

func (s *startupReadinessStore) List(context.Context) ([]*coreauth.Auth, error) {
	out := make([]*coreauth.Auth, 0, len(s.auths))
	for _, a := range s.auths {
		out = append(out, a.Clone())
	}
	return out, nil
}

func (s *startupReadinessStore) Save(_ context.Context, a *coreauth.Auth) (string, error) {
	return a.ID, nil
}

func (s *startupReadinessStore) Delete(context.Context, string) error { return nil }

// TestServiceRun_APIRequestsDuringInitialLoadAreRetryable holds the initial auth
// load open (the watcher Start call blocks) and checks that API traffic gets a
// retryable 503 instead of an "unknown provider" 400, that /healthz stays up, and
// that the same request is served once the load finishes.
func TestServiceRun_APIRequestsDuringInitialLoadAreRetryable(t *testing.T) {
	const (
		clientAPIKey = "startup-readiness-client-key"
		modelAlias   = "claude-startup-readiness"
	)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_startup","type":"message","role":"assistant","model":"claude-startup-upstream","content":[{"type":"text","text":"ready-ok"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	t.Cleanup(upstream.Close)

	ln, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatalf("failed to listen: %v", errListen)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if errClose := ln.Close(); errClose != nil {
		t.Fatalf("failed to close listener: %v", errClose)
	}

	authDir := t.TempDir()
	cfgPath := filepath.Join(authDir, "config.yaml")
	if errWrite := os.WriteFile(cfgPath, []byte("{}"), 0o600); errWrite != nil {
		t.Fatalf("write dummy config: %v", errWrite)
	}
	claudeModels := internalregistry.GetClaudeModels()
	if len(claudeModels) == 0 {
		t.Fatal("no static Claude models")
	}
	cfg := &config.Config{
		SDKConfig: config.SDKConfig{APIKeys: []string{clientAPIKey}},
		Host:      "127.0.0.1",
		Port:      port,
		AuthDir:   authDir,
		// The alias exists only for this test's credential, so leftover
		// registrations from other tests cannot make it routable early.
		OAuthModelAlias: map[string][]config.OAuthModelAlias{
			"claude": {{Name: claudeModels[0].ID, Alias: modelAlias, Fork: true}},
		},
	}
	// A stored OAuth credential, like the auth files in production. Unlike
	// config API keys, its models are only registered by the initial load.
	store := &startupReadinessStore{auths: []*coreauth.Auth{{
		ID:       "startup-readiness-claude.json",
		Provider: "claude",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"auth_kind": "oauth",
			"api_key":   "sk-startup-readiness-upstream",
			"base_url":  upstream.URL,
		},
	}}}

	releaseLoad := make(chan struct{})
	loadStarted := make(chan struct{})
	watcherFactory := func(configPath, dir string, reload func(*config.Config)) (*WatcherWrapper, error) {
		return &WatcherWrapper{
			start: func(ctx context.Context) error {
				close(loadStarted)
				select {
				case <-releaseLoad:
				case <-ctx.Done():
				}
				return nil
			},
			stop:                func() error { return nil },
			setConfig:           func(*config.Config) {},
			setPluginAuthParser: func(PluginAuthParser) {},
		}, nil
	}

	service, errBuild := NewBuilder().
		WithConfig(cfg).
		WithConfigPath(cfgPath).
		WithWatcherFactory(watcherFactory).
		WithCoreAuthManager(coreauth.NewManager(store, nil, nil)).
		Build()
	if errBuild != nil {
		t.Fatalf("Build() failed: %v", errBuild)
	}

	reg := internalregistry.GetGlobalRegistry()
	runCtx, cancel := context.WithCancel(context.Background())
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- service.Run(runCtx) }()
	released := false
	t.Cleanup(func() {
		if !released {
			close(releaseLoad)
		}
		cancel()
		select {
		case errRun := <-runErrCh:
			if errRun != nil && !errors.Is(errRun, context.Canceled) {
				t.Errorf("unexpected Run error: %v", errRun)
			}
		case <-time.After(10 * time.Second):
			t.Errorf("timed out waiting for Run to exit")
		}
		for _, a := range service.coreManager.List() {
			if a != nil {
				reg.UnregisterClient(a.ID)
			}
		}
	})

	select {
	case <-loadStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the initial auth load to start")
	}

	baseURL := "http://127.0.0.1:" + strconv.Itoa(port)
	client := &http.Client{}
	healthz := func() int {
		resp, errGet := client.Get(baseURL + "/healthz")
		if errGet != nil {
			return 0
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	// The listener opens before the initial load; wait until it accepts.
	deadline := time.Now().Add(10 * time.Second)
	for healthz() != http.StatusOK {
		if time.Now().After(deadline) {
			t.Fatal("/healthz never returned 200 while the initial load was running")
		}
		time.Sleep(10 * time.Millisecond)
	}

	postMessages := func() (int, http.Header, string) {
		body := `{"model":"` + modelAlias + `","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
		req, errReq := http.NewRequest(http.MethodPost, baseURL+"/v1/messages?beta=true", bytes.NewBufferString(body))
		if errReq != nil {
			t.Fatalf("build request: %v", errReq)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", clientAPIKey)
		req.Header.Set("anthropic-version", "2023-06-01")
		resp, errDo := client.Do(req)
		if errDo != nil {
			t.Fatalf("POST /v1/messages: %v", errDo)
		}
		defer func() { _ = resp.Body.Close() }()
		respBody, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header, string(respBody)
	}

	status, header, body := postMessages()
	if status != http.StatusServiceUnavailable {
		t.Fatalf("during initial load: status = %d, want 503; body = %s", status, body)
	}
	if got := header.Get("Retry-After"); got != "1" {
		t.Fatalf("during initial load: Retry-After = %q, want \"1\"", got)
	}
	if got := healthz(); got != http.StatusOK {
		t.Fatalf("during initial load: /healthz = %d, want 200", got)
	}

	close(releaseLoad)
	released = true

	deadline = time.Now().Add(10 * time.Second)
	for {
		status, _, body = postMessages()
		if status != http.StatusServiceUnavailable {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("API still not ready after the initial load finished; body = %s", body)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if status != http.StatusOK || !strings.Contains(body, "ready-ok") {
		t.Fatalf("after initial load: status = %d, body = %s; want 200 from upstream", status, body)
	}
}
