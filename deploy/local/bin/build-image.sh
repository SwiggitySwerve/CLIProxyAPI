#!/usr/bin/env bash
# build-image.sh [--dry-run] <git-ref|worktree> <image-ref>
#
# Builds the proxy image while writing almost nothing to disk. A plain `podman build`
# of the Dockerfile downloads the modules and compiles inside image layers; on a host
# with slow storage that stalled IO long enough to restart unrelated services.
#
#   1. The source is unpacked on tmpfs: `git archive` of the ref, or for `worktree` the
#      tracked and untracked files of this checkout.
#   2. The binary is compiled in the Dockerfile's builder image with the Dockerfile's own
#      `go build` line, with the module cache, the build cache and /tmp all on tmpfs.
#   3. `podman build` runs the same Dockerfile with the builder stage replaced by that
#      binary (--build-context builder=...), so only the final stage's layers are written.
#
# Both steps wait for IO pressure to be low before they start. Environment:
#   VERSION          value for main.Version (default staging)
#   BUILD_TMP        tmpfs directory to work in (default /dev/shm); emptied afterwards.
#                    Needs about 3 GB free; tmpfs is memory, so leave headroom.
#   BUILD_CACHE_DIR  existing Go cache to reuse (with mod/ and build/); it is kept
#   BUILD_PSI_START  start only below this `full avg10` IO pressure (default 5)
#   BUILD_PSI_ABORT  give up at or above this `full avg60` IO pressure (default 15)
set -euo pipefail
. "$(dirname "${BASH_SOURCE[0]}")/../staging/lib.sh"

DRY_RUN=0
ARGS=()
for a in "$@"; do
  case "$a" in
    --dry-run) DRY_RUN=1 ;;
    -*) die "unknown flag $a" ;;
    *) ARGS+=("$a") ;;
  esac
done
[ "${#ARGS[@]}" -eq 2 ] || die "usage: build-image.sh [--dry-run] <git-ref|worktree> <image-ref>"
SRC=${ARGS[0]}
IMAGE=${ARGS[1]}
BUILD_TMP=${BUILD_TMP:-/dev/shm}
PSI_START=${BUILD_PSI_START:-5}
PSI_ABORT=${BUILD_PSI_ABORT:-15}

if [ "$SRC" = worktree ]; then
  COMMIT=$(git -C "$REPO_ROOT" rev-parse --short=8 HEAD)
  [ -z "$(git -C "$REPO_ROOT" status --porcelain)" ] || COMMIT="$COMMIT-dirty"
  dockerfile=$(cat "$REPO_ROOT/Dockerfile")
else
  COMMIT=$(git -C "$REPO_ROOT" rev-parse --verify --short=8 "$SRC^{commit}") || die "unknown git ref '$SRC'"
  dockerfile=$(git -C "$REPO_ROOT" show "$SRC:Dockerfile")
fi
BUILD_DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ)
export VERSION=${VERSION:-staging} COMMIT BUILD_DATE

# Take the builder image and the compile command from the Dockerfile, so this script
# cannot drift from it. Anything else in the builder stage would be skipped silently,
# so refuse a Dockerfile whose shape is not the one this script was written for.
builder_image=$(awk 'toupper($1) == "FROM" && tolower($NF) == "builder" { print $2 }' <<<"$dockerfile")
build_cmd=$(sed -n 's/^RUN \(.*go build .*\)$/\1/p' <<<"$dockerfile")
[ -n "$builder_image" ] && [ "$(wc -l <<<"$builder_image")" -eq 1 ] || die "Dockerfile: expected one 'FROM <image> AS builder' line"
[ -n "$build_cmd" ] && [ "$(wc -l <<<"$build_cmd")" -eq 1 ] || die "Dockerfile: expected one 'RUN ... go build ...' line"
grep -qxF 'WORKDIR /app' <<<"$dockerfile" || die "Dockerfile: the builder stage no longer works in /app"
grep -qF -- '-o ./CLIProxyAPI ' <<<"$build_cmd" || die "Dockerfile: the build no longer writes ./CLIProxyAPI"
grep -qxF 'COPY --from=builder ./app/CLIProxyAPI /CLIProxyAPI/CLIProxyAPI' <<<"$dockerfile" ||
  die "Dockerfile: the final stage no longer copies ./app/CLIProxyAPI from the builder"
