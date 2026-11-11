#!/usr/bin/env bash
# Helpers shared by scenario scripts. Sourced, not executed. Scenarios assume
# the stack is up (staging/up.sh) and exit non-zero on the first failed check.
. "$(dirname "${BASH_SOURCE[0]}")/../lib.sh"

fail() { echo "FAIL: $*" >&2; exit 1; }
ok() { echo "ok: $*"; }
check() { # check <description> <command...>
  local desc=$1
  shift
  if "$@" >/dev/null; then ok "$desc"; else fail "$desc"; fi
}

# mock_set <credential> <mode> [json-extra]: set a scenario and print its JSON.
# Example: mock_set "$CRED_A" rejected-7d '"reset_in":7200'
mock_set() {
  curl -fsS -X POST "$(mock_url)/admin/scenario" \
    -d "{\"credential\":\"$1\",\"mode\":\"$2\"${3:+,$3}}"
}
mock_reset() { curl -fsS -X POST "$(mock_url)/admin/reset" >/dev/null; }
# mock_log [credential]: JSON array of request log entries.
mock_log() {
  curl -fsS "$(mock_url)/admin/log${1:+?credential=$1}" | jq -c '.entries'
}

# claude_request [extra-json-fields] [user-text]: POST a Claude message through the proxy.
# Session affinity is on, so requests with the same first user text share a session and
# stay on one credential; pass different user text to start a new session.
# Prints the response body; fails the scenario on a non-200.
claude_request() {
  local out status
  out=$(curl -sS -w '\n%{http_code}' -X POST "$(proxy_url)/v1/messages" \
    -H "x-api-key: $(client_key)" -H 'content-type: application/json' \
    -H 'anthropic-version: 2023-06-01' \
    -d "{\"model\":\"claude-staging-model\",\"max_tokens\":32${1:+,$1},\"messages\":[{\"role\":\"user\",\"content\":\"${2:-hi}\"}]}")
  status=${out##*$'\n'}
  [ "$status" = 200 ] || fail "proxy returned HTTP $status: ${out%$'\n'*}"
  echo "${out%$'\n'*}"
}

# Restart only the staging proxy container: drops all in-memory cooldown state.
restart_proxy() {
  ct_exists "$PROXY_CT" || die "$PROXY_CT is not running"
  podman restart "$PROXY_CT" >/dev/null
  wait_http "$(proxy_url)/healthz" 60 || die "proxy did not come back after restart"
}

require_stack() {
  curl -fsS -o /dev/null "$(proxy_url)/healthz" && curl -fsS -o /dev/null "$(mock_url)/healthz" ||
    die "staging is not up; run deploy/local/staging/up.sh first"
}
