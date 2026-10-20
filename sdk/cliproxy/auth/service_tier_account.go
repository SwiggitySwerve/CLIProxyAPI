package auth

import (
	"net/http"
	"strings"
)

// Swervebox's ultrafast entitlement belongs exclusively to this account.
// Keep this deployment policy in the credential owner, including final-payload checks.
const ultrafastAccountEmail = "research@astrabit.io"

// ValidateServiceTierAccount rejects ultrafast on every other credential.
// Other service tiers retain their existing credential eligibility.
func ValidateServiceTierAccount(auth *Auth, tier string) error {
	if !strings.EqualFold(strings.TrimSpace(tier), "ultrafast") {
		return nil
	}
	if auth != nil && strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		email, _ := auth.Metadata["email"].(string)
		if strings.EqualFold(strings.TrimSpace(email), ultrafastAccountEmail) {
			return nil
		}
	}
	// This is a local request policy failure, not a bad credential. Do not
	// suspend the selected account or send the same request to another account.
	return NewRequestScopedError("ultrafast requires "+ultrafastAccountEmail+"; fallback to other accounts is prohibited", http.StatusForbidden)
}
