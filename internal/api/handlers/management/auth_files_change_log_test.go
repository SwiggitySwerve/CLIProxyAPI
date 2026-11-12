package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func setupInfoLogHook(t *testing.T) *logtest.Hook {
	_, hook := logtest.NewNullLogger()
	oldLevel := log.GetLevel()
	log.SetLevel(log.InfoLevel)
	savedHooks := make(log.LevelHooks)
	for lvl, hs := range log.StandardLogger().Hooks {
		savedHooks[lvl] = append([]log.Hook(nil), hs...)
	}
	log.AddHook(hook)
	t.Cleanup(func() {
		log.SetLevel(oldLevel)
		log.StandardLogger().ReplaceHooks(savedHooks)
	})
	return hook
}

func credentialChangeMessages(hook *logtest.Hook, prefix string) []string {
	var out []string
	for _, entry := range hook.AllEntries() {
		if entry.Level == log.InfoLevel && strings.HasPrefix(entry.Message, prefix) {
			out = append(out, entry.Message)
		}
	}
	return out
}

func newChangeLogHandler(t *testing.T) (*Handler, string) {
	t.Helper()
	t.Setenv("MANAGEMENT_PASSWORD", "")
	authDir := t.TempDir()
	filePath := filepath.Join(authDir, "claude-a.json")
	if err := os.WriteFile(filePath, []byte(`{"type":"claude"}`), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID:         "claude-a.json",
		FileName:   "claude-a.json",
		Provider:   "claude",
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"path": filePath},
		Metadata:   map[string]any{"type": "claude", "priority": float64(50)},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	registered, _ := manager.GetByID("claude-a.json")
	return NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager), registered.EnsureIndex()
}

func patchCredential(t *testing.T, handler gin.HandlerFunc, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPatch, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req
	handler(ctx)
	return rec
}

func TestPatchAuthFileStatusLogsCredentialChange(t *testing.T) {
	h, index := newChangeLogHandler(t)
	hook := setupInfoLogHook(t)

	rec := patchCredential(t, h.PatchAuthFileStatus, "/v8/management/credentials/status", `{"name":"claude-a.json","disabled":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	want := "credential status changed: auth_index=" + index + " provider=claude disabled=true (was false)"
	got := credentialChangeMessages(hook, "credential status changed")
	if len(got) != 1 || got[0] != want {
		t.Fatalf("log lines = %q, want [%q]", got, want)
	}

	patchCredential(t, h.PatchAuthFileStatus, "/v8/management/credentials/status", `{"name":"claude-a.json","disabled":false}`)
	want = "credential status changed: auth_index=" + index + " provider=claude disabled=false (was true)"
	got = credentialChangeMessages(hook, "credential status changed")
	if len(got) != 2 || got[1] != want {
		t.Fatalf("log lines = %q, want second %q", got, want)
	}
}

func TestPatchAuthFileStatusLogsNothingOnFailure(t *testing.T) {
	h, _ := newChangeLogHandler(t)
	hook := setupInfoLogHook(t)

	rec := patchCredential(t, h.PatchAuthFileStatus, "/v8/management/credentials/status", `{"name":"missing.json","disabled":true}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if got := credentialChangeMessages(hook, "credential status changed"); len(got) != 0 {
		t.Fatalf("unexpected log lines on failure: %q", got)
	}
}

func TestPatchAuthFileFieldsLogsPriorityChangeWithoutLeakingValues(t *testing.T) {
	h, index := newChangeLogHandler(t)
	hook := setupInfoLogHook(t)

	body := `{"name":"claude-a.json","priority":90,"proxy_url":"http://user:secret@proxy.local","headers":{"X-Api-Key":"sk-ant-leak"}}`
	rec := patchCredential(t, h.PatchAuthFileFields, "/v8/management/credentials/fields", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	want := "credential fields changed: auth_index=" + index + " provider=claude headers=(redacted) priority=90 (was 50) proxy_url=(redacted)"
	got := credentialChangeMessages(hook, "credential fields changed")
	if len(got) != 1 || got[0] != want {
		t.Fatalf("log lines = %q, want [%q]", got, want)
	}
	for _, entry := range hook.AllEntries() {
		if strings.Contains(entry.Message, "secret") || strings.Contains(entry.Message, "sk-ant-leak") {
			t.Fatalf("log leaked a value: %q", entry.Message)
		}
	}
}

func TestPatchAuthFileFieldsLogsNothingOnFailure(t *testing.T) {
	h, _ := newChangeLogHandler(t)
	hook := setupInfoLogHook(t)

	rec := patchCredential(t, h.PatchAuthFileFields, "/v8/management/credentials/fields", `{"name":"claude-a.json","weight":"high"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 body=%s", rec.Code, rec.Body.String())
	}
	if got := credentialChangeMessages(hook, "credential fields changed"); len(got) != 0 {
		t.Fatalf("unexpected log lines on failure: %q", got)
	}
}
