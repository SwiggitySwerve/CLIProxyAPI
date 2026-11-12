package auth

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
)

func claudeObservationContext(status string) context.Context {
	ctx := internallogging.WithResponseHeadersHolder(context.Background())
	internallogging.SetResponseHeaders(ctx, http.Header{
		"Anthropic-Ratelimit-Unified-Status":         []string{status},
		"Anthropic-Ratelimit-Unified-7d-Reset":       []string{"1782951970"},
		"Anthropic-Ratelimit-Unified-7d-Utilization": []string{"0.42"},
		"Authorization": []string{"Bearer sk-secret-token"},
		"X-Request-Id":  []string{"req-123"},
	})
	return ctx
}

type fakeObservationClock struct{ now time.Time }

func (c *fakeObservationClock) Now() time.Time { return c.now }

func newObservationTestManager(t *testing.T, store CooldownStateStore, clock *fakeObservationClock) *Manager {
	t.Helper()
	manager := NewManager(nil, nil, nil)
	manager.SetCooldownStateStore(store)
	if clock != nil {
		manager.quotaObservationNow = clock.Now
	}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), &Auth{ID: "claude-1", Provider: "claude", Status: StatusActive}); errRegister != nil {
		t.Fatalf("Register() returned error: %v", errRegister)
	}
	return manager
}

func observationRecord(records []CooldownStateRecord, authID, model string) *CooldownStateRecord {
	for i := range records {
		if records[i].AuthID == authID && records[i].Model == model {
			return &records[i]
		}
	}
	return nil
}

func TestQuotaObservationPersistThrottlesAndCoalesces(t *testing.T) {
	store := &recordingCooldownStateStore{}
	clock := &fakeObservationClock{now: time.Unix(1_000_000, 0)}
	manager := newObservationTestManager(t, store, clock)

	for i := 0; i < 5; i++ {
		manager.MarkResult(claudeObservationContext("allowed"), Result{AuthID: "claude-1", Provider: "claude", Model: "claude-sonnet-4-5", Success: true})
		clock.now = clock.now.Add(time.Second)
	}
	if got := store.saveCount.Load(); got != 1 {
		t.Fatalf("five observations inside one window saved %d times, want 1", got)
	}

	clock.now = clock.now.Add(quotaObservationFlushInterval)
	manager.MarkResult(claudeObservationContext("allowed_warning"), Result{AuthID: "claude-1", Provider: "claude", Model: "claude-sonnet-4-5", Success: true})
	if got := store.saveCount.Load(); got != 2 {
		t.Fatalf("observation after the window saved %d times in total, want 2", got)
	}
	record := observationRecord(store.savedRecords(), "claude-1", "")
	if record == nil || record.Observation == nil {
		t.Fatalf("saved records %#v carry no credential observation", store.savedRecords())
	}
	if got := record.Observation.Signals["Anthropic-Ratelimit-Unified-Status"]; got != "allowed_warning" {
		t.Fatalf("persisted status = %q, want allowed_warning", got)
	}
	if record.Observation.Source != QuotaObservationSourceResponseHeaders {
		t.Fatalf("persisted source = %q, want %q", record.Observation.Source, QuotaObservationSourceResponseHeaders)
	}
	if !record.NextRetryAfter.IsZero() || !record.UpdatedAt.IsZero() {
		t.Fatalf("observation-only record carries a deadline %v or version %v", record.NextRetryAfter, record.UpdatedAt)
	}
	if modelRecord := observationRecord(store.savedRecords(), "claude-1", "claude-sonnet-4-5"); modelRecord == nil || modelRecord.Observation == nil {
		t.Fatalf("saved records %#v carry no per-model observation", store.savedRecords())
	}
}

