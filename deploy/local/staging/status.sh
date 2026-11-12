#!/usr/bin/env bash
# status.sh: show the staging pod, its containers and health. Exit 1 if down.
set -euo pipefail
. "$(dirname "$0")/lib.sh"

if ! pod_exists "$POD"; then
  echo "staging: down"
  exit 1
fi
podman ps -a --filter "pod=$POD" --format '{{.Names}}\t{{.Image}}\t{{.Status}}'
rc=0
if curl -fsS -o /dev/null "$(proxy_url)/healthz"; then echo "proxy  healthy $(proxy_url)"; else echo "proxy  UNHEALTHY"; rc=1; fi
if curl -fsS -o /dev/null "$(mock_url)/healthz"; then echo "mock   healthy $(mock_url)"; else echo "mock   UNHEALTHY"; rc=1; fi
exit "$rc"
