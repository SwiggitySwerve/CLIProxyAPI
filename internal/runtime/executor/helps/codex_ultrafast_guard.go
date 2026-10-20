package helps

import (
	"bytes"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
	"io"
	"net/http"
	"strings"
)

// ValidateCodexServiceTier checks the final payload after translation and overrides,
// including requests continuing on a socket already bound to another account.
func ValidateCodexServiceTier(credential *auth.Auth, payload []byte, headerSets ...http.Header) error {
	// Some backend JSON decoders accept the last duplicate key, while gjson.Get
	// reads the first. Check every occurrence so parsing differences cannot
	// change the permitted account after this boundary.
	var tierError error
	gjson.ParseBytes(payload).ForEach(func(key, value gjson.Result) bool {
		if key.String() == executor.ServiceTierMetadataKey {
			tierError = auth.ValidateServiceTierAccount(credential, value.String())
		}
		return tierError == nil
	})
	if tierError != nil {
		return tierError
	}
	for _, headers := range headerSets {
		for name, values := range headers {
			if !strings.EqualFold(name, executor.CodexRoutingHintHeader) {
				continue
			}
			for _, hint := range values {
				for _, field := range strings.Split(hint, ";") {
					key, tier, ok := strings.Cut(field, "=")
					if ok && strings.EqualFold(strings.TrimSpace(key), "tier") {
						if err := auth.ValidateServiceTierAccount(credential, tier); err != nil {
							return err
						}
					}
				}
			}
		}
	}
	return nil
}

// ValidateCodexServiceTierRequest preserves generic HTTP forwarding bodies,
// including callers whose requests have no replay reader.
func ValidateCodexServiceTierRequest(credential *auth.Auth, req *http.Request) error {
	var payload []byte
	if req.Body != nil && req.Body != http.NoBody {
		var err error
		payload, err = io.ReadAll(req.Body)
		_ = req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(payload))
		if req.GetBody != nil {
			req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(payload)), nil }
		}
		if err != nil {
			return err
		}
	}
	return ValidateCodexServiceTier(credential, payload, req.Header)
}