func TestQuotaObservationFlushWritesPendingObservation(t *testing.T) {
	store := &recordingCooldownStateStore{}
	clock := &fakeObservationClock{now: time.Unix(1_000_000, 0)}
	manager := newObservationTestManager(t, store, clock)

	manager.MarkResult(claudeObservationContext("allowed"), Result{AuthID: "claude-1", Provider: "claude", Model: "claude-sonnet-4-5", Success: true})
	clock.now = clock.now.Add(time.Second)
	manager.MarkResult(claudeObservationContext("allowed_warning"), Result{AuthID: "claude-1", Provider: "claude", Model: "claude-sonnet-4-5", Success: true})
	if got := store.saveCount.Load(); got != 1 {
		t.Fatalf("saves before flush = %d, want 1", got)
	}

	manager.FlushQuotaObservations(context.Background())
	if got := store.saveCount.Load(); got != 2 {
		t.Fatalf("saves after flush = %d, want 2", got)
	}
	record := observationRecord(store.savedRecords(), "claude-1", "")
	if record == nil || record.Observation == nil || record.Observation.Signals["Anthropic-Ratelimit-Unified-Status"] != "allowed_warning" {
		t.Fatalf("flush did not write the latest observation: %#v", record)
	}

	manager.FlushQuotaObservations(context.Background())
	if got := store.saveCount.Load(); got != 2 {
		t.Fatalf("flush with nothing pending saved again: %d, want 2", got)
	}
}

func TestQuotaObservationPersistNeverWritesSignalsOutsideAllowlist(t *testing.T) {
	dir := t.TempDir()
	store := NewFileCooldownStateStore(dir)
	manager := newObservationTestManager(t, store, &fakeObservationClock{now: time.Unix(1_000_000, 0)})

	manager.MarkResult(claudeObservationContext("allowed"), Result{AuthID: "claude-1", Provider: "claude", Model: "claude-sonnet-4-5", Success: true})
	// Inject non-allowlisted values directly into the live snapshot to prove the
	// write path filters on its own, not only the header collector.
	manager.mu.Lock()
	manager.auths["claude-1"].Quota.Signals["Authorization"] = "Bearer sk-injected"
	manager.auths["claude-1"].Quota.Signals["X-Api-Key"] = "sk-injected-key"
	manager.mu.Unlock()
	manager.markQuotaObservationDirty()
	manager.FlushQuotaObservations(context.Background())

	data, errRead := os.ReadFile(filepath.Join(dir, "claude-1.cds"))
	if errRead != nil {
		t.Fatalf("read state file: %v", errRead)
	}
	content := string(data)
	for _, forbidden := range []string{"Authorization", "X-Api-Key", "sk-", "X-Request-Id", "req-123"} {
		if strings.Contains(content, forbidden) {
			t.Fatalf("state file contains %q:\n%s", forbidden, content)
		}
	}
	for _, want := range []string{`"observed_at"`, `"source": "response_headers"`, "Anthropic-Ratelimit-Unified-Status"} {
		if !strings.Contains(content, want) {
			t.Fatalf("state file lacks %q:\n%s", want, content)
		}
	}
}

func TestQuotaObservationRestoreRoundTripAndNoCooldown(t *testing.T) {
	dir := t.TempDir()
	first := newObservationTestManager(t, NewFileCooldownStateStore(dir), &fakeObservationClock{now: time.Unix(1_000_000, 0)})
	// A rejected status is the strongest signal; a restored copy must still not cool.
	first.MarkResult(claudeObservationContext("rejected"), Result{AuthID: "claude-1", Provider: "claude", Model: "claude-sonnet-4-5", Success: true})
	before, _ := first.GetByID("claude-1")
	if before.Quota.ObservedAt.IsZero() {
		t.Fatal("first manager recorded no observation")
	}
	first.FlushQuotaObservations(context.Background())

	second := newObservationTestManager(t, NewFileCooldownStateStore(dir), nil)
	if errRestore := second.RestoreCooldownStates(context.Background()); errRestore != nil {
		t.Fatalf("RestoreCooldownStates() returned error: %v", errRestore)
	}
	restored, ok := second.GetByID("claude-1")
	if !ok {
		t.Fatal("restored auth not found")
	}
	if !restored.Quota.ObservedAt.Equal(before.Quota.ObservedAt) {
		t.Fatalf("restored ObservedAt = %v, want %v", restored.Quota.ObservedAt, before.Quota.ObservedAt)
	}
	if got := restored.Quota.Signals["Anthropic-Ratelimit-Unified-Status"]; got != "rejected" {
		t.Fatalf("restored status signal = %q, want rejected", got)
	}
	if _, leaked := restored.Quota.Signals["Authorization"]; leaked {
		t.Fatal("restored signals contain Authorization")
	}
	modelState := restored.ModelStates["claude-sonnet-4-5"]
	if modelState == nil || !modelState.Quota.ObservedAt.Equal(before.Quota.ObservedAt) {
		t.Fatalf("restored model state = %#v, want observation at %v", modelState, before.Quota.ObservedAt)
	}
	if restored.Unavailable || !restored.NextRetryAfter.IsZero() || restored.Quota.Exceeded || !restored.Quota.NextRecoverAt.IsZero() || restored.Status == StatusError {
		t.Fatalf("restored observation created a credential cooldown: %#v", restored)
	}
	if modelState.Unavailable || !modelState.NextRetryAfter.IsZero() || modelState.Quota.Exceeded || modelState.Status == StatusError {
		t.Fatalf("restored observation created a model cooldown: %#v", modelState)
	}
}

