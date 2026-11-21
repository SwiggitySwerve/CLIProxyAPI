package management

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MoeForge/moego/json"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type mockCatalogAuthProvider struct {
	identifier string
	entries    []pluginapi.AuthProviderEntry
}

func (m *mockCatalogAuthProvider) Identifier() string {
	return m.identifier
}

func (m *mockCatalogAuthProvider) AuthEntries() []pluginapi.AuthProviderEntry {
	return m.entries
}

func (m *mockCatalogAuthProvider) ParseAuth(context.Context, pluginapi.AuthParseRequest) (pluginapi.AuthParseResponse, error) {
	return pluginapi.AuthParseResponse{}, nil
}

func (m *mockCatalogAuthProvider) StartLogin(ctx context.Context, req pluginapi.AuthLoginStartRequest) (pluginapi.AuthLoginStartResponse, error) {
	return pluginapi.AuthLoginStartResponse{}, nil
}

func (m *mockCatalogAuthProvider) PollLogin(ctx context.Context, req pluginapi.AuthLoginPollRequest) (pluginapi.AuthLoginPollResponse, error) {
	return pluginapi.AuthLoginPollResponse{Status: "wait"}, nil
}

func (m *mockCatalogAuthProvider) RefreshAuth(context.Context, pluginapi.AuthRefreshRequest) (pluginapi.AuthRefreshResponse, error) {
	return pluginapi.AuthRefreshResponse{}, nil
}