case "$builder_image" in */*) ;; *) builder_image=docker.io/library/$builder_image ;; esac

echo "source:   $SRC ($COMMIT)"
echo "image:    $IMAGE (main.Version=$VERSION)"
echo "compile:  in $builder_image, caches and /tmp on tmpfs under $BUILD_TMP"
echo "          $build_cmd"
echo "assemble: podman build with the builder stage replaced by the compiled binary"
[ "$DRY_RUN" = 0 ] || { echo "dry-run: nothing was built"; exit 0; }

[ "$(stat -f -c %T "$BUILD_TMP")" = tmpfs ] || die "BUILD_TMP=$BUILD_TMP is not on tmpfs"

psi() { awk -v f="$1" '$1 == "full" { for (i = 2; i <= NF; i++) { split($i, kv, "="); if (kv[1] == f) print kv[2] } }' /proc/pressure/io; }
at_least() { awk -v a="$1" -v b="$2" 'BEGIN { exit !(a >= b) }'; }
io_gate() { # io_gate <step>: wait up to ten minutes for low IO pressure before a step
  local i
  [ -r /proc/pressure/io ] || { echo "no /proc/pressure/io: not checking IO pressure before $1"; return; }
  for i in $(seq 1 60); do
    if at_least "$(psi avg60)" "$PSI_ABORT"; then
      die "IO pressure is too high to $1 (full avg60 $(psi avg60), limit $PSI_ABORT); try later"
    fi
    at_least "$(psi avg10)" "$PSI_START" || return 0
    [ "$i" != 1 ] || echo "waiting to $1: IO pressure full avg10 is $(psi avg10), need below $PSI_START"
    sleep 10
  done
  die "IO pressure stayed at or above $PSI_START for ten minutes; did not $1"
}

WORK=$(mktemp -d "$BUILD_TMP/cli-proxy-build.XXXXXX")
# The Go module cache is read-only, so make it writable before removing it.
trap 'chmod -R u+w "$WORK" 2>/dev/null; rm -rf "$WORK"' EXIT
CACHE=${BUILD_CACHE_DIR:-$WORK/cache}
APP=$WORK/builder/app # the path the final stage copies from: <builder>/app/CLIProxyAPI
mkdir -p "$APP" "$CACHE/mod" "$CACHE/build"

if [ "$SRC" = worktree ]; then
  git -C "$REPO_ROOT" ls-files -z --cached --others --exclude-standard |
    tar -C "$REPO_ROOT" --null --files-from=- --ignore-failed-read -cf - 2>/dev/null | tar -xf - -C "$APP"
else
  git -C "$REPO_ROOT" archive "$SRC" | tar -xf - -C "$APP"
fi

io_gate "compile"
ionice -c3 nice -n10 podman run --rm --tmpfs /tmp:rw,exec,size=3g \
  -v "$APP":/app -w /app -v "$CACHE/mod":/go/pkg/mod -v "$CACHE/build":/root/.cache/go-build \
  -e VERSION -e COMMIT -e BUILD_DATE "$builder_image" sh -euc "$build_cmd"
[ -x "$APP/CLIProxyAPI" ] || die "the compile step did not produce $APP/CLIProxyAPI"
echo "compiled; tmpfs in use: $(du -shc "$WORK" "$CACHE" 2>/dev/null | tail -1 | cut -f1)"

io_gate "assemble the image"
ionice -c3 nice -n10 podman build -q -t "$IMAGE" -f "$APP/Dockerfile" --build-context "builder=$WORK/builder" \
  --build-arg "VERSION=$VERSION" --build-arg "COMMIT=$COMMIT" --build-arg "BUILD_DATE=$BUILD_DATE" "$APP" >/dev/null
echo "built $IMAGE ($(podman image inspect --format '{{.Size}}' "$IMAGE" | awk '{ printf "%.0f MB", $1 / 1000000 }'))"
