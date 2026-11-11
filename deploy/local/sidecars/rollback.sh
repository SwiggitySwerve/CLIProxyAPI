#!/usr/bin/env bash
# Undo install.sh: restore the files it backed up and remove the ones it created.
# Usage: rollback.sh [--from <backup-dir>] [--apply]
# Without --from the newest backup that holds an install.manifest is used.
# Dry-run is the default: nothing is written without --apply. Like install.sh, this
# never restarts a unit; reload and restart them yourself afterwards.
set -euo pipefail

APPLY=0
FROM=""
while [ $# -gt 0 ]; do
  case "$1" in
    --apply) APPLY=1 ;;
    --dry-run) APPLY=0 ;;
    --from) FROM=${2:?--from needs a directory}; shift ;;
    -h|--help) sed -n '2,7p' "$0"; exit 0 ;;
    *) echo "unknown argument: $1 (use --from <dir>, --dry-run or --apply)" >&2; exit 2 ;;
  esac
  shift
done

ROOT="$HOME/.local/share/cli-proxy-backups"
if [ -z "$FROM" ]; then
  for d in $(ls -1d "$ROOT"/*/ 2>/dev/null | sort -r); do
    if [ -f "$d/install.manifest" ]; then FROM=${d%/}; break; fi
  done
fi
if [ -z "$FROM" ] || [ ! -f "$FROM/install.manifest" ]; then
  echo "no install.manifest found (looked in ${FROM:-$ROOT})" >&2
  exit 1
fi
echo "Using backup: $FROM"

while read -r action rel; do
  dest="$HOME/$rel"
  case "$action" in
    restore)
      if [ "$APPLY" -ne 1 ]; then
        echo "=== $dest (live -> backup)"
        diff -u "$dest" "$FROM/files/$rel" || true
      else
        mkdir -p "$(dirname "$dest")"
        cp -p "$FROM/files/$rel" "$dest"
        echo "restored: $dest"
      fi
      ;;
    absent)
      if [ "$APPLY" -ne 1 ]; then
        echo "would remove (did not exist before install): $dest"
      else
        rm -f "$dest"
        echo "removed: $dest"
      fi
      ;;
  esac
done < "$FROM/install.manifest"

if [ "$APPLY" -ne 1 ]; then
  echo
  echo "DRY RUN: nothing changed. Re-run with --apply to proceed."
else
  echo
  echo "Next (not done by this script): systemctl --user daemon-reload && systemctl --user restart cli-proxy-exporter"
fi
