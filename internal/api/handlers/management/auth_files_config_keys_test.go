package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

const (
	configKeySecretClaude = "sk-ant-api03-SECRET-claude-key-material"
	configKeySecretCodex  = "sk-codex-SECRET-codex-key-material"
)

// configKeyListingFixture registers one file-backed codex credential, one codex
// config API key with identical runtime state, and one claude config API key.
func configKeyListingFixture(t *testing.T) (*Handler, *coreauth.Manager, time.Time) {
	t.Helper()
	t.Setenv("MANAGEMENT_PASSWORD", "")
	now := time.Now().UTC()
	next := now.Add(time.Hour)
	cfg := &config.Config{AuthDir: t.TempDir()}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)

	path := filepath.Join(cfg.AuthDir, "codex-file.json")
	if errWrite := os.WriteFile(path, []byte(`{"type":"codex"}`), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	cooling := func(auth *coreauth.Auth) *coreauth.Auth {
		auth.Status = coreauth.StatusError
		auth.Unavailable = true
		auth.NextRetryAfter = next
		auth.Quota = coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: next, ObservedAt: now, Signals: map[string]string{"x-codex-primary-used-percent": "90"}}
		auth.ModelStates = map[string]*coreauth.ModelState{
			"model-a": {Unavailable: true, NextRetryAfter: next, Quota: coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: next, ObservedAt: now, Signals: map[string]string{"x-codex-primary-used-percent": "90"}}},
		}
		return auth
	}
	registerAuthForLookupTest(t, manager, cooling(&coreauth.Auth{
		ID: "codex-file.json", Index: "index-file", FileName: "codex-file.json", Provider: "codex",
		Attributes: map[string]string{"path": path, "priority": "7"},
	}))
	registerAuthForLookupTest(t, manager, cooling(&coreauth.Auth{
		ID: "codex:apikey:aaaa", Index: "index-codex-key", Provider: "codex", Label: "codex-apikey",
		Attributes: map[string]string{
			"source": "config:codex[aaaa]", "config_index": "0", "api_key": configKeySecretCodex,
			"base_url": "https://codex.example.test", "priority": "7",
		},
	}))
	registerAuthForLookupTest(t, manager, &coreauth.Auth{
		ID: "claude:apikey:bbbb", Index: "index-claude-key", Provider: "claude", Label: "claude-apikey", Status: coreauth.StatusActive,
		Attributes: map[string]string{
			"source": "config:claude[bbbb]", "config_index": "0", "api_key": configKeySecretClaude,
			"base_url": "https://claude.example.test",
		},
	})
	return NewHandlerWithoutConfigFilePath(cfg, manager), manager, next
}

func listCredentialsRaw(t *testing.T, h *Handler, query string, v8 bool) (map[string]map[string]any, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/management/credentials"+query, nil)
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
	byID := make(map[string]map[string]any, len(payload.Files))
	for _, file := range payload.Files {
		byID[file["id"].(string)] = file
	}
	return byID, rec.Body.String()
}

func TestListCredentialsV8IncludesConfigAPIKeys(t *testing.T) {
	h, _, next := configKeyListingFixture(t)
	for _, query := range []string{"", "?page=1&page_size=10"} {
		files, _ := listCredentialsRaw(t, h, query, true)
		if len(files) != 3 {
			t.Fatalf("query %q: got %d entries, want 3: %+v", query, len(files), files)
		}
		if files["codex-file.json"]["kind"] != "auth_file" {
			t.Fatalf("query %q: file kind = %v", query, files["codex-file.json"]["kind"])
		}
		for _, id := range []string{"codex:apikey:aaaa", "claude:apikey:bbbb"} {
			entry := files[id]
			if entry == nil {
				t.Fatalf("query %q: config key %s missing", query, id)
			}
			if entry["kind"] != "config_api_key" || entry["source"] != "config" || entry["runtime_only"] != false {
				t.Fatalf("query %q: %s identity fields = kind %v source %v runtime_only %v", query, id, entry["kind"], entry["source"], entry["runtime_only"])
			}
			if entry["account_type"] != "api_key" {
				t.Fatalf("query %q: %s account_type = %v", query, id, entry["account_type"])
			}
		}

		// Runtime state of the config key must be reported exactly like the file-backed twin.
		file, key := files["codex-file.json"], files["codex:apikey:aaaa"]
		for _, field := range []string{"status", "status_message", "unavailable", "next_retry_after", "quota", "model_quotas", "priority", "cooldowns", "provider", "type"} {
			if !reflect.DeepEqual(file[field], key[field]) {
				t.Fatalf("query %q: field %s differs: file=%#v config=%#v", query, field, file[field], key[field])
			}
		}
		if key["status"] != string(coreauth.StatusError) || key["unavailable"] != true || key["priority"] != float64(7) {
			t.Fatalf("query %q: unexpected state %+v", query, key)
		}
		if got, _ := key["next_retry_after"].(string); got == "" {
			t.Fatalf("query %q: next_retry_after missing", query)
		} else if parsed, errParse := time.Parse(time.RFC3339Nano, got); errParse != nil || !parsed.Equal(next) {
			t.Fatalf("query %q: next_retry_after = %q, want %s", query, got, next)
		}
	}
}

