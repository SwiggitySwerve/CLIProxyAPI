package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

const cooldownEvidenceMessage = "upstream 429 cooldown applied"

func cooldownEvidenceEntries(hook *logtest.Hook) []*log.Entry {
	var entries []*log.Entry
	for _, entry := range hook.AllEntries() {
		if strings.Contains(entry.Message, cooldownEvidenceMessage) {
			entries = append(entries, entry)
		}
	}
	return entries
}

func newCooldownEvidenceAuth(t *testing.T, manager *Manager, provider, id, model string) *Auth {
	t.Helper()
	auth := &Auth{
		ID:       id,
		Provider: provider,
		Attributes: map[string]string{
			"api_key": "sk-ant-secret-token-value",
		},
		Metadata: map[string]any{"email": "operator@example.test"},
	}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	return auth
}

func TestClaude429CooldownLogsUpstreamHeadersOnce(t *testing.T) {
	hook := setupTestLoggerHook(t)
	manager := NewManager(nil, nil, nil)
	const model = "claude-opus-evidence"
	auth := newCooldownEvidenceAuth(t, manager, "claude", "claude-operator@example.test.json", model)

	resetEpoch := "1763000000"
	ctx := internallogging.WithResponseHeadersHolder(context.Background())
	internallogging.SetResponseHeaders(ctx, http.Header{
		"Anthropic-Ratelimit-Unified-Status":               []string{"rejected"},
		"Anthropic-Ratelimit-Unified-7d-Status":            []string{"rejected"},
		"Anthropic-Ratelimit-Unified-7d-Utilization":       []string{"1.0"},
		"Anthropic-Ratelimit-Unified-7d-Reset":             []string{resetEpoch},
		"Anthropic-Ratelimit-Unified-Representative-Claim": []string{"seven_day"},
		"Anthropic-Ratelimit-Unified-Overage-Status":       []string{"rejected"},
		"Retry-After":   []string{"79200"},
		"Authorization": []string{"Bearer sk-ant-secret-token-value"},
		"X-Api-Key":     []string{"sk-ant-secret-token-value"},
		"Set-Cookie":    []string{"session=secret-cookie"},
	})

	retryAfter := 22 * time.Hour
	before := time.Now()
	manager.MarkResult(ctx, Result{
		AuthID:          auth.ID,
		Provider:        "claude",
		Model:           model,
		Success:         false,
		RetryAfter:      &retryAfter,
		CredentialScope: true,
		Error:           &Error{HTTPStatus: http.StatusTooManyRequests, Message: "seven_day rejected"},
	})

	entries := cooldownEvidenceEntries(hook)
	if len(entries) != 1 {
		t.Fatalf("expected exactly one cooldown evidence line, got %d: %#v", len(entries), hook.AllEntries())
	}
	entry := entries[0]
	if entry.Level != log.WarnLevel {
		t.Fatalf("level = %v, want warn", entry.Level)
	}
	for _, want := range []string{
		"scope=credential",
		"retry_after=22h0m0s",
		"anthropic-ratelimit-unified-status=rejected",
		"anthropic-ratelimit-unified-7d-status=rejected",
		"anthropic-ratelimit-unified-7d-utilization=1.0",
		"anthropic-ratelimit-unified-7d-reset=" + resetEpoch,
		"anthropic-ratelimit-unified-representative-claim=seven_day",
		"anthropic-ratelimit-unified-overage-status=rejected",
		"retry-after=79200",
		"until=",
	} {
		if !strings.Contains(entry.Message, want) {
			t.Fatalf("message missing %q: %s", want, entry.Message)
		}
	}
	if entry.Data["provider"] != "claude" || entry.Data["model"] != model || entry.Data["reason"] != "credential_quota" {
		t.Fatalf("unexpected fields: %#v", entry.Data)
	}
	if idx, _ := entry.Data["auth_index"].(string); idx == "" || idx != auth.EnsureIndex() {
		t.Fatalf("auth_index = %#v, want %q", entry.Data["auth_index"], auth.EnsureIndex())
	}

	// The logged deadline must be the one the manager applied.
	updated, ok := manager.GetByID(auth.ID)
	if !ok {
		t.Fatalf("auth %q missing", auth.ID)
	}
	deadline := updated.Quota.NextRecoverAt
	if deadline.Before(before.Add(retryAfter - time.Minute)) {
		t.Fatalf("manager deadline %v is not ~22h out", deadline)
	}
	if !strings.Contains(entry.Message, "until="+deadline.UTC().Format(time.RFC3339)) {
		t.Fatalf("message deadline does not match manager deadline %s: %s", deadline.UTC().Format(time.RFC3339), entry.Message)
	}

	rendered := entry.Message
	for key, value := range entry.Data {
		rendered += " " + key + "=" + fmt.Sprint(value)
	}
	for _, secret := range []string{"sk-ant-secret-token-value", "operator@example.test", "secret-cookie", "Bearer", auth.ID} {
		if strings.Contains(rendered, secret) {
			t.Fatalf("log line leaks %q: %s", secret, rendered)
		}
	}
}

