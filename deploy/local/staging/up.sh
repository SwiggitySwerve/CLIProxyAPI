#!/usr/bin/env bash
# up.sh [git-ref|worktree]: build the proxy image from a git ref (or the current
# worktree, the default) and start the staging pod: proxy + mock upstream.
# Idempotent: re-running rebuilds if needed and recreates the two containers.
set -euo pipefail
. "$(dirname "$0")/lib.sh"

SRC=${1:-worktree}
BUILD_CTX=$REPO_ROOT
DIRTY=""
if [ "$SRC" = worktree ]; then
  SHA=$(git -C "$REPO_ROOT" rev-parse --short=8 HEAD)
  [ -z "$(git -C "$REPO_ROOT" status --porcelain)" ] || DIRTY="-dirty"
else
  SHA=$(git -C "$REPO_ROOT" rev-parse --verify --short=8 "$SRC^{commit}") || die "unknown git ref '$SRC'"
  mkdir -p "$DATA_DIR/build"
  BUILD_CTX=$(mktemp -d "$DATA_DIR/build/src.XXXXXX")
  trap 'rm -rf "$BUILD_CTX"' EXIT
  git -C "$REPO_ROOT" archive "$SRC" | tar -x -C "$BUILD_CTX"
fi
IMAGE="$IMAGE_REPO:staging-$SHA$DIRTY"

if [ -z "$DIRTY" ] && podman image exists "$IMAGE"; then
  echo "image $IMAGE already built"
else
  echo "building $IMAGE from $SRC"
  podman build -t "$IMAGE" --build-arg VERSION=staging --build-arg "COMMIT=$SHA" -f "$BUILD_CTX/Dockerfile" "$BUILD_CTX"
fi

render_config
ensure_pod
start_mock
start_proxy "$IMAGE"
wait_http "$(mock_url)/healthz" 30 || die "mock did not become healthy (deploy/local/staging/logs.sh mock)"
wait_http "$(proxy_url)/healthz" 60 || die "proxy did not become healthy (deploy/local/staging/logs.sh)"

echo "staging up: image=$IMAGE"
echo "  proxy  $(proxy_url)   (client and management keys under $DATA_DIR)"
echo "  mock   $(mock_url)   (admin: /admin/scenario, /admin/log)"
