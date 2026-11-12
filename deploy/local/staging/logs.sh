#!/usr/bin/env bash
# logs.sh [proxy|mock] [podman logs args]: logs of a staging container (default proxy).
set -euo pipefail
. "$(dirname "$0")/lib.sh"

which=${1:-proxy}
[ $# -eq 0 ] || shift
case "$which" in
  proxy) ct=$PROXY_CT ;;
  mock) ct=$MOCK_CT ;;
  *) die "usage: logs.sh [proxy|mock] [podman logs args]" ;;
esac
ct_exists "$ct" || die "$ct is not running (up.sh first)"
exec podman logs "$@" "$ct"
