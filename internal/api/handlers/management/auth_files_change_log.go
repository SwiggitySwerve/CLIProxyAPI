package management

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const logValueRedacted = "(redacted)"

// logCredentialStatusChange records an operator enable/disable. The values go in
// the message text because the file log formatter drops unknown logrus fields.
func logCredentialStatusChange(auth *coreauth.Auth, disabled, previous bool) {
	if auth == nil {
		return
	}
	log.Infof("credential status changed: auth_index=%s provider=%s disabled=%t (was %t)",
		auth.EnsureIndex(), strings.ToLower(strings.TrimSpace(auth.Provider)), disabled, previous)
}

// logCredentialFieldsChange records an operator field patch. Only numbers and
// booleans are printed; every other value (headers, proxy_url, notes) could carry
// a secret and is shown as redacted. previous holds the pre-patch metadata values
// rendered by logFieldValue, keyed like fields.
func logCredentialFieldsChange(auth *coreauth.Auth, fields map[string]json.RawMessage, requestRetry authFileRequestRetryPatch, previous map[string]string) {
	if auth == nil {
		return
	}
	pairs := make(map[string]string, len(fields)+1)
	for key, raw := range fields {
		value, err := decodeAuthFileFieldValue(raw)
		if err != nil {
			value = struct{}{}
		}
		pairs[key] = logFieldValue(value)
	}
	if requestRetry.Set {
		if requestRetry.Value == nil {
			pairs["request_retry"] = "null"
		} else {
			pairs["request_retry"] = strconv.Itoa(*requestRetry.Value)
		}
	}
	keys := make([]string, 0, len(pairs))
	for key := range pairs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var message strings.Builder
	message.WriteString("credential fields changed: auth_index=" + auth.EnsureIndex() + " provider=" + strings.ToLower(strings.TrimSpace(auth.Provider)))
	for _, key := range keys {
		message.WriteString(" " + key + "=" + pairs[key])
		if old, ok := previous[key]; ok && pairs[key] != logValueRedacted {
			message.WriteString(" (was " + old + ")")
		}
	}
	log.Info(message.String())
}

// snapshotLogFieldValues renders the current metadata values of the patched
// top-level fields so the change log can show the old value.
func snapshotLogFieldValues(metadata map[string]any, fields map[string]json.RawMessage) map[string]string {
	previous := make(map[string]string, len(fields)+1)
	for key := range fields {
		if !strings.Contains(key, ".") {
			previous[key] = logFieldValue(metadata[key])
		}
	}
	previous["request_retry"] = logFieldValue(metadata["request_retry"])
	return previous
}

func logFieldValue(value any) string {
	switch typed := value.(type) {
	case nil:
		return "null"
	case bool:
		return strconv.FormatBool(typed)
	case json.Number:
		return typed.String()
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	default:
		return logValueRedacted
	}
}
