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

ROOT="$HOME/.local/share/proxd/backups"
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

PROXD_UNITS="proxd-exporter.service proxd-catalog.service proxd-rerank.timer proxd-recovery-check.timer proxd-claude-ua-sync.timer"
OLD_UNITS="cli-proxy-exporter.service cli-proxy-catalog.service cli-proxy-rerank.timer cli-proxy-recovery-check.timer cli-proxy-claude-ua-sync.timer"
if [ "$APPLY" -ne 1 ]; then
  echo
  echo "DRY RUN: nothing changed. Re-run with --apply to proceed."
  echo "If this undoes the first proxd install (proxd-* files listed as 'would remove') and the proxd-* units"
  echo "are running, stop them before --apply, while their unit files still exist:"
  echo "  systemctl --user disable --now $PROXD_UNITS"
  u=proxd-claude-ua-sync.service  # wait until it neither runs nor has a queued job; an unreadable state keeps waiting
  echo "  while s=\$(systemctl --user show -P ActiveState $u); [ -z \"\$s\" ] || [ \"\$s\" = activating ] || [ \"\$s\" = deactivating ] || ! j=\$(systemctl --user list-jobs --no-legend) || printf '%s\\n' \"\$j\" | grep -qw $u; do sleep 2; done"
  echo "  (a running sync must finish before anything else rewrites the Claude config)"
else
  echo
  echo "Next (not done by this script): systemctl --user daemon-reload"
  echo "If the proxd-* units still run: systemctl --user restart proxd-exporter proxd-catalog"
  echo "If the proxd-* files were removed, go back to the pre-rename units (install.sh never removed them):"
  echo "  First, if proxd-claude-ua-sync already wrote its block, give it the old markers back in place (same"
  echo "  inode; the old sync refuses an unmarked upstream: block):"
  echo "    (f=~/.local/share/proxd/claude/config.yaml; t=\$(mktemp) && trap 'rm -f \"\$t\"' EXIT && sed 's/^# \\(BEGIN\\|END\\) proxd-claude-ua-sync\\( (managed, do not edit)\\)\\?\$/# \\1 cli-proxy-claude-ua-sync\\2/' \"\$f\" > \"\$t\" && cat \"\$t\" > \"\$f\")"
  echo "  systemctl --user enable --now $OLD_UNITS"
  echo "  (starting cli-proxy-claude-ua-sync.timer runs the sync at once and can restart proxd-claude; pick a quiet moment)"
fi
