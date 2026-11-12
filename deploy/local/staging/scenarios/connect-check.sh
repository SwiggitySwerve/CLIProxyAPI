#!/usr/bin/env bash
# connect-check.sh: runs the cli-proxy-connect sidecar against the staging stack.
#
# The mock cannot complete a real OAuth login (Claude and Codex logins only exchange codes
# with the real provider), so the login step is covered only as far as start, poll and
# cancel. Verify, listing and the exporter check run against fake API-key credentials:
#   1. healthy A: all steps pass, the exporter lists it as available
#   2. A cooled by a mock 429: verify fails (exit 1), the listing shows the cooldown and
#      the exporter lists it as cooling
#   3. login with nobody signing in: prints the URL, polls, times out and cancels the session
# A throwaway exporter runs on the host with its own HOME and the staging key only.
set -euo pipefail
. "$(dirname "$0")/common.sh"
require_stack
command -v python3 >/dev/null || die "python3 is required"

SIDECARS=$REPO_ROOT/deploy/local/sidecars
WORK=$(mktemp -d)
EXPORTER_PORT=$((PROXY_PORT + 2))
trap 'kill "${EXPORTER_PID:-0}" 2>/dev/null || true; rm -rf "$WORK"' EXIT

mkdir -p "$WORK/home/.config/cli-proxy-rerank"
mgmt_key > "$WORK/home/.config/cli-proxy-rerank/management-key"
cp "$WORK/home/.config/cli-proxy-rerank/management-key" "$WORK/key"
cat > "$WORK/instances.json" <<EOF
{"instances": [{"id": "claude-staging", "provider": "claude", "base_url": "$(proxy_url)", "management_path": "/v8/management"}]}
EOF
echo "{\"listen\": [[\"127.0.0.1\", $EXPORTER_PORT]]}" > "$WORK/exporter.json"
export CLI_PROXY_INSTANCES=$WORK/instances.json CLI_PROXY_MANAGEMENT_KEY_FILE=$WORK/key
export CLI_PROXY_EXPORTER_CONFIG=$WORK/exporter.json
HOME=$WORK/home CLI_PROXY_EXPORTER_LISTEN=127.0.0.1:$EXPORTER_PORT \
  python3 -I "$SIDECARS/exporter/cli-proxy-exporter" >"$WORK/exporter.log" 2>&1 &
EXPORTER_PID=$!
wait_http "http://127.0.0.1:$EXPORTER_PORT/metrics" 20 || die "throwaway exporter did not start: $(cat "$WORK/exporter.log")"

connect() { python3 -I "$SIDECARS/connect/cli-proxy-connect" claude "$@"; }
mgmt_get() { curl -fsS -H "Authorization: Bearer $(mgmt_key)" "$(proxy_url)/v8/management$1"; }

echo "== 1: healthy credential A =="
restart_proxy
mock_reset
mock_set "$CRED_A" allowed >/dev/null
mock_set "$CRED_B" allowed >/dev/null
auth_a=$(mgmt_get /credentials | jq -r '[.files[] | select(.provider == "claude" and .priority == 10)][0].auth_index')
[ -n "$auth_a" ] && [ "$auth_a" != null ] || fail "could not find credential A in the listing"
out=$(connect --credential "$auth_a" --model claude-staging-model) || fail "healthy connect exited non-zero: $out"
echo "$out"
check "verify passed" grep -q '^\[  ok\] verify' <<<"$out"
check "listing shows quota windows with observed_at" grep -q 'observed_at' <<<"$out"
check "listing shows no cooldown" grep -q 'cooldown    none' <<<"$out"
check "exporter lists A as available" grep -q 'state="available".*auth_index="'"$auth_a"'"' <<<"$out"

echo "== 2: A cooled by a mock 429 =="
mock_set "$CRED_A" rejected-7d '"reset_in":7200' >/dev/null
claude_request >/dev/null # A answers 429 and is cooled, B serves the request
status=0
out=$(connect --credential "$auth_a" --model claude-staging-model) || status=$?
echo "$out"
check "exit status is 1" test "$status" = 1
check "verify failed with the upstream 429" grep -q '^\[FAIL\] verify: HTTP 429' <<<"$out"
check "listing shows the credential cooldown" grep -q 'cooldown    scope credential' <<<"$out"
check "exporter lists A as cooling" grep -q 'state="cooling".*auth_index="'"$auth_a"'"' <<<"$out"

echo "== 3: login start, poll and cancel =="
status=0
out=$(connect --timeout 4 </dev/null) || status=$?
check "login printed an authorization URL" grep -q 'https://' <<<"$out"
check "login timed out with exit 1" test "$status" = 1

echo "PASS (a completed OAuth login is not covered: the mock cannot issue tokens)"
