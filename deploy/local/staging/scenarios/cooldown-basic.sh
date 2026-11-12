#!/usr/bin/env bash
# cooldown-basic.sh [--quick]: two fake Claude credentials; A is rejected on the 7d
# window, B is healthy. Asserts that the proxy serves the request via B and cools A.
#
# Part 1: A's reset is 2h ahead. A is tried first (higher priority), the 429 puts it
#   in cooldown, the request succeeds via B, and follow-up requests skip A.
# Part 2 (skipped by --quick): A's reset is ~20s ahead. A must stay cooled until the
#   mock's reset plus the 1-30s jitter the code adds, then be tried again. This is
#   how the cooldown deadline is checked, because API-key credentials are not
#   listed by GET /v8/management/credentials (so next_retry_after cannot be read).
#
# Template for later scenarios: copy this file, change the mock_set lines and asserts.
set -euo pipefail
. "$(dirname "$0")/common.sh"
require_stack

SLACK=2 # seconds of tolerance for scheduling and clock reads

echo "== part 1: A rejected-7d (reset +2h), B allowed =="
restart_proxy
since=$(date +%s)
mock_reset
reset_at=$(mock_set "$CRED_A" rejected-7d '"reset_in":7200' | jq -r .reset_at)
mock_set "$CRED_B" allowed >/dev/null

body=$(claude_request)
check "request succeeded via B" jq -e --arg c "$CRED_B" '.content[0].text | endswith($c)' <<<"$body"
check "A was tried once and answered 429" jq -e '. == [{"status":429}]' <<<"$(mock_log "$CRED_A" | jq -c '[.[] | {status}]')"
check "B served the retried request" jq -e '. == [{"status":200}]' <<<"$(mock_log "$CRED_B" | jq -c '[.[] | {status}]')"

claude_request >/dev/null
claude_request '"stream":true' >/dev/null
check "A skipped while cooled (still one request)" jq -e 'length == 1' <<<"$(mock_log "$CRED_A")"
check "B served all three requests" jq -e 'length == 3' <<<"$(mock_log "$CRED_B")"

proxy_logs=$(podman logs --since "$since" "$PROXY_CT" 2>&1)
if grep -q 'parsed Anthropic rate limit reset headers' <<<"$proxy_logs"; then
  ok "proxy parsed the Anthropic reset headers (claude_ratelimit.go)"
else
  fail "proxy never parsed Anthropic reset headers"
fi

listed=$(curl -fsS -H "Authorization: Bearer $(mgmt_key)" "$(proxy_url)/v8/management/credentials" | jq '.files | length')
echo "note: /v8/management/credentials lists $listed entries (config API-key credentials are not listed)"

[ "${1:-}" != "--quick" ] || { echo "PASS (quick: timing part skipped)"; exit 0; }

echo "== part 2: A rejected-7d (reset +20s): cooldown ends at reset + 1..30s =="
restart_proxy
mock_reset
reset_at=$(mock_set "$CRED_A" rejected-7d '"reset_in":20' | jq -r .reset_at)
mock_set "$CRED_B" allowed >/dev/null
claude_request >/dev/null
retry_ts=""
for i in $(seq 1 70); do
  claude_request '' "probe $i" >/dev/null # new session each time, so affinity cannot pin B
  retry_ts=$(mock_log "$CRED_A" | jq -r 'if length > 1 then .[1].ts else empty end')
  [ -z "$retry_ts" ] || break
  sleep 1
done
[ -n "$retry_ts" ] || fail "A was not tried again within 70s"
echo "mock reset_at=$reset_at, A retried at $retry_ts"
check "cooldown lasted at least until reset + 1s jitter" \
  awk -v t="$retry_ts" -v r="$reset_at" -v s="$SLACK" 'BEGIN{exit !(t >= r + 1 - 0.2)}'
check "cooldown ended within reset + 30s jitter" \
  awk -v t="$retry_ts" -v r="$reset_at" -v s="$SLACK" 'BEGIN{exit !(t <= r + 30 + s)}'

echo "PASS"
