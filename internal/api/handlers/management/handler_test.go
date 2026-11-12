package management

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
)

func TestAuthenticateManagementKey_LocalhostIPBan_BlocksCorrectKeyDuringBan(t *testing.T) {
	h := &Handler{
		cfg:            &config.Config{},
		failedAttempts: make(map[string]*attemptInfo),
		envSecret:      "test-secret",
	}

	for i := 0; i < 5; i++ {
		allowed, statusCode, errMsg := h.AuthenticateManagementKey("127.0.0.1", true, fmt.Sprintf("wrong-secret-%d", i))
		if allowed {
			t.Fatalf("expected auth to be denied at attempt %d", i+1)
		}
		if statusCode != http.StatusUnauthorized || errMsg != "invalid management key" {
			t.Fatalf("unexpected auth failure at attempt %d: status=%d msg=%q", i+1, statusCode, errMsg)
		}
	}

	allowed, statusCode, errMsg := h.AuthenticateManagementKey("127.0.0.1", true, "test-secret")
	if allowed {
		t.Fatalf("expected correct key to be denied while banned")
	}
	if statusCode != http.StatusForbidden {
		t.Fatalf("expected forbidden status while banned, got %d", statusCode)
	}
	if !strings.HasPrefix(errMsg, "IP banned due to too many failed attempts. Try again in") {
		t.Fatalf("unexpected banned message: %q", errMsg)
	}
}

func TestMiddlewareSetsSupportPluginHeader(t *testing.T) {

	h := &Handler{
		cfg:            &config.Config{},
		failedAttempts: make(map[string]*attemptInfo),
		envSecret:      "test-secret",
	}
	middleware := h.Middleware()

	t.Run("invalid key", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/config", nil)
		c.Request.RemoteAddr = "127.0.0.1:12345"
		c.Request.Header.Set("X-Management-Key", "wrong-secret")

		middleware(c)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
		}
		if got := rec.Header().Get("X-CPA-SUPPORT-PLUGIN"); got != pluginhost.SupportPluginHeaderValue() {
			t.Fatalf("X-CPA-SUPPORT-PLUGIN = %q, want %q", got, pluginhost.SupportPluginHeaderValue())
		}
	})

	t.Run("valid key", func(t *testing.T) {
		engine := gin.New()
		engine.GET("/v0/management/config", middleware, func(c *gin.Context) {
			c.Status(http.StatusOK)
		})

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v0/management/config", nil)
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("X-Management-Key", "test-secret")
		engine.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		if got := rec.Header().Get("X-CPA-SUPPORT-PLUGIN"); got != pluginhost.SupportPluginHeaderValue() {
			t.Fatalf("X-CPA-SUPPORT-PLUGIN = %q, want %q", got, pluginhost.SupportPluginHeaderValue())
		}
	})
}

// A client repeating one stale or missing key (a dashboard page firing nine
// requests with an old key; every host-side caller of a rootless podman
// container shares one source address) is not guessing. Each distinct wrong key
// counts once, so it cannot lock the correct key out; five distinct wrong keys
// still ban.
func TestAuthenticateManagementKey_RepeatedSameWrongKeyDoesNotBan(t *testing.T) {
	h := &Handler{
		cfg:            &config.Config{RemoteManagement: config.RemoteManagement{AllowRemote: true}},
		failedAttempts: make(map[string]*attemptInfo),
		envSecret:      "test-secret",
	}
	const host = "10.0.2.100"
	for i := 0; i < 9; i++ {
		if allowed, statusCode, errMsg := h.AuthenticateManagementKey(host, false, "stale-key"); allowed || statusCode != http.StatusUnauthorized {
			t.Fatalf("stale key attempt %d: allowed=%v status=%d msg=%q, want 401", i+1, allowed, statusCode, errMsg)
		}
	}
	for i := 0; i < 3; i++ {
		if allowed, statusCode, _ := h.AuthenticateManagementKey(host, false, ""); allowed || statusCode != http.StatusUnauthorized {
			t.Fatalf("missing key attempt %d: allowed=%v status=%d, want 401", i+1, allowed, statusCode)
		}
	}
	if allowed, statusCode, errMsg := h.AuthenticateManagementKey(host, false, "test-secret"); !allowed {
		t.Fatalf("correct key after repeated stale-key failures: status=%d msg=%q, want allowed", statusCode, errMsg)
	}

	for i := 0; i < 5; i++ {
		h.AuthenticateManagementKey(host, false, fmt.Sprintf("guess-%d", i))
	}
	if allowed, statusCode, errMsg := h.AuthenticateManagementKey(host, false, "test-secret"); allowed || statusCode != http.StatusForbidden || !strings.HasPrefix(errMsg, "IP banned") {
		t.Fatalf("after five distinct wrong keys: allowed=%v status=%d msg=%q, want the ban", allowed, statusCode, errMsg)
	}
}

// A repeated or missing key still takes one of the five slots: with a stale key
// and a missing key already counted, three distinct guesses complete the ban.
func TestAuthenticateManagementKey_RepeatedKeysShareTheFiveSlots(t *testing.T) {
	h := &Handler{
		cfg:            &config.Config{RemoteManagement: config.RemoteManagement{AllowRemote: true}},
		failedAttempts: make(map[string]*attemptInfo),
		envSecret:      "test-secret",
	}
	const host = "10.0.2.100"
	for i := 0; i < 9; i++ {
		h.AuthenticateManagementKey(host, false, "stale-key")
	}
	for i := 0; i < 3; i++ {
		h.AuthenticateManagementKey(host, false, "")
	}
	for i := 0; i < 3; i++ {
		h.AuthenticateManagementKey(host, false, fmt.Sprintf("guess-%d", i))
	}
	if allowed, statusCode, errMsg := h.AuthenticateManagementKey(host, false, "test-secret"); allowed || statusCode != http.StatusForbidden || !strings.HasPrefix(errMsg, "IP banned") {
		t.Fatalf("after stale + missing + three guesses: allowed=%v status=%d msg=%q, want the ban", allowed, statusCode, errMsg)
	}
}