func TestCooldownEvidenceNotLoggedWhenDeadlineNotExtended(t *testing.T) {
	hook := setupTestLoggerHook(t)
	manager := NewManager(nil, nil, nil)
	const model = "claude-opus-evidence-repeat"
	auth := newCooldownEvidenceAuth(t, manager, "claude", "claude-repeat.json", model)
	ctx := internallogging.WithResponseHeadersHolder(context.Background())
	internallogging.SetResponseHeaders(ctx, http.Header{
		"Anthropic-Ratelimit-Unified-Status":    []string{"rejected"},
		"Anthropic-Ratelimit-Unified-7d-Status": []string{"rejected"},
	})

	long := 22 * time.Hour
	short := time.Hour
	for _, retryAfter := range []time.Duration{long, short} {
		retryAfter := retryAfter
		manager.MarkResult(ctx, Result{
			AuthID: auth.ID, Provider: "claude", Model: model, Success: false,
			RetryAfter: &retryAfter, CredentialScope: true,
			Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "rejected"},
		})
	}
	if got := len(cooldownEvidenceEntries(hook)); got != 1 {
		t.Fatalf("expected one line (the extension only), got %d", got)
	}
}

func TestCodex429ModelScopedCooldownLogsCodexHeaders(t *testing.T) {
	hook := setupTestLoggerHook(t)
	manager := NewManager(nil, nil, nil)
	const model = "gpt-evidence"
	auth := newCooldownEvidenceAuth(t, manager, "codex", "codex-evidence.json", model)
	ctx := internallogging.WithResponseHeadersHolder(context.Background())
	internallogging.SetResponseHeaders(ctx, http.Header{
		"X-Codex-Primary-Used-Percent":          []string{"100"},
		"X-Codex-Primary-Reset-After-Seconds":   []string{"3600"},
		"X-Codex-Secondary-Used-Percent":        []string{"42"},
		"X-Codex-Secondary-Reset-After-Seconds": []string{"86400"},
		"Authorization":                         []string{"Bearer codex-secret-token"},
	})
	retryAfter := time.Hour
	manager.MarkResult(ctx, Result{
		AuthID: auth.ID, Provider: "codex", Model: model, Success: false,
		RetryAfter: &retryAfter,
		Error:      &Error{HTTPStatus: http.StatusTooManyRequests, Message: "usage_limit_reached"},
	})

	entries := cooldownEvidenceEntries(hook)
	if len(entries) != 1 {
		t.Fatalf("expected exactly one line, got %d", len(entries))
	}
	msg := entries[0].Message
	for _, want := range []string{
		"scope=model",
		"x-codex-primary-used-percent=100",
		"x-codex-primary-reset-after-seconds=3600",
		"x-codex-secondary-used-percent=42",
		"x-codex-secondary-reset-after-seconds=86400",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message missing %q: %s", want, msg)
		}
	}
	if strings.Contains(msg, "codex-secret-token") {
		t.Fatalf("message leaks token: %s", msg)
	}
	if entries[0].Data["reason"] != "quota" {
		t.Fatalf("reason = %#v, want quota", entries[0].Data["reason"])
	}
}
