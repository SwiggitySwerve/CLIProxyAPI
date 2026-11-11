package auth

import (
	"net/http"
	"strings"
	"sync/atomic"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// ultrafastPolicy holds the normalized credential identities allowed to use the
// ultrafast tier. The zero value allows nobody.
type ultrafastPolicy struct {
	accountIDs map[string]struct{}
	authIDs    map[string]struct{}
	emails     map[string]struct{}
}

var ultrafastPolicyValue atomic.Pointer[ultrafastPolicy]

// SetUltrafastPolicy installs the credentials entitled to the ultrafast tier from
// config. An empty config denies ultrafast for every credential (fail closed).
func SetUltrafastPolicy(cfg internalconfig.UltrafastConfig) {
	ultrafastPolicyValue.Store(&ultrafastPolicy{
		accountIDs: ultrafastIdentitySet(cfg.AccountIDs),
		authIDs:    ultrafastIdentitySet(cfg.AuthIDs),
		emails:     ultrafastIdentitySet(cfg.Emails),
	})
}

func ultrafastIdentitySet(values []string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value = normalizeUltrafastIdentity(value); value != "" {
			set[value] = struct{}{}
		}
	}
	return set
}

func normalizeUltrafastIdentity(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func (p *ultrafastPolicy) allows(auth *Auth) bool {
	if p == nil || auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return false
	}
	matches := func(set map[string]struct{}, value string) bool {
		value = normalizeUltrafastIdentity(value)
		if value == "" {
			return false
		}
		_, ok := set[value]
		return ok
	}
	accountID, _ := auth.Metadata["account_id"].(string)
	email, _ := auth.Metadata["email"].(string)
	return matches(p.accountIDs, accountID) || matches(p.authIDs, auth.ID) || matches(p.emails, email)
}

// ValidateServiceTierAccount rejects ultrafast on every credential that is not
// listed in routing.ultrafast. Other service tiers retain their existing
// credential eligibility.
func ValidateServiceTierAccount(auth *Auth, tier string) error {
	if !strings.EqualFold(strings.TrimSpace(tier), "ultrafast") {
		return nil
	}
	if ultrafastPolicyValue.Load().allows(auth) {
		return nil
	}
	// This is a local request policy failure, not a bad credential. Do not
	// suspend the selected account or send the same request to another account.
	return NewRequestScopedError("ultrafast is not permitted on this credential (routing.ultrafast); fallback to other accounts is prohibited", http.StatusForbidden)
}
