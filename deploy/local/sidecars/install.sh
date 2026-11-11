#!/usr/bin/env bash
# Install the local sidecars (rerank, exporter) and their systemd units to their live
# locations. Shows a diff for every file first; with --apply, backs the originals up to
#   ~/.local/share/cli-proxy-backups/<timestamp>/   (undo with rollback.sh)
# and then copies. Dry-run is the default: nothing is written without --apply.
#
# This script never restarts, enables or disables a unit. After --apply, run
#   systemctl --user daemon-reload
#   systemctl --user restart cli-proxy-exporter   # the exporter reads its code at start
# yourself; the rerank timer picks up the new script on its next run.
set -euo pipefail

APPLY=0
for arg in "$@"; do
  case "$arg" in
    --apply) APPLY=1 ;;
    --dry-run) APPLY=0 ;;
    -h|--help) sed -n '2,11p' "$0"; exit 0 ;;
    *) echo "unknown argument: $arg (use --dry-run or --apply)" >&2; exit 2 ;;
  esac
done

SRC=$(cd "$(dirname "$0")" && pwd)
LOCAL=$(cd "$SRC/.." && pwd)
BIN="$HOME/bin"
UNITS="$HOME/.config/systemd/user"
CFG="$HOME/.config/cli-proxy"
TS=$(date -u +%Y%m%dT%H%M%SZ)
BACKUP="$HOME/.local/share/cli-proxy-backups/$TS"
MANIFEST="$BACKUP/install.manifest"

# source|destination|mode  (rerank/ also holds any set-equal-priorities helper; it is not installed)
FILES=(
  "$SRC/rerank/cli-proxy-rerank|$BIN/cli-proxy-rerank|0700"
  "$SRC/rerank/cli-proxy-rerank.service|$UNITS/cli-proxy-rerank.service|0644"
  "$SRC/rerank/cli-proxy-rerank.timer|$UNITS/cli-proxy-rerank.timer|0644"
  "$SRC/exporter/cli-proxy-exporter|$BIN/cli-proxy-exporter|0700"
  "$SRC/exporter/cli-proxy-exporter.service|$UNITS/cli-proxy-exporter.service|0644"
)

# Config the scripts read. Created only when missing, never overwritten.
SEEDS=()
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

if [ ! -e "$CFG/instances.json" ]; then
  SEEDS+=("$LOCAL/instances.example.json|$CFG/instances.json|0600")
fi
if [ ! -e "$CFG/exporter.json" ]; then
  # Keep the exporter's current listen addresses: read them from the live script so that
  # the host-specific address never has to live in the repo.
  addrs=""
  if [ -f "$BIN/cli-proxy-exporter" ]; then
    addrs=$(grep -E '^LISTEN = ' "$BIN/cli-proxy-exporter" \
      | grep -oE '\("[0-9.]+", [0-9]+\)' \
      | sed -E 's/\("([0-9.]+)", ([0-9]+)\)/["\1", \2]/' | paste -sd, - || true)
  fi
  if [ -z "$addrs" ]; then
    echo "note: no LISTEN found in the live exporter; exporter.json will listen on loopback only" >&2
    addrs='["127.0.0.1", 9787]'
  fi
  printf '{"listen": [%s]}\n' "$addrs" > "$TMP/exporter.json"
  SEEDS+=("$TMP/exporter.json|$CFG/exporter.json|0600")
fi

CHANGED=()
consider() { # src dest mode
  local src=$1 dest=$2
  if [ -e "$dest" ]; then
    if cmp -s "$src" "$dest"; then
      echo "unchanged: $dest"
      return
    fi
    echo "=== $dest (live -> repo)"
    diff -u "$dest" "$src" || true
  else
    echo "=== $dest (new file)"
    if [ "${dest##*/}" = exporter.json ]; then
      echo "(content derived from the live exporter's LISTEN: $(cat "$src"))"
    else
      echo "(from $src)"
    fi
  fi
  CHANGED+=("$src|$dest|$3")
}

for entry in "${FILES[@]}" "${SEEDS[@]}"; do
  IFS='|' read -r src dest mode <<< "$entry"
  consider "$src" "$dest" "$mode"
done

if [ "${#CHANGED[@]}" -eq 0 ]; then
  echo "Nothing to do."
  exit 0
fi

if [ "$APPLY" -ne 1 ]; then
  echo
  echo "DRY RUN: ${#CHANGED[@]} file(s) would be installed; originals would be backed up to $BACKUP"
  echo "Re-run with --apply to proceed."
  exit 0
fi

mkdir -p "$BACKUP/files"
: > "$MANIFEST"
for entry in "${CHANGED[@]}"; do
  IFS='|' read -r src dest mode <<< "$entry"
  rel=${dest#"$HOME"/}
  if [ -e "$dest" ]; then
    mkdir -p "$BACKUP/files/$(dirname "$rel")"
    cp -p "$dest" "$BACKUP/files/$rel"
    echo "restore $rel" >> "$MANIFEST"
  else
    echo "absent $rel" >> "$MANIFEST"
  fi
  mkdir -p "$(dirname "$dest")"
  install -m "$mode" "$src" "$dest"
  echo "installed: $dest"
done
echo
echo "Backup: $BACKUP"
echo "Next (not done by this script): systemctl --user daemon-reload && systemctl --user restart cli-proxy-exporter"
echo "Undo:   $SRC/rollback.sh --from $BACKUP --apply"