func newCatalogTestAuthFile(t *testing.T, authDir, fileName string) string {
	t.Helper()
	full := filepath.Join(authDir, fileName)
	if err := os.WriteFile(full, []byte(`{"type":"antigravity"}`), 0600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	return full
}

func TestGetProviderCatalogV8_ContractLifecycleAndETag(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("CLIPROXY_INSTANCE_ID", "gemini-primary")
	t.Setenv(acceptedProvidersEnvVar, "")

	authDir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	activePath := newCatalogTestAuthFile(t, authDir, "antigravity-active.json")
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:         "antigravity-active.json",
		FileName:   "antigravity-active.json",
		Provider:   "antigravity",
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{coreauth.AttributePath: activePath},
	}); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	host := pluginhost.New()
	host.RegisterPluginForTest("example-provider-plugin", pluginapi.Plugin{
		Metadata: pluginapi.Metadata{
			Name: "Example Provider",
			Logo: "/v0/resource/plugins/example-provider-plugin/logo.svg",
		},
		Capabilities: pluginapi.Capabilities{
			AuthProvider: &mockCatalogAuthProvider{
				identifier: "example-plugin",
				entries: []pluginapi.AuthProviderEntry{
					{
						Identifier: "example-plugin",
						Name:       "Example Provider",
						Logo:       "/v0/resource/plugins/example-provider-plugin/logo.svg",
					},
				},
			},
			QuotaProvider: &mockQuotaProvider{
				identifier: "example-plugin",
				supported:  []string{"example-plugin"},
			},
		},
	})

	cfg := &config.Config{
		AuthDir:           authDir,
		AcceptedProviders: []string{"antigravity", "example-plugin"},
	}
	h := NewHandlerWithoutConfigFilePath(cfg, manager)
	h.SetPluginHost(host)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v8/management/providers", nil)
	h.GetProviderCatalogV8(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatalf("expected non-empty ETag header")
	}

	var resp providerCatalogResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode catalog: %v", err)
	}
	if resp.SchemaVersion != 1 {
		t.Fatalf("schema_version = %d, want 1", resp.SchemaVersion)
	}
	if resp.InstanceID == nil || *resp.InstanceID != "gemini-primary" {
		t.Fatalf("instance_id = %#v, want gemini-primary", resp.InstanceID)
	}
	if len(resp.AcceptedProviders) != 2 || resp.AcceptedProviders[0] != "antigravity" || resp.AcceptedProviders[1] != "example-plugin" {
		t.Fatalf("accepted_providers = %#v", resp.AcceptedProviders)
	}
	if len(resp.Providers) != 2 {
		t.Fatalf("providers len = %d, want 2: %#v", len(resp.Providers), resp.Providers)
	}

	anti := resp.Providers[0]
	if anti.ID != "antigravity" || anti.Source != "builtin" || anti.Lifecycle != lifecycleActive {
		t.Fatalf("unexpected antigravity entry: %+v", anti)
	}
	if anti.Credentials.Total != 1 || anti.Credentials.Active != 1 || anti.Quota.Reader != "builtin" || !anti.Quota.Supported {
		t.Fatalf("unexpected antigravity counts/quota: %+v", anti)
	}

	pluginEntry := resp.Providers[1]
	if pluginEntry.ID != "example-plugin" || pluginEntry.Source != "plugin" || pluginEntry.Lifecycle != lifecycleAwaitingAuth {
		t.Fatalf("unexpected plugin entry: %+v", pluginEntry)
	}
	if pluginEntry.PluginID == nil || *pluginEntry.PluginID != "example-provider-plugin" {
		t.Fatalf("plugin_id = %#v", pluginEntry.PluginID)
	}
	if pluginEntry.Logo == nil || *pluginEntry.Logo != "/v0/resource/plugins/example-provider-plugin/logo.svg" {
		t.Fatalf("logo = %#v", pluginEntry.Logo)
	}
	if pluginEntry.Quota.Reader != "plugin" || !pluginEntry.Quota.Supported || pluginEntry.Quota.PluginID == nil || *pluginEntry.Quota.PluginID != "example-provider-plugin" {
		t.Fatalf("unexpected plugin quota: %+v", pluginEntry.Quota)
	}

	// Second call with matching If-None-Match returns 304 Not Modified.
	rec304 := httptest.NewRecorder()
	ctx304, _ := gin.CreateTestContext(rec304)
	ctx304.Request = httptest.NewRequest(http.MethodGet, "/v8/management/providers", nil)
	ctx304.Request.Header.Set("If-None-Match", etag)
	h.GetProviderCatalogV8(ctx304)
	if rec304.Code != http.StatusNotModified {
		t.Fatalf("304 check status = %d, want 304", rec304.Code)
	}

	// Mutate credential to disabled -> lifecycle becomes disabled and ETag changes.
	if _, err := manager.Update(context.Background(), &coreauth.Auth{
		ID:         "antigravity-active.json",
		FileName:   "antigravity-active.json",
		Provider:   "antigravity",
		Disabled:   true,
		Status:     coreauth.StatusDisabled,
		Attributes: map[string]string{coreauth.AttributePath: activePath},
	}); err != nil {
		t.Fatalf("disable auth: %v", err)
	}
	recAfter := httptest.NewRecorder()
	ctxAfter, _ := gin.CreateTestContext(recAfter)
	ctxAfter.Request = httptest.NewRequest(http.MethodGet, "/v8/management/providers", nil)
	ctxAfter.Request.Header.Set("If-None-Match", etag)
	h.GetProviderCatalogV8(ctxAfter)
	if recAfter.Code != http.StatusOK {
		t.Fatalf("after disable status = %d, want 200", recAfter.Code)
	}
	if recAfter.Header().Get("ETag") == etag {
		t.Fatalf("expected ETag to change after lifecycle transition")
	}
	var afterResp providerCatalogResponse
	if err := json.Unmarshal(recAfter.Body.Bytes(), &afterResp); err != nil {
		t.Fatalf("decode after catalog: %v", err)
	}
	if afterResp.Providers[0].Lifecycle != lifecycleDisabled || afterResp.Providers[0].Credentials.Disabled != 1 {
		t.Fatalf("expected disabled lifecycle, got %+v", afterResp.Providers[0])
	}

	// Re-enable with RequiresSignIn -> needs_attention.
	if _, err := manager.Update(context.Background(), &coreauth.Auth{
		ID:             "antigravity-active.json",
		FileName:       "antigravity-active.json",
		Provider:       "antigravity",
		Status:         coreauth.StatusError,
		RequiresSignIn: true,
		Attributes:     map[string]string{coreauth.AttributePath: activePath},
	}); err != nil {
		t.Fatalf("error auth: %v", err)
	}
	recErr := httptest.NewRecorder()
	ctxErr, _ := gin.CreateTestContext(recErr)
	ctxErr.Request = httptest.NewRequest(http.MethodGet, "/v8/management/providers", nil)
	h.GetProviderCatalogV8(ctxErr)
	var errResp providerCatalogResponse
	if err := json.Unmarshal(recErr.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("decode err catalog: %v", err)
	}
	if errResp.Providers[0].Lifecycle != lifecycleNeedsAttention || errResp.Providers[0].Credentials.Problem != 1 {
		t.Fatalf("expected needs_attention lifecycle, got %+v", errResp.Providers[0])
	}
}

