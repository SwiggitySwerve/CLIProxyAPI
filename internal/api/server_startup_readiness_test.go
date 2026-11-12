package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStartupReadinessGateCoversOnlyProxyRoutes(t *testing.T) {
	server := newTestServerWithOptions(t, WithLocalManagementPassword("startup-readiness-local"))
	serve := func(method, path, body string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		server.engine.ServeHTTP(rr, req)
		return rr
	}

	server.SetAPIReady(false)
	for _, path := range []string{"/v1/messages", "/v1/chat/completions", "/v1beta/models/gemini-x:generateContent", "/openai/v1/videos", "/backend-api/codex/responses"} {
		rr := serve(http.MethodPost, path, `{"model":"m"}`)
		if rr.Code != http.StatusServiceUnavailable || rr.Header().Get("Retry-After") != "1" {
			t.Fatalf("not ready: POST %s = %d (Retry-After %q), want 503 with Retry-After 1", path, rr.Code, rr.Header().Get("Retry-After"))
		}
	}
	if rr := serve(http.MethodGet, "/healthz", ""); rr.Code != http.StatusOK {
		t.Fatalf("not ready: GET /healthz = %d, want 200", rr.Code)
	}
	if rr := serve(http.MethodHead, "/api/hello", ""); rr.Code != http.StatusOK {
		t.Fatalf("not ready: HEAD /api/hello = %d, want 200", rr.Code)
	}
	if rr := serve(http.MethodGet, "/v8/management/config", ""); rr.Code == http.StatusServiceUnavailable {
		t.Fatal("not ready: management route must not be gated")
	}

	server.SetAPIReady(true)
	if rr := serve(http.MethodPost, "/v1/messages", `{"model":"m"}`); rr.Code == http.StatusServiceUnavailable {
		t.Fatalf("ready: POST /v1/messages = 503, want the route to run")
	}
}
