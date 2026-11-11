package auth

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// QuotaVerdictState is the provider's answer to "may this credential serve
// requests right now?".
type QuotaVerdictState int

const (
	// QuotaVerdictUnknown means the adapter could not tell. The cooldown is kept.
	QuotaVerdictUnknown QuotaVerdictState = iota
	// QuotaVerdictAllowed means the provider says usage is allowed.
	QuotaVerdictAllowed
	// QuotaVerdictLimited means the provider says a limit is still in force.
	QuotaVerdictLimited
)

// String returns a stable lowercase name for logs.
func (s QuotaVerdictState) String() string {
	switch s {
	case QuotaVerdictAllowed:
		return "allowed"
	case QuotaVerdictLimited:
		return "limited"
	default:
		return "unknown"
	}
}

// QuotaVerdict is an authoritative recheck result. Only an Allowed verdict may
// clear a cooldown.
type QuotaVerdict struct {
	State QuotaVerdictState
	// Signals holds the bounded provider quota values the verdict was based on.
	// It never carries credentials.
	Signals map[string]string
	// ObservedAt is when the provider produced the answer.
	ObservedAt time.Time
	// Source names where the verdict came from (for example an endpoint name).
	Source string
}

// QuotaUsageSnapshot is an advisory, cheap reading of a credential's usage. It
// is only a trigger for deciding when to run a Verdict and never clears a
// cooldown on its own.
type QuotaUsageSnapshot struct {
	Signals    map[string]string
	ObservedAt time.Time
}

// QuotaRecheckHost is the part of Manager an adapter may use to reach the
// provider. Manager implements it.
type QuotaRecheckHost interface {
	// HttpRequest sends req through the credential's executor.
	HttpRequest(ctx context.Context, auth *Auth, req *http.Request) (*http.Response, error)
	// VerifyInference runs a minimal real inference request on credential id so
	// an adapter can read the rate-limit state from the response headers; the
	// returned auth carries the refreshed Quota snapshot.
	VerifyInference(ctx context.Context, id, model string) (*Auth, error)
}

// QuotaRecheckAdapter plugs one provider into the quota recheck loop.
type QuotaRecheckAdapter interface {
	// Provider is the lowercase provider key the adapter serves.
	Provider() string
	// UsageSnapshot returns an advisory usage reading that is cheap to obtain.
	UsageSnapshot(ctx context.Context, host QuotaRecheckHost, auth *Auth) (QuotaUsageSnapshot, error)
	// Verdict returns the authoritative answer. An error or an Unknown state
	// keeps the existing cooldown.
	Verdict(ctx context.Context, host QuotaRecheckHost, auth *Auth) (QuotaVerdict, error)
}

// quotaSnapshotTrigger is implemented by adapters whose UsageSnapshot is its own
// upstream call. The loop polls it on snapshotInterval for cooled credentials
// and runs an early Verdict when snapshotChanged reports a relevant change
// between two snapshots.
type quotaSnapshotTrigger interface {
	snapshotInterval() time.Duration
	snapshotChanged(prev, next map[string]string) bool
}

// RegisterQuotaRecheckAdapter registers adapter for its provider, replacing any
// earlier one. Providers without an adapter are never rechecked.
func (m *Manager) RegisterQuotaRecheckAdapter(adapter QuotaRecheckAdapter) {
	if m == nil || adapter == nil {
		return
	}
	provider := strings.ToLower(strings.TrimSpace(adapter.Provider()))
	if provider == "" {
		return
	}
	m.quotaReconcile.adapters.Store(provider, adapter)
}

func (m *Manager) quotaRecheckAdapter(provider string) QuotaRecheckAdapter {
	adapter, _ := m.quotaReconcile.adapters.Load(strings.ToLower(strings.TrimSpace(provider)))
	typed, _ := adapter.(QuotaRecheckAdapter)
	return typed
}

// errQuotaCooldownChanged reports that the cooldown a recheck observed is no
// longer the current one.
var errQuotaCooldownChanged = errors.New("quota cooldown changed since it was observed")

// quotaCooldownIdentity identifies one cooldown episode of a credential. A new
// quota rejection bumps the rejection count (and usually the recovery time),
// and a replaced credential changes its version or epoch, so any of them makes
// an earlier observation stale.
type quotaCooldownIdentity struct {
	epoch      uint64
	version    uint64
	rejections uint64
	recoverAt  string
}

func newQuotaCooldownIdentity(auth *Auth) quotaCooldownIdentity {
	id := quotaCooldownIdentity{epoch: auth.RegistrationEpoch, version: auth.CredentialVersion, rejections: auth.quotaRejections}
	var b strings.Builder
	b.WriteString(strconv.FormatInt(auth.Quota.NextRecoverAt.UnixNano(), 10))
	keys := make([]string, 0, len(auth.ModelStates))
	for key, state := range auth.ModelStates {
		if state != nil && state.Quota.Exceeded {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		b.WriteByte('|')
		b.WriteString(key)
		b.WriteByte('=')
		b.WriteString(strconv.FormatInt(auth.ModelStates[key].Quota.NextRecoverAt.UnixNano(), 10))
	}
	id.recoverAt = b.String()
	return id
}
