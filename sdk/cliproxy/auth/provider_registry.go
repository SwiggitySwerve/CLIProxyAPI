package auth

import (
	"net/http"
	"time"
)

// providerQuota is one provider's entry in the provider registry: which quota
// signals it exposes and how the hub reads and rechecks them. It is the first
// slice of the provider adapter contract (roadmap 3.2); later slices add the
// header allowlist, 429 classification and the model catalog source.
type providerQuota struct {
	// observes reports that responses carry a passive credential-level quota
	// snapshot (ProviderSupportsQuotaObservation).
	observes bool
	// resetWindow returns the reset time soonest-reset ranks on. Nil means the
	// provider has no reset time and ranks like fill-first.
	resetWindow soonestResetWindowFunc
	// recheck is the QuotaRecheckAdapter NewManager registers. Nil means the
	// provider is never rechecked unless an adapter is registered explicitly.
	recheck QuotaRecheckAdapter
	// trialVerdict makes the all-cooled round send the user's request on a
	// cooled credential (half-open trial) and read the verdict from the response
	// headers. Without one the round calls the registered adapter's Verdict
	// (Codex today); a provider with neither is not rechecked by that round.
	trialVerdict func(cooled *Auth, now time.Time) func(http.Header) QuotaVerdictState
}

// providerQuotas is keyed by lower-case provider. It is written only in init
// and read-only afterwards.
var providerQuotas = map[string]providerQuota{}

// registerProviderQuota adds or replaces the entry for provider.
func registerProviderQuota(provider string, entry providerQuota) {
	providerQuotas[quotaRecheckProviderKey(provider)] = entry
}

// providerQuotaFor returns the entry for provider, or the zero entry (observes
// nothing, no window, no adapter, no trial) for an unknown provider.
func providerQuotaFor(provider string) providerQuota {
	return providerQuotas[quotaRecheckProviderKey(provider)]
}

// One registration per provider, in the style of RegisterQuotaRecheckAdapter.
// A provider without an entry observes nothing and is never rechecked.
func init() {
	registerProviderQuota("claude", providerQuota{
		observes:     true,
		resetWindow:  claudeResetWindow,
		recheck:      claudeQuotaRecheckAdapter{},
		trialVerdict: claudeTrialVerdict,
	})
	registerProviderQuota("codex", providerQuota{
		observes:    true,
		resetWindow: codexResetWindow,
		recheck:     codexQuotaRecheckAdapter{},
	})
	// Devin observes, but no Devin header is on the signal allowlist yet, so a
	// response never replaces its snapshot.
	registerProviderQuota("devin", providerQuota{observes: true})
}
