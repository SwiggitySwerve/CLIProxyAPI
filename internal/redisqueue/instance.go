package redisqueue

import "strings"

// deploymentInstanceID is operator-supplied provenance, never an account or
// credential-derived identity. Missing/invalid configuration stays unattributed.
func deploymentInstanceID(value string) string {
	value = strings.TrimSpace(value)
	if len(value) == 0 || len(value) > 64 || value[0] < 'a' || value[0] > 'z' {
		return ""
	}
	for _, c := range value {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return ""
		}
	}
	return value
}
