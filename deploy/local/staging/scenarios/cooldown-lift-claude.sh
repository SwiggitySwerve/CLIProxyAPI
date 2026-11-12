#!/usr/bin/env bash
# cooldown-lift-claude.sh: Claude credential A is cooled by a 7d rejection whose reset
# is 2h ahead, then the mock lifts the limit. With quota-reconcile `claude: clear` and
# short intervals, the recheck loop's inference probe sees allowed headers, clears the
# cooldown, and A serves client requests again long before its reset.
#
# Needs the staging harness from feat/local-staging-harness (common.sh, lib.sh, the
# mock) and a proxy image that contains the Claude quota recheck adapter.
set -euo pipefail
. "$(dirname "$0")/common.sh"
require_stack

CFG="$DATA_DIR/config.yaml"
BACKUP=$(mktemp)
cp "$CFG" "$BACKUP"
restore() {
  cp "$BACKUP" "$CFG"
  rm -f "$BACKUP"
  restart_proxy
}
trap restore EXIT

grep -q '^    quota-reconcile: true$' "$CFG" || fail "unexpected quota-reconcile line in $CFG"
sed -i 's/^    quota-reconcile: true$/    quota-reconcile:\n      codex: {mode: clear}\n      claude: {mode: clear, interval: 10s, near-reset-interval: 10s}/' "$CFG"

echo "== A rejected-7d (reset +2h), B allowed; claude recheck in clear mode =="
restart_proxy
since=$(date +%s)
mock_reset
reset_at=$(mock_set "$CRED_A" rejected-7d '"reset_in":7200' | jq -r .reset_at)
mock_set "$CRED_B" allowed >/dev/null

body=$(claude_request '' "before lift")
check "request served via B while A is rejected" jq -e --arg c "$CRED_B" '.content[0].text | endswith($c)' <<<"$body"
check "A answered 429" jq -e 'map(.status) | index(429) != null' <<<"$(mock_log "$CRED_A")"

echo "== lift A at the mock =="
mock_set "$CRED_A" lift >/dev/null
lifted=$(date +%s)
served_ts=""
for i in $(seq 1 90); do
  body=$(claude_request '' "after lift $i") # new session each time, so affinity cannot pin B
  if jq -e --arg c "$CRED_A" '.content[0].text | endswith($c)' <<<"$body" >/dev/null; then
    served_ts=$(date +%s)
    break
  fi
  sleep 1
done
[ -n "$served_ts" ] || fail "A did not serve within 90s of the lift"
echo "lifted at $lifted, A served at $served_ts, mock reset_at $reset_at"
check "A served again before its reset" test "$served_ts" -lt "$((reset_at - 3600))"

proxy_logs=$(podman logs --since "$since" "$PROXY_CT" 2>&1)
if grep -q 'provider reports usage allowed, cleared stale quota cooldown' <<<"$proxy_logs"; then
  ok "the recheck loop cleared A's cooldown"
else
  fail "no recheck clear in the proxy log"
fi

echo "PASS"
