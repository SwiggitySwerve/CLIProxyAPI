#!/usr/bin/env bash
# all-cooled-one-lifts.sh: two fake Claude credentials, both rejected on the 7d window
# far ahead. Asserts the all-cooled recheck round (roadmap 2.0c):
#   1. a request returns 429 while both are cooled;
#   2. a second request runs one half-open trial per credential (both still 429) and
#      starts the 60s per-credential gap;
#   3. after B is lifted in the mock, requests inside the gap do not reach upstream,
#      and within 60s (plus slack) of the trial round a request is served by B;
#   4. B is then healthy: the next request is served by B without trialling A.
# Needs the staging harness from feat/local-staging-harness. It switches the rendered
# staging config to quota-reconcile {codex: clear, claude: clear}; up.sh re-renders it.
set -euo pipefail
. "$(dirname "$0")/common.sh"
require_stack

GAP=60
SLACK=10

# status_request <user-text>: POST a Claude message; prints "<status> <body>".
status_request() {
  local out
  out=$(curl -sS -w '\n%{http_code}' -X POST "$(proxy_url)/v1/messages" \
    -H "x-api-key: $(client_key)" -H 'content-type: application/json' \
    -H 'anthropic-version: 2023-06-01' \
    -d "{\"model\":\"claude-staging-model\",\"max_tokens\":32,\"messages\":[{\"role\":\"user\",\"content\":\"$1\"}]}")
  echo "${out##*$'\n'} ${out%$'\n'*}"
}
count() { mock_log "$1" | jq 'length'; }

echo "== setup: quota-reconcile claude: clear, A and B rejected-7d (reset +6d) =="
sed -i 's|^    quota-reconcile: .*$|    quota-reconcile: {codex: {mode: clear}, claude: {mode: clear}}|' "$DATA_DIR/config.yaml"
grep -q 'claude: {mode: clear}' "$DATA_DIR/config.yaml" || fail "could not set quota-reconcile in $DATA_DIR/config.yaml"
restart_proxy
mock_reset
mock_set "$CRED_A" rejected-7d '"reset_in":518400' >/dev/null
mock_set "$CRED_B" rejected-7d '"reset_in":518400' >/dev/null

read -r status body <<<"$(status_request "all cooled 1")"
check "first request returns 429 (got $status)" test "$status" = 429
check "A and B each answered one 429" test "$(count "$CRED_A")-$(count "$CRED_B")" = 1-1

read -r status body <<<"$(status_request "all cooled 2")"
trial_round=$(date +%s)
check "all-cooled request still returns 429 (got $status)" test "$status" = 429
check "the round trialled each credential once" test "$(count "$CRED_A")-$(count "$CRED_B")" = 2-2
check "the 429 is the proxy's cooldown error" grep -q 'cooling down' <<<"$body"

echo "== lift B in the mock =="
mock_set "$CRED_B" lift >/dev/null
read -r status body <<<"$(status_request "inside the gap")"
check "inside the 60s gap the request returns 429 (got $status)" test "$status" = 429
check "inside the gap nothing reached upstream" test "$(count "$CRED_A")-$(count "$CRED_B")" = 2-2

served_at=""
for i in $(seq 1 $(((GAP + SLACK) / 5))); do
  sleep 5
  read -r status body <<<"$(status_request "poll $i")"
  if [ "$status" = 200 ]; then
    served_at=$(date +%s)
    break
  fi
done
[ -n "$served_at" ] || fail "no request was served within $((GAP + SLACK))s of the trial round"
elapsed=$((served_at - trial_round))
echo "served $elapsed s after the trial round"
check "served by B" jq -e --arg c "$CRED_B" '.content[0].text | endswith($c)' <<<"$body"
check "served within the 60s gap plus ${SLACK}s" test "$elapsed" -le $((GAP + SLACK))
check "B was trialled exactly once more" test "$(count "$CRED_B")" = 3
check "A was trialled at most once more" test "$(count "$CRED_A")" -le 3

a_before=$(count "$CRED_A")
body=$(claude_request '' "after the lift")
check "next request served by B through normal selection" jq -e --arg c "$CRED_B" '.content[0].text | endswith($c)' <<<"$body"
check "A not trialled while B is healthy" test "$(count "$CRED_A")" = "$a_before"

proxy_logs=$(podman logs --since "$trial_round" "$PROXY_CT" 2>&1)
if grep -q 'half-open trial says usage is allowed, cleared stale quota cooldown' <<<"$proxy_logs"; then
  ok "proxy logged the guarded clear"
else
  fail "proxy did not log the half-open clear"
fi

echo "PASS"
