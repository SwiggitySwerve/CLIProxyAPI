package auth

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"
)

// QuotaObservationSourceResponseHeaders marks an observation captured from
// upstream response headers by MarkResult.
const QuotaObservationSourceResponseHeaders = "response_headers"

// quotaObservationFlushInterval bounds how often a changed observation alone
// triggers a cooldown state save. Observations change on every response, so
// they are coalesced; cooldown changes still save immediately and carry the
// latest observations with them.
const quotaObservationFlushInterval = 30 * time.Second

// QuotaObservationRecord is the persisted passive quota snapshot for one
// auth/model pair. It is advisory: restoring it never creates, extends or
// shortens a cooldown.
type QuotaObservationRecord struct {
	Signals    map[string]string `json:"signals,omitempty"`
	ObservedAt time.Time         `json:"observed_at"`
	Source     string            `json:"source,omitempty"`
}

// quotaObservationFlushState tracks whether observations changed since the last
// successful save, and when that save happened.
type quotaObservationFlushState struct {
	mu        sync.Mutex
	dirty     bool
	lastFlush time.Time
}

func (m *Manager) quotaObservationClock() time.Time {
	if m.quotaObservationNow != nil {
		return m.quotaObservationNow()
	}
	return time.Now()
}

// markQuotaObservationDirty records a changed observation and reports whether
// the throttle window allows saving it now.
func (m *Manager) markQuotaObservationDirty() bool {
	state := &m.quotaObservationFlush
	state.mu.Lock()
	defer state.mu.Unlock()
	state.dirty = true
	now := m.quotaObservationClock()
	return state.lastFlush.IsZero() || now.Sub(state.lastFlush) >= quotaObservationFlushInterval
}

// noteQuotaObservationsSaved is called after any successful state save, since
// every save writes the current observations.
func (m *Manager) noteQuotaObservationsSaved() {
	state := &m.quotaObservationFlush
	state.mu.Lock()
	state.dirty = false
	state.lastFlush = m.quotaObservationClock()
	state.mu.Unlock()
}

// observeQuotaChange saves state after a changed observation when the throttle
// window allows it. Callers must not hold m.mu.
func (m *Manager) observeQuotaChange() {
	if m.markQuotaObservationDirty() {
		m.persistCooldownStates(context.Background())
	}
}

// FlushQuotaObservations writes observations that changed since the last save.
// Call it on shutdown so the throttle window does not lose the newest snapshot.
func (m *Manager) FlushQuotaObservations(ctx context.Context) {
	if m == nil {
		return
	}
	state := &m.quotaObservationFlush
	state.mu.Lock()
	dirty := state.dirty
	state.mu.Unlock()
	if dirty {
		m.persistCooldownStates(ctx)
	}
}

// sanitizeQuotaObservationSignals re-applies the provider allowlist and value
// bounds, so only quota header values are ever persisted or restored.
func sanitizeQuotaObservationSignals(provider string, raw map[string]string) map[string]string {
	if len(raw) == 0 || !ProviderSupportsQuotaObservation(provider) {
		return nil
	}
	headers := make(http.Header, len(raw))
	for key, value := range raw {
		headers.Set(key, value)
	}
	return collectQuotaSignals(provider, headers)
}

func quotaObservationRecordOf(provider string, quota QuotaState) *QuotaObservationRecord {
	if quota.ObservedAt.IsZero() {
		return nil
	}
	signals := sanitizeQuotaObservationSignals(provider, quota.Signals)
	if len(signals) == 0 {
		return nil
	}
	return &QuotaObservationRecord{
		Signals:    signals,
		ObservedAt: quota.ObservedAt.UTC(),
		Source:     QuotaObservationSourceResponseHeaders,
	}
}

// attachQuotaObservationsLocked adds the auth's observations to its cooldown
// records, appending observation-only records (zero NextRetryAfter) for pairs
// that are not cooling. Restore ignores records with a zero NextRetryAfter for
// cooldown purposes, including on builds that predate this field.
func attachQuotaObservationsLocked(auth *Auth, records []CooldownStateRecord) []CooldownStateRecord {
	if auth == nil || auth.ID == "" || auth.Disabled || auth.Status == StatusDisabled {
		return records
	}
	provider := strings.TrimSpace(auth.Provider)
	attach := func(model string, quota QuotaState) {
		observation := quotaObservationRecordOf(provider, quota)
		if observation == nil {
			return
		}
		for i := range records {
			if records[i].Model == model {
				records[i].Observation = observation
				return
			}
		}
		// UpdatedAt stays zero so versioned stores (Postgres upserts only when
		// updated_at does not go backwards) stamp the save time; ObservedAt can
		// be older than a cooldown row this record replaces after a clear.
		records = append(records, CooldownStateRecord{
			Provider:    provider,
			AuthID:      auth.ID,
			AuthFile:    cooldownAuthFile(auth),
			Model:       model,
			Status:      "observed",
			Observation: observation,
		})
	}
	attach("", auth.Quota)
	for model, state := range auth.ModelStates {
		if state != nil && strings.TrimSpace(model) != "" {
			attach(strings.TrimSpace(model), state.Quota)
		}
	}
	return records
}

// restoreQuotaObservationLocked restores only ObservedAt and Signals. It never
// reads or writes cooldown or scheduling fields.
func (m *Manager) restoreQuotaObservationLocked(record CooldownStateRecord) bool {
	if record.Observation == nil || record.Observation.ObservedAt.IsZero() {
		return false
	}
	auth := m.auths[strings.TrimSpace(record.AuthID)]
	if auth == nil {
		return false
	}
	provider := strings.TrimSpace(auth.Provider)
	if provider == "" {
		provider = strings.TrimSpace(record.Provider)
	}
	signals := sanitizeQuotaObservationSignals(provider, record.Observation.Signals)
	if len(signals) == 0 {
		return false
	}
	restored := QuotaState{ObservedAt: record.Observation.ObservedAt, Signals: signals}
	model := strings.TrimSpace(record.Model)
	if model == "" {
		before := auth.Quota.ObservedAt
		auth.Quota = mergeQuotaObservation(auth.Quota, restored)
		return !auth.Quota.ObservedAt.Equal(before)
	}
	state := ensureModelState(auth, model)
	if state == nil {
		return false
	}
	before := state.Quota.ObservedAt
	state.Quota = mergeQuotaObservation(state.Quota, restored)
	return !state.Quota.ObservedAt.Equal(before)
}
