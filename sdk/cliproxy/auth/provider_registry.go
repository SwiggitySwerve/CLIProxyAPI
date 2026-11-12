package auth

import (
	"net/http"
	"time"
)

// providerQuota is one provider's entry in the provider registry: which quota
// signals it exposes and how the hub reads and rechecks them. It is the first
// part of the provider adapter contract (roadmap 3.2); later slices add 429
// classification and the model catalog source.
type providerQuota struct {
	// observes reports that responses carry a passive credential-level quota
	// snapshot (ProviderSupportsQuotaObservation).
	observes bool
	// signalRank reports whether a lower-case response header name is one of
	// the provider's quota signals and its retention rank: past the header cap
	// lower ranks are kept first (collectQuotaSignals). Nil means no header is.
	signalRank func(name string) (rank int, ok bool)
	// rejects reports that collected signals pin a cooldown to the provider's
	// own reset (rejectedClaudeSignalsForCooldown). Nil means none do. A
	// provider with one also restores persisted cooldown signals and echoes
	// them on the 429 the hub returns (providerPinsCooldownSignals). Those
	// signals are still read in Claude's format: persisted signals keep only
	// anthropic-ratelimit-unified-* keys (sanitizePersistedClaudeCooldownSignals),
	// and the selector reads them with hasRejectedClaudeUnifiedSignals.
	rejects func(signals map[string]string) bool
	// recheckCadence is the recheck cadence used when the configured mode sets
	// no durations. Nil means the slow default (quotaRecheckPolicyFor).
	recheckCadence *quotaRecheckCadence
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

// providerPinsCooldownSignals reports that the provider's rejection signals pin
// its cooldowns, so they are restored on restart and echoed on the 429 the hub
// returns. Only Claude has a rejects rule today.
func providerPinsCooldownSignals(provider string) bool {
	return providerQuotaFor(provider).rejects != nil
}

// One registration per provider, in the style of RegisterQuotaRecheckAdapter.
// A provider without an entry observes nothing and is never rechecked.
func init() {
	registerProviderQuota("claude", providerQuota{
		observes:     true,
		signalRank:   claudeSignalRank,
		rejects:      hasRejectedClaudeUnifiedSignals,
		resetWindow:  claudeResetWindow,
		recheck:      claudeQuotaRecheckAdapter{},
		trialVerdict: claudeTrialVerdict,
	})
	registerProviderQuota("codex", providerQuota{
		observes:    true,
		signalRank:  codexSignalRank,
		resetWindow: codexResetWindow,
		recheck:     codexQuotaRecheckAdapter{},
		// The original loop: probe a cooled credential on every pass, limited
		// only by quotaProbeMinGap.
		recheckCadence: &quotaRecheckCadence{interval: quotaProbeMinGap, nearInterval: quotaProbeMinGap},
	})
	// Devin observes, but no Devin header is on the signal allowlist yet, so a
	// response never replaces its snapshot.
	registerProviderQuota("devin", providerQuota{observes: true})
}
