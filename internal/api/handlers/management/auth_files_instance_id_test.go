package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func listCredentialsForInstanceIDTest(t *testing.T, v8 bool) []map[string]any {
	t.Helper()
	t.Setenv("MANAGEMENT_PASSWORD", "")
	cfg := &config.Config{AuthDir: t.TempDir()}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	for _, id := range []string{"a", "b"} {
		registerAuthForLookupTest(t, manager, &coreauth.Auth{
			ID: id, Index: "index-" + id, Provider: "codex", Status: coreauth.StatusActive,
			Attributes: map[string]string{"runtime_only": "true"},
		})
	}
	h := NewHandlerWithoutConfigFilePath(cfg, manager)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v8/management/credentials", nil)
	if v8 {
		ctx.Set(ConfigV8ContextKey, true)
	}
	h.ListAuthFiles(ctx)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Files []map[string]any `json:"files"`
	}
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &payload); errDecode != nil {
		t.Fatal(errDecode)
	}
	if len(payload.Files) != 2 {
		t.Fatalf("files = %+v", payload.Files)
	}
	return payload.Files
}

func TestListCredentialsV8ReportsInstanceID(t *testing.T) {
	t.Setenv("CLIPROXY_INSTANCE_ID", "claude-primary")
	for _, file := range listCredentialsForInstanceIDTest(t, true) {
		if file["instance_id"] != "claude-primary" {
			t.Fatalf("instance_id = %v, want claude-primary (entry=%+v)", file["instance_id"], file)
		}
	}
}

func TestListCredentialsV8OmitsInstanceIDWhenUnsetOrInvalid(t *testing.T) {
	for _, value := range []string{"", "Not Valid!"} {
		t.Setenv("CLIPROXY_INSTANCE_ID", value)
		for _, file := range listCredentialsForInstanceIDTest(t, true) {
			if _, present := file["instance_id"]; present {
				t.Fatalf("instance_id present for env %q: %+v", value, file)
			}
		}
	}
}

func TestListAuthFilesV0NeverReportsInstanceID(t *testing.T) {
	t.Setenv("CLIPROXY_INSTANCE_ID", "claude-primary")
	for _, file := range listCredentialsForInstanceIDTest(t, false) {
		if _, present := file["instance_id"]; present {
			t.Fatalf("v0 entry gained instance_id: %+v", file)
		}
	}
}
