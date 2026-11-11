#!/usr/bin/env bash
# Shared names, guards and helpers for the staging stack. Sourced, not executed.
# Every podman call below is scoped to the cli-proxy-staging* names and ports.
set -euo pipefail

STAGING_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "$STAGING_DIR/../../.." && pwd)
DATA_DIR=${STAGING_DATA_DIR:-$HOME/.local/share/cli-proxy-staging}
POD=cli-proxy-staging-pod
PROXY_CT=cli-proxy-staging
MOCK_CT=cli-proxy-staging-mock
PROXY_PORT=${STAGING_PROXY_PORT:-18329}
MOCK_PORT=${STAGING_MOCK_PORT:-18330}
IMAGE_REPO=localhost/cli-proxy-api
MOCK_IMAGE=docker.io/library/python:3.12-slim
CRED_A=sk-staging-fake-claude-a
CRED_B=sk-staging-fake-claude-b

die() { echo "error: $*" >&2; exit 1; }

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

ensure_pod() {
  if pod_exists "$POD"; then return; fi
  guard_port_free_or_ours "$PROXY_PORT"
  guard_port_free_or_ours "$MOCK_PORT"
  podman pod create --name "$POD" \
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

proxy_url() { echo "http://127.0.0.1:$PROXY_PORT"; }
mock_url() { echo "http://127.0.0.1:$MOCK_PORT"; }