func TestGetProviderCatalogV8_OpenHubIncludesAllBuiltins(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv(acceptedProvidersEnvVar, "")

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, coreauth.NewManager(nil, nil, nil))
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v8/management/providers", nil)
	h.GetProviderCatalogV8(ctx)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp providerCatalogResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode catalog: %v", err)
	}
	if resp.AcceptedProviders != nil {
		t.Fatalf("expected nil accepted_providers on open hub, got %#v", resp.AcceptedProviders)
	}
	if len(resp.Providers) != len(builtinOAuthProviders) {
		t.Fatalf("providers len = %d, want %d", len(resp.Providers), len(builtinOAuthProviders))
	}
	for _, entry := range resp.Providers {
		if entry.Lifecycle != lifecycleAwaitingAuth {
			t.Fatalf("expected awaiting_auth for %s, got %s", entry.ID, entry.Lifecycle)
		}
		if entry.OAuth == nil {
			t.Fatalf("expected oauth descriptor for builtin %s", entry.ID)
		}
		if entry.OAuth.Aliases == nil || entry.OAuth.Domains == nil {
			t.Fatalf("expected non-nil slices for builtin %s", entry.ID)
		}
	}
}

func TestAcceptedProvidersEnforcement_OAuthImportAndUpload(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv(acceptedProvidersEnvVar, "")

	authDir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{
		AuthDir:           authDir,
		AcceptedProviders: []string{"antigravity"},
	}, manager)

	// 1. StartOAuthV8 rejects unaccepted provider with 409 provider_not_accepted.
	recOAuth := httptest.NewRecorder()
	ctxOAuth, _ := gin.CreateTestContext(recOAuth)
	ctxOAuth.Request = httptest.NewRequest(http.MethodGet, "/v8/management/oauth/auth-url?provider=codex", nil)
	h.StartOAuthV8(ctxOAuth)
	if recOAuth.Code != http.StatusConflict {
		t.Fatalf("StartOAuthV8 status = %d, want 409, body = %s", recOAuth.Code, recOAuth.Body.String())
	}
	var errBody map[string]any
	if err := json.Unmarshal(recOAuth.Body.Bytes(), &errBody); err != nil {
		t.Fatalf("decode 409 body: %v", err)
	}
	if errBody["error"] != "provider_not_accepted" || errBody["provider"] != "codex" {
		t.Fatalf("unexpected 409 body: %#v", errBody)
	}

	// 2. ImportOAuthV8 rejects unaccepted provider with 409.
	recImport := httptest.NewRecorder()
	ctxImport, _ := gin.CreateTestContext(recImport)
	ctxImport.Request = httptest.NewRequest(http.MethodPost, "/v8/management/oauth/import?provider=vertex", nil)
	h.ImportOAuthV8(ctxImport)
	if recImport.Code != http.StatusConflict {
		t.Fatalf("ImportOAuthV8 status = %d, want 409, body = %s", recImport.Code, recImport.Body.String())
	}

	// 3. UploadAuthFile (raw JSON) rejects unaccepted provider with 409.
	rawCodex := `{"type":"codex","email":"user@example.test","access_token":"tok","refresh_token":"ref","expired":"2099-01-01T00:00:00Z"}`
	recUploadRaw := httptest.NewRecorder()
	ctxUploadRaw, _ := gin.CreateTestContext(recUploadRaw)
	ctxUploadRaw.Request = httptest.NewRequest(http.MethodPost, "/v8/management/credentials?name=codex-wrong.json", strings.NewReader(rawCodex))
	ctxUploadRaw.Request.Header.Set("Content-Type", "application/json")
	h.UploadAuthFile(ctxUploadRaw)
	if recUploadRaw.Code != http.StatusConflict {
		t.Fatalf("UploadAuthFile raw status = %d, want 409, body = %s", recUploadRaw.Code, recUploadRaw.Body.String())
	}

	// 4. UploadAuthFile (multipart single file) rejects unaccepted provider with 409.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "codex-mp.json")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := fw.Write([]byte(rawCodex)); err != nil {
		t.Fatalf("write form file: %v", err)
	}
	_ = mw.Close()
	recUploadMP := httptest.NewRecorder()
	ctxUploadMP, _ := gin.CreateTestContext(recUploadMP)
	ctxUploadMP.Request = httptest.NewRequest(http.MethodPost, "/v8/management/credentials", &buf)
	ctxUploadMP.Request.Header.Set("Content-Type", mw.FormDataContentType())
	h.UploadAuthFile(ctxUploadMP)
	if recUploadMP.Code != http.StatusConflict {
		t.Fatalf("UploadAuthFile multipart status = %d, want 409, body = %s", recUploadMP.Code, recUploadMP.Body.String())
	}

	// 5. Env var override CLIPROXY_ACCEPTED_PROVIDERS takes precedence over config.
	t.Setenv(acceptedProvidersEnvVar, "claude,codex")
	if err := h.checkAcceptedProvider("codex"); err != nil {
		t.Fatalf("expected codex accepted via env override, got %v", err)
	}
	if err := h.checkAcceptedProvider("anthropic"); err != nil {
		t.Fatalf("expected anthropic alias accepted when claude is accepted, got %v", err)
	}
	if err := h.checkAcceptedProvider("antigravity"); err == nil {
		t.Fatalf("expected antigravity rejected under env override")
	}
}

