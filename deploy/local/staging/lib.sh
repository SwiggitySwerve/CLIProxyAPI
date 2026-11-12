#!/usr/bin/env bash
# Shared names, guards and helpers for the staging stack. Sourced, not executed.
# Every podman call below is scoped to the cli-proxy-staging* names and ports.
#
# STAGING_NAME (optional, [a-z0-9]+, not "mock"/"pod") makes a separate stack so
# several can run at once. Unset keeps the original names. Give each stack its own
# STAGING_PROXY_PORT and STAGING_MOCK_PORT; the data dir defaults to match the name:
#   STAGING_NAME  pod                        containers                                 data dir
#   (unset)       cli-proxy-staging-pod      cli-proxy-staging, cli-proxy-staging-mock  .../cli-proxy-staging
#   f7a           cli-proxy-staging-f7a-pod  cli-proxy-staging-f7a, ...-f7a-mock        .../cli-proxy-staging-f7a
set -euo pipefail

STAGING_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "$STAGING_DIR/../../.." && pwd)
NAME=${STAGING_NAME:-}
case "$NAME" in
  "") BASE=cli-proxy-staging ;;
  mock|pod) echo "error: STAGING_NAME must not be '$NAME' (collides with the default names)" >&2; exit 1 ;;
  *[!a-z0-9]*) echo "error: STAGING_NAME must match [a-z0-9]+, got '$NAME'" >&2; exit 1 ;;
  *) BASE=cli-proxy-staging-$NAME ;;
esac
DATA_DIR=${STAGING_DATA_DIR:-$HOME/.local/share/$BASE}
POD=$BASE-pod
PROXY_CT=$BASE
MOCK_CT=$BASE-mock
PROXY_PORT=${STAGING_PROXY_PORT:-18329}
MOCK_PORT=${STAGING_MOCK_PORT:-18330}
IMAGE_REPO=localhost/cli-proxy-api
MOCK_IMAGE=docker.io/library/python:3.12-slim
CRED_A=sk-staging-fake-claude-a
CRED_B=sk-staging-fake-claude-b

die() { echo "error: $*" >&2; exit 1; }

# up.sh holds its lock on fd 9; keep long-lived container processes from inheriting it.
podman() { command podman "$@" 9>&-; }

# Refuse anything that is not a staging name, port or data dir.
guard_name() {
  case "$1" in cli-proxy-staging*) ;; *) die "refusing to touch non-staging container/pod '$1'" ;; esac
}
guard_ports() {
  local p
  for p in "$PROXY_PORT" "$MOCK_PORT"; do
    case "$p" in
      8317|8318|8319|9787|18317) die "port $p belongs to the live deployment" ;;
    esac
  done
  [ "$PROXY_PORT" != "$MOCK_PORT" ] || die "proxy and mock ports must differ"
}
guard_data_dir() {
  case "$(basename "$DATA_DIR")" in cli-proxy-staging*) ;; *) die "STAGING_DATA_DIR must be named cli-proxy-staging*" ;; esac
}
guard_ports
guard_data_dir

ct_exists() { guard_name "$1"; podman container exists "$1"; }
ct_rm() { guard_name "$1"; podman rm -f "$1" >/dev/null 2>&1 || true; }
pod_exists() { guard_name "$1"; podman pod exists "$1"; }

# A port may be in use only by our own pod.
guard_port_free_or_ours() {
  local port=$1
  if ss -ltn "sport = :$port" 2>/dev/null | grep -q LISTEN && ! pod_exists "$POD"; then
    die "port $port is already in use by something outside the staging pod"
  fi
}

# A pod labelled with a different owner belongs to a stack with another data dir or
# ports (e.g. a second agent that forgot STAGING_NAME): refuse instead of clobbering.
# Pods created before the label existed have none and are accepted.
OWNER="$DATA_DIR:$PROXY_PORT:$MOCK_PORT"
guard_pod_owner() {
  local have
  have=$(podman pod inspect --format '{{index .Labels "cli-proxy-staging.owner"}}' "$POD" 2>/dev/null || true)
  [ -z "$have" ] || [ "$have" = "$OWNER" ] ||
    die "pod $POD is already in use by another stack ($have); set a distinct STAGING_NAME (and ports/data dir) or run down.sh for that stack"
}

ensure_pod() {
  if pod_exists "$POD"; then guard_pod_owner; return; fi
  guard_port_free_or_ours "$PROXY_PORT"
  guard_port_free_or_ours "$MOCK_PORT"
  podman pod create --name "$POD" --label "cli-proxy-staging.owner=$OWNER" \
    -p "127.0.0.1:$PROXY_PORT:8317" -p "127.0.0.1:$MOCK_PORT:$MOCK_PORT" >/dev/null
}

random_key() { openssl rand -hex 24; }

# Render config.yaml from the template. Keys are generated once and kept under
# the data dir (mode 600); they never enter the repo.
render_config() {
  umask 077
  mkdir -p "$DATA_DIR/auth"
  [ -s "$DATA_DIR/.management-key" ] || random_key > "$DATA_DIR/.management-key"
  [ -s "$DATA_DIR/.client-key" ] || echo "sk-staging-client-$(random_key)" > "$DATA_DIR/.client-key"
  sed -e "s|__STAGING_MANAGEMENT_KEY__|$(cat "$DATA_DIR/.management-key")|" \
      -e "s|__STAGING_CLIENT_KEY__|$(cat "$DATA_DIR/.client-key")|" \
      -e "s|__MOCK_BASE_URL__|http://127.0.0.1:$MOCK_PORT|" \
      "$STAGING_DIR/config.template.yaml" > "$DATA_DIR/config.yaml"
}

mgmt_key() { cat "$DATA_DIR/.management-key"; }
client_key() { cat "$DATA_DIR/.client-key"; }

start_mock() {
  ct_rm "$MOCK_CT"
  podman run -d --pod "$POD" --name "$MOCK_CT" \
    -v "$STAGING_DIR/mock:/mock:ro" "$MOCK_IMAGE" \
    python -u /mock/mock_upstream.py --port "$MOCK_PORT" >/dev/null
}

# start_proxy <image-ref>: (re)create the proxy container from the given image.
start_proxy() {
  ct_rm "$PROXY_CT"
  podman run -d --pod "$POD" --name "$PROXY_CT" -e TZ=Etc/UTC \
    -v "$DATA_DIR/config.yaml:/CLIProxyAPI/config.yaml:rw" \
    -v "$DATA_DIR/auth:/root/.cli-proxy-api:rw" \
    "$1" ./CLIProxyAPI --local-model >/dev/null
}

wait_http() { # wait_http <url> [seconds]
  local i
  for i in $(seq 1 "${2:-60}"); do
    curl -fsS -o /dev/null "$1" 2>/dev/null && return 0
    sleep 1
  done
  return 1
}

# wait_api_ready <base-url> [seconds]: /healthz answers before credentials load; the API
# routes answer 503 until the initial auth load finishes. An unauthenticated GET
# /v1/models gets 503 while loading and 401 (or 200) once ready.
wait_api_ready() {
  local i code
  for i in $(seq 1 "${2:-60}"); do
    code=$(curl -s -o /dev/null -w '%{http_code}' "$1/v1/models" 2>/dev/null || true)
    case "$code" in 000|503) sleep 1 ;; *) return 0 ;; esac
  done
  return 1
}

proxy_url() { echo "http://127.0.0.1:$PROXY_PORT"; }
mock_url() { echo "http://127.0.0.1:$MOCK_PORT"; }
