#!/usr/bin/env bash
# Install the local sidecars (rerank, exporter, recovery-check, catalog, connect, claude-ua-sync) and their
# systemd units to their live locations as proxd-*. Shows a diff for every file first; with --apply, backs the
# originals up to
#   ~/.local/share/proxd/backups/<timestamp>/   (undo with rollback.sh)
# and then copies. Dry-run is the default: nothing is written without --apply.
#
# This script never restarts, enables or disables a unit, and leaves the pre-rename cli-proxy-* units
# and scripts alone. After --apply it prints the daemon-reload and the commands that move each
# cli-proxy-* unit to its proxd-* unit; run them yourself when you choose (the cutover).
# Once the proxd-* units run, re-installing only needs a restart of proxd-exporter and proxd-catalog
# (they read their code at start); the timers pick up new scripts on their next run.
set -euo pipefail

APPLY=0
for arg in "$@"; do
  case "$arg" in
    --apply) APPLY=1 ;;
    --dry-run) APPLY=0 ;;
    -h|--help) sed -n '2,12p' "$0"; exit 0 ;;
    *) echo "unknown argument: $arg (use --dry-run or --apply)" >&2; exit 2 ;;
  esac
done

SRC=$(cd "$(dirname "$0")" && pwd)
LOCAL=$(cd "$SRC/.." && pwd)
BIN="$HOME/bin"
UNITS="$HOME/.config/systemd/user"
CFG="$HOME/.config/proxd"
TS=$(date -u +%Y%m%dT%H%M%SZ)
BACKUP="$HOME/.local/share/proxd/backups/$TS"
MANIFEST="$BACKUP/install.manifest"

# source|destination|mode  (rerank/ also holds any set-equal-priorities helper; it is not installed)
FILES=(
  "$SRC/rerank/proxd-rerank|$BIN/proxd-rerank|0700"
  "$SRC/rerank/proxd-rerank.service|$UNITS/proxd-rerank.service|0644"
  "$SRC/rerank/proxd-rerank.timer|$UNITS/proxd-rerank.timer|0644"
  "$SRC/exporter/proxd-exporter|$BIN/proxd-exporter|0700"
  "$SRC/exporter/proxd-exporter.service|$UNITS/proxd-exporter.service|0644"
  "$SRC/recovery-check/proxd-recovery-check|$BIN/proxd-recovery-check|0700"
  "$SRC/recovery-check/proxd-recovery-check.service|$UNITS/proxd-recovery-check.service|0644"
  "$SRC/recovery-check/proxd-recovery-check.timer|$UNITS/proxd-recovery-check.timer|0644"
  "$SRC/catalog/proxd-catalog|$BIN/proxd-catalog|0700"
  "$SRC/catalog/proxd-catalog.service|$UNITS/proxd-catalog.service|0644"
  "$SRC/connect/proxd-connect|$BIN/proxd-connect|0700"
  "$SRC/claude-ua-sync/proxd-claude-ua-sync|$BIN/proxd-claude-ua-sync|0700"
  "$SRC/claude-ua-sync/proxd-claude-ua-sync.service|$UNITS/proxd-claude-ua-sync.service|0644"
  "$SRC/claude-ua-sync/proxd-claude-ua-sync.timer|$UNITS/proxd-claude-ua-sync.timer|0644"
)

# Pre-rename unit -> proxd unit. install.sh only prints these moves (print_cutover); it never runs them.
CUTOVER=(
  "cli-proxy-exporter.service|proxd-exporter.service"
  "cli-proxy-catalog.service|proxd-catalog.service"
  "cli-proxy-rerank.timer|proxd-rerank.timer"
  "cli-proxy-recovery-check.timer|proxd-recovery-check.timer"
  "cli-proxy-claude-ua-sync.timer|proxd-claude-ua-sync.timer"
)
wait_for() { # a shell loop that waits until a oneshot service neither runs nor has a queued job
  # Fails closed: an unreadable state keeps it waiting.
  echo "while s=\$(systemctl --user show -P ActiveState $1); [ -z \"\$s\" ] || [ \"\$s\" = activating ] || [ \"\$s\" = deactivating ] || ! j=\$(systemctl --user list-jobs --no-legend) || printf '%s\\n' \"\$j\" | grep -qw $1; do sleep 2; done"
}
print_cutover() {
  echo "Cutover (not done by this script; run when you choose):"
  echo "  systemctl --user daemon-reload"
  if [ -d "$UNITS/cli-proxy-exporter.service.d" ] && [ ! -e "$UNITS/proxd-exporter.service.d" ]; then
    echo "  cp -a $UNITS/cli-proxy-exporter.service.d $UNITS/proxd-exporter.service.d   # host drop-in, e.g. wait-cni0.conf"
  fi
  local entry old new
  for entry in "${CUTOVER[@]}"; do
    IFS='|' read -r old new <<< "$entry"
    if [ -e "$UNITS/$old" ] && [ "$old" = cli-proxy-claude-ua-sync.timer ]; then
      # Two syncs rewriting the bind-mounted config in place at once can corrupt it: let a running one finish.
      echo "  systemctl --user disable --now $old && $(wait_for cli-proxy-claude-ua-sync.service) && systemctl --user enable --now $new"
    elif [ -e "$UNITS/$old" ]; then
      echo "  systemctl --user disable --now $old && systemctl --user enable --now $new"
    else
      echo "  systemctl --user enable --now $new"
    fi
  done
  echo "Starting a timer whose OnBootSec has passed runs its service at once. proxd-claude-ua-sync then runs"
  echo "claude update and, if the hub does not hot-reload its config, restarts proxd-claude: start that timer"
  echo "when no Claude session on the hub would mind. The cli-proxy-* files stay in place for rollback."
}

# A host whose config is still in the pre-rename dir must move it to ~/.config/proxd first;
# seeding here would shadow its instances.json and exporter.json (listen addresses).
for f in instances.json exporter.json; do
  if [ -e "$HOME/.config/cli-proxy/$f" ] && [ ! -e "$CFG/$f" ]; then
    echo "~/.config/cli-proxy/$f exists but $CFG/$f does not: move it into $CFG first" >&2
    exit 1
  fi
done

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
  for live in "$BIN/proxd-exporter" "$BIN/cli-proxy-exporter"; do
    if [ -z "$addrs" ] && [ -f "$live" ]; then
      addrs=$(grep -E '^LISTEN = ' "$live" \
        | grep -oE '\("[0-9.]+", [0-9]+\)' \
        | sed -E 's/\("([0-9.]+)", ([0-9]+)\)/["\1", \2]/' | paste -sd, - || true)
    fi
  done
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
  print_cutover
  exit 0
fi

if [ "$APPLY" -ne 1 ]; then
  echo
  echo "DRY RUN: ${#CHANGED[@]} file(s) would be installed; originals would be backed up to $BACKUP"
  echo "Re-run with --apply to proceed. After it:"
  print_cutover
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
print_cutover
echo "Already cut over: systemctl --user daemon-reload && systemctl --user restart proxd-exporter proxd-catalog"
echo "Undo:  $SRC/rollback.sh --from $BACKUP --apply"
