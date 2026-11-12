#!/usr/bin/env bash
# roll.sh [--approved] [--dry-run] <staging|claude|codex> <image-tag>
#
#   staging  recreate the staging proxy container from <image-tag>.
#   claude   live unit cli-proxy-claude.container (container cli-proxy-claude, port 8319).
#   codex    live unit cli-proxy-api.container    (container cli-proxy-api,    port 8317).
#
# Live targets need --approved. They record the running image to a rollback file,
# rewrite the unit's Image= line, daemon-reload, restart that one unit, smoke-check
# /healthz and print the exact rollback command. --dry-run prints what would change
# and touches nothing. <image-tag> is a full image ref, or a tag of localhost/cli-proxy-api.
set -euo pipefail
. "$(dirname "${BASH_SOURCE[0]}")/../staging/lib.sh"

SELF=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/roll.sh
UNIT_DIR=${LIVE_UNIT_DIR:-$HOME/.config/containers/systemd}
APPROVED=0
DRY_RUN=0
ARGS=()
for a in "$@"; do
  case "$a" in
    --approved) APPROVED=1 ;;
    --dry-run) DRY_RUN=1 ;;
    -*) die "unknown flag $a" ;;
    *) ARGS+=("$a") ;;
  esac
done
[ "${#ARGS[@]}" -eq 2 ] || die "usage: roll.sh [--approved] [--dry-run] <staging|claude|codex> <image-tag>"
TARGET=${ARGS[0]}
case "${ARGS[1]}" in */*) IMAGE=${ARGS[1]} ;; *) IMAGE="$IMAGE_REPO:${ARGS[1]}" ;; esac

mkdir_rollback() { mkdir -p "$DATA_DIR/rollback"; }
record_rollback() { # record_rollback <target> <previous-image>
  mkdir_rollback
  local f="$DATA_DIR/rollback/$1-$(date -u +%Y%m%dT%H%M%SZ).txt"
  printf 'target=%s\nprevious_image=%s\nnew_image=%s\n' "$1" "$2" "$IMAGE" > "$f"
  echo "rollback record: $f"
}

roll_staging() {
  local prev
  prev=$(podman inspect --format '{{.ImageName}}' "$PROXY_CT" 2>/dev/null || true)
  if [ "$DRY_RUN" = 1 ]; then
    echo "dry-run: would recreate $PROXY_CT: ${prev:-<not running>} -> $IMAGE"
    return
  fi
  podman image exists "$IMAGE" || die "image $IMAGE not found (build it with staging/up.sh)"
  [ -s "$DATA_DIR/config.yaml" ] || render_config
  ensure_pod
  [ -z "$prev" ] || record_rollback staging "$prev"
  start_proxy "$IMAGE"
  wait_http "$(proxy_url)/healthz" 60 || die "staging proxy unhealthy after roll; previous image: ${prev:-none}"
  wait_api_ready "$(proxy_url)" 60 || die "staging proxy API not ready after roll; previous image: ${prev:-none}"
  echo "staging rolled to $IMAGE"
  [ -z "$prev" ] || echo "rollback: $SELF staging $prev"
}

roll_live() {
  local unit service container port
  case "$TARGET" in
    claude) unit=cli-proxy-claude.container service=cli-proxy-claude.service container=cli-proxy-claude port=8319 ;;
    codex) unit=cli-proxy-api.container service=cli-proxy-api.service container=cli-proxy-api port=8317 ;;
  esac
  local unit_path="$UNIT_DIR/$unit"
  [ -f "$unit_path" ] || die "unit file $unit_path not found"
  local unit_image running
  unit_image=$(sed -n 's/^Image=//p' "$unit_path" | head -n1)
  [ -n "$unit_image" ] || die "no Image= line in $unit_path"
  running=$(podman inspect --format '{{.ImageName}}' "$container" 2>/dev/null || true)
  local rollback_image=${running:-$unit_image}

  if [ "$DRY_RUN" = 1 ]; then
    echo "dry-run: nothing will be changed"
    echo "  unit:        $unit_path"
    echo "  Image= now:  $unit_image"
    echo "  running:     ${running:-<not running>}"
    echo "  would write: Image=$IMAGE"
    echo "  would run:   systemctl --user daemon-reload && systemctl --user restart $service"
    echo "  would check: curl http://127.0.0.1:$port/healthz"
    echo "  rollback:    $SELF $TARGET $rollback_image --approved"
    return
  fi
  [ "$APPROVED" = 1 ] || die "live target '$TARGET' requires --approved (or use --dry-run)"
  podman image exists "$IMAGE" || die "image $IMAGE not found locally; refusing to roll"

  record_rollback "$TARGET" "$rollback_image"
  sed -i "s|^Image=.*|Image=$IMAGE|" "$unit_path"
  systemctl --user daemon-reload
  systemctl --user restart "$service"
  if wait_http "http://127.0.0.1:$port/healthz" 60 && wait_api_ready "http://127.0.0.1:$port" 60; then
    echo "$TARGET rolled to $IMAGE, /healthz ok, API ready"
  else
    echo "HEALTH CHECK FAILED for $TARGET on port $port" >&2
    echo "rollback now: $SELF $TARGET $rollback_image --approved" >&2
    exit 1
  fi
  echo "rollback: $SELF $TARGET $rollback_image --approved"
}

case "$TARGET" in
  staging) roll_staging ;;
  claude | codex) roll_live ;;
  *) die "target must be staging, claude or codex" ;;
esac