func TestListCredentialsV8ConfigAPIKeysExposeNoKeyMaterial(t *testing.T) {
	h, _, _ := configKeyListingFixture(t)
	for _, query := range []string{"", "?page=1&page_size=10", "?name=claude:apikey:bbbb"} {
		_, body := listCredentialsRaw(t, h, query, true)
		for _, forbidden := range []string{configKeySecretClaude, configKeySecretCodex, "SECRET", "api_key\":\"", "api-key"} {
			if strings.Contains(body, forbidden) {
				t.Fatalf("query %q: listing leaked %q: %s", query, forbidden, body)
			}
		}
	}
	files, _ := listCredentialsRaw(t, h, "", true)
	for _, id := range []string{"codex:apikey:aaaa", "claude:apikey:bbbb"} {
		if account, present := files[id]["account"]; present {
			t.Fatalf("%s exposes account %v", id, account)
		}
	}
}

func TestListAuthFilesV0StillHidesConfigAPIKeys(t *testing.T) {
	h, _, _ := configKeyListingFixture(t)
	files, body := listCredentialsRaw(t, h, "", false)
	if len(files) != 1 || files["codex-file.json"] == nil {
		t.Fatalf("v0 listing changed: %s", body)
	}
	if _, present := files["codex-file.json"]["kind"]; present {
		t.Fatalf("v0 entry gained kind: %+v", files["codex-file.json"])
	}
	files, _ = listCredentialsRaw(t, h, "?page=1&page_size=10", false)
	if len(files) != 1 {
		t.Fatalf("v0 paginated listing changed: %+v", files)
	}
}

// Config API keys are managed through the config endpoints. The file-oriented
// mutations keep refusing them with 404 and must not touch the credential.
func TestCredentialFileMutationsRejectConfigAPIKeys(t *testing.T) {
	h, manager, _ := configKeyListingFixture(t)
	router := gin.New()
	router.PATCH("/credentials/fields", h.PatchAuthFileFields)
	router.DELETE("/credentials", h.DeleteAuthFile)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/credentials/fields", strings.NewReader(`{"name":"claude:apikey:bbbb","priority":9}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("PATCH fields status = %d, want 404: %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/credentials?name=claude:apikey:bbbb", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("DELETE status = %d, want 404: %s", rec.Code, rec.Body.String())
	}

	auth, ok := manager.GetByID("claude:apikey:bbbb")
	if !ok || auth.Attributes["priority"] != "" || auth.Attributes["api_key"] != configKeySecretClaude {
		t.Fatalf("config key was modified or removed: ok=%v attrs=%+v", ok, auth)
	}
}

// File-backed credentials whose secret is an API key must not echo the key in
// the `account` field (AccountInfo returns the raw key for API-key auths).
func TestListCredentialsFileBackedAPIKeyExposesNoKeyMaterial(t *testing.T) {
	const fileKeySecret = "sk-file-SECRET-apikey-material"
	t.Setenv("MANAGEMENT_PASSWORD", "")
	cfg := &config.Config{AuthDir: t.TempDir()}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	path := filepath.Join(cfg.AuthDir, "claude-key.json")
	if errWrite := os.WriteFile(path, []byte(`{"type":"claude"}`), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	registerAuthForLookupTest(t, manager, &coreauth.Auth{
		ID: "claude-key.json", Index: "index-file-key", FileName: "claude-key.json", Provider: "claude", Status: coreauth.StatusActive,
		Attributes: map[string]string{"path": path, "api_key": fileKeySecret},
	})
	h := NewHandlerWithoutConfigFilePath(cfg, manager)

	for _, v8 := range []bool{true, false} {
		for _, query := range []string{"", "?page=1&page_size=10"} {
			files, body := listCredentialsRaw(t, h, query, v8)
			if strings.Contains(body, fileKeySecret) || strings.Contains(body, "SECRET") {
				t.Fatalf("v8=%v query %q: listing leaked the key: %s", v8, query, body)
			}
			entry := files["claude-key.json"]
			if entry == nil {
				t.Fatalf("v8=%v query %q: file-backed key missing: %s", v8, query, body)
			}
			if entry["account_type"] != "api_key" {
				t.Fatalf("v8=%v query %q: account_type = %v", v8, query, entry["account_type"])
			}
			if account, present := entry["account"]; present {
				t.Fatalf("v8=%v query %q: exposes account %v", v8, query, account)
			}
		}
	}
}
