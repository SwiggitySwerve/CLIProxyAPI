#!/usr/bin/env bash
# Move *.bak* clutter out of the watched proxy directories
#   ~/.local/share/cli-proxy-{api,claude}/auth/   and   ~/.local/share/cli-proxy-{api,claude}/
# into ~/.local/share/cli-proxy-backups/<timestamp>/ (mirroring the directory layout), so the
# auth watcher stops seeing them. Dry-run is the default: nothing moves without --apply.
#
# Safety: only regular files whose name contains ".bak" and does not end in ".json" are
# moved, only at the top level of those directories. Live credential files (*.json),
# lock files and databases are never touched. A tidy.manifest in the backup dir lists
# "original<TAB>moved-to" so a move can be undone by hand.
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

DATA="$HOME/.local/share"
TS=$(date -u +%Y%m%dT%H%M%SZ)
BACKUP="$DATA/cli-proxy-backups/$TS"

FOUND=()
for inst in "$DATA"/cli-proxy-api "$DATA"/cli-proxy-claude; do
  [ -d "$inst" ] || continue
  for dir in "$inst" "$inst/auth"; do
    [ -d "$dir" ] || continue
    while IFS= read -r -d '' f; do
      case "$f" in *.json) continue ;; esac
      FOUND+=("$f")
    done < <(find "$dir" -maxdepth 1 -type f -name '*.bak*' -print0)
  done
done

if [ "${#FOUND[@]}" -eq 0 ]; then
  echo "Nothing to move."
  exit 0
fi

# Report by location and filename pattern (account names in auth files are collapsed).
for f in "${FOUND[@]}"; do
  rel=${f#"$DATA"/}
  name=${rel##*/}
  dir=${rel%/*}
  case "$dir" in
    */auth) pattern="<account>${name#*.json}" ; [ "$pattern" = "<account>$name" ] && pattern="<account>.*" ;;
    *) pattern=${name%%.bak*}.bak* ;;
  esac
  echo "$dir/$pattern"
done | sort | uniq -c | sed 's/^ *\([0-9]*\) /\1 x /'
echo "total: ${#FOUND[@]} file(s) -> $BACKUP"

if [ "$APPLY" -ne 1 ]; then
  echo "DRY RUN: nothing moved. Re-run with --apply to proceed."
  exit 0
fi

mkdir -p "$BACKUP"
: > "$BACKUP/tidy.manifest"
for f in "${FOUND[@]}"; do
  rel=${f#"$DATA"/}
  mkdir -p "$BACKUP/$(dirname "$rel")"
  mv -n "$f" "$BACKUP/$rel"
  printf '%s\t%s\n' "$f" "$BACKUP/$rel" >> "$BACKUP/tidy.manifest"
done
echo "moved ${#FOUND[@]} file(s); manifest: $BACKUP/tidy.manifest"