func TestFetchCredentialQuotaV8_BindingExtendedBucketsAndNoProbe(t *testing.T) {
	gin.SetMode(gin.TestMode)
	authDir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)

	period := int64(18000)
	pluginAuth, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "plugin-cred.json",
		FileName: "plugin-cred.json",
		Provider: "example-plugin",
		Status:   coreauth.StatusActive,
	})
	if err != nil {
		t.Fatalf("register plugin auth: %v", err)
	}
	pluginAuth.EnsureIndex()

	probeOnlyAuth, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "probe-only.json",
		FileName: "probe-only.json",
		Provider: "probe-provider",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{
			"quota_probe": map[string]any{
				"url": "https://127.0.0.1/should-never-run",
			},
		},
	})
	if err != nil {
		t.Fatalf("register probe auth: %v", err)
	}
	probeOnlyAuth.EnsureIndex()

	host := pluginhost.New()
	host.RegisterPluginForTest("example-quota-plugin", pluginapi.Plugin{
		Metadata: pluginapi.Metadata{Name: "Example Quota"},
		Capabilities: pluginapi.Capabilities{
			QuotaProvider: &mockQuotaProvider{
				identifier: "example-plugin",
				supported:  []string{"example-plugin"},
				fetchFn: func(ctx context.Context, req pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
					return pluginapi.QuotaFetchResponse{
						Groups: []pluginapi.QuotaGroup{
							{
								DisplayName: "Session",
								Buckets: []pluginapi.QuotaBucket{
									{
										ID:                "five-hour",
										Window:            "5h",
										RemainingFraction: 0.75,
										ResetTime:         "2026-11-21T15:00:00Z",
										PeriodSeconds:     &period,
										Scope:             "account",
									},
								},
							},
						},
					}, nil
				},
			},
		},
	})
	host.RegisterPluginForTest("other-quota-plugin", pluginapi.Plugin{
		Metadata: pluginapi.Metadata{Name: "Other Quota"},
		Capabilities: pluginapi.Capabilities{
			QuotaProvider: &mockQuotaProvider{
				identifier: "other-plugin",
				supported:  []string{"other-plugin"},
			},
		},
	})

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	h.SetPluginHost(host)

	// 1. Host-resolved fetch returns extended QuotaBucket fields (id, periodSeconds, scope).
	recOK := httptest.NewRecorder()
	ctxOK, _ := gin.CreateTestContext(recOK)
	ctxOK.Request = httptest.NewRequest(http.MethodPost, "/v8/management/quota/fetch", strings.NewReader(`{"auth_index":"`+pluginAuth.Index+`"}`))
	ctxOK.Request.Header.Set("Content-Type", "application/json")
	h.FetchCredentialQuotaV8(ctxOK)
	if recOK.Code != http.StatusOK {
		t.Fatalf("v8 quota fetch status = %d, body = %s", recOK.Code, recOK.Body.String())
	}
	var got pluginapi.QuotaFetchResponse
	if err := json.Unmarshal(recOK.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode quota response: %v", err)
	}
	if len(got.Groups) != 1 || len(got.Groups[0].Buckets) != 1 {
		t.Fatalf("unexpected groups: %+v", got.Groups)
	}
	bucket := got.Groups[0].Buckets[0]
	if bucket.ID != "five-hour" || bucket.PeriodSeconds == nil || *bucket.PeriodSeconds != 18000 || bucket.Scope != "account" {
		t.Fatalf("extended bucket fields not preserved: %+v", bucket)
	}

	// 2. Mismatched caller plugin_id is rejected with 403 Forbidden.
	recMismatch := httptest.NewRecorder()
	ctxMismatch, _ := gin.CreateTestContext(recMismatch)
	ctxMismatch.Request = httptest.NewRequest(http.MethodPost, "/v8/management/quota/fetch", strings.NewReader(`{"auth_index":"`+pluginAuth.Index+`","plugin_id":"other-quota-plugin"}`))
	ctxMismatch.Request.Header.Set("Content-Type", "application/json")
	h.FetchCredentialQuotaV8(ctxMismatch)
	if recMismatch.Code != http.StatusForbidden {
		t.Fatalf("mismatched plugin_id status = %d, want 403, body = %s", recMismatch.Code, recMismatch.Body.String())
	}

	// 3. Declarative quota_probe metadata is NOT executed on v8 (returns 501).
	recProbe := httptest.NewRecorder()
	ctxProbe, _ := gin.CreateTestContext(recProbe)
	ctxProbe.Request = httptest.NewRequest(http.MethodPost, "/v8/management/quota/fetch", strings.NewReader(`{"auth_index":"`+probeOnlyAuth.Index+`"}`))
	ctxProbe.Request.Header.Set("Content-Type", "application/json")
	h.FetchCredentialQuotaV8(ctxProbe)
	if recProbe.Code != http.StatusNotImplemented {
		t.Fatalf("probe-only v8 status = %d, want 501, body = %s", recProbe.Code, recProbe.Body.String())
	}
}

func TestBuildAuthFileEntry_LifecycleV8Only(t *testing.T) {
	authDir := t.TempDir()
	authPath := newCatalogTestAuthFile(t, authDir, "c1.json")
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil)
	auth := &coreauth.Auth{
		ID:         "c1.json",
		FileName:   "c1.json",
		Provider:   "antigravity",
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{coreauth.AttributePath: authPath},
	}

	v8Entry := h.buildAuthFileEntry(auth, authFileQuotaContext{v8: true})
	if v8Entry["lifecycle"] != lifecycleActive {
		t.Fatalf("v8 entry lifecycle = %#v, want %q", v8Entry["lifecycle"], lifecycleActive)
	}
	v0Entry := h.buildAuthFileEntry(auth, authFileQuotaContext{v8: false})
	if _, exists := v0Entry["lifecycle"]; exists {
		t.Fatalf("v0 entry must not include lifecycle, got %#v", v0Entry["lifecycle"])
	}
}