func TestQuotaObservationRestoreKeepsCooldownDeadlineExact(t *testing.T) {
	nextRetry := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	observedAt := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	store := &recordingCooldownStateStore{load: []CooldownStateRecord{{
		Provider:       "claude",
		AuthID:         "claude-1",
		Status:         "cooling",
		NextRetryAfter: nextRetry,
		Reason:         "quota",
		Quota:          QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: nextRetry},
		UpdatedAt:      observedAt,
		Observation: &QuotaObservationRecord{
			// A reset far earlier than the cooldown must not shorten it.
			Signals:    map[string]string{"Anthropic-Ratelimit-Unified-Status": "allowed", "Anthropic-Ratelimit-Unified-7d-Reset": "1"},
			ObservedAt: observedAt,
			Source:     QuotaObservationSourceResponseHeaders,
		},
	}}}
	manager := newObservationTestManager(t, store, nil)
	if errRestore := manager.RestoreCooldownStates(context.Background()); errRestore != nil {
		t.Fatalf("RestoreCooldownStates() returned error: %v", errRestore)
	}
	restored, _ := manager.GetByID("claude-1")
	if !restored.NextRetryAfter.Equal(nextRetry) || !restored.Quota.NextRecoverAt.Equal(nextRetry) || !restored.Unavailable {
		t.Fatalf("cooldown changed by restore: next_retry=%v recover=%v unavailable=%v, want %v", restored.NextRetryAfter, restored.Quota.NextRecoverAt, restored.Unavailable, nextRetry)
	}
	if !restored.Quota.ObservedAt.Equal(observedAt) || restored.Quota.Signals["Anthropic-Ratelimit-Unified-Status"] != "allowed" {
		t.Fatalf("observation not restored beside cooldown: %#v", restored.Quota)
	}
}

func TestQuotaObservationRestoreLoadsLegacyStateFile(t *testing.T) {
	dir := t.TempDir()
	nextRetry := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	legacy := `{
  "version": 1,
  "auth_id": "claude-1",
  "provider": "claude",
  "updated_at": "2026-01-01T00:00:00Z",
  "records": [
    {
      "provider": "claude",
      "auth_id": "claude-1",
      "status": "cooling",
      "next_retry_after": "` + nextRetry.Format(time.RFC3339) + `",
      "reason": "quota",
      "quota": {"exceeded": true, "reason": "quota", "next_recover_at": "` + nextRetry.Format(time.RFC3339) + `"},
      "updated_at": "2026-01-01T00:00:00Z"
    }
  ]
}
`
	if errWrite := os.WriteFile(filepath.Join(dir, "claude-1.cds"), []byte(legacy), 0o600); errWrite != nil {
		t.Fatalf("write legacy file: %v", errWrite)
	}
	manager := newObservationTestManager(t, NewFileCooldownStateStore(dir), nil)
	if errRestore := manager.RestoreCooldownStates(context.Background()); errRestore != nil {
		t.Fatalf("RestoreCooldownStates() returned error: %v", errRestore)
	}
	restored, _ := manager.GetByID("claude-1")
	if !restored.Unavailable || !restored.NextRetryAfter.Equal(nextRetry) {
		t.Fatalf("legacy cooldown not restored: %#v", restored)
	}
	if !restored.Quota.ObservedAt.IsZero() || len(restored.Quota.Signals) != 0 {
		t.Fatalf("legacy file produced an observation: %#v", restored.Quota)
	}
}
