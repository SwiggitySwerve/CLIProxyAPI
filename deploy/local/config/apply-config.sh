#!/usr/bin/env bash
# Apply the Phase 1 config hardening to the proxy config.yaml files (roadmap 1.4, 1.5).
#   apply-config.sh [--apply] [--config PATH]...      show the diff; with --apply, edit
#   apply-config.sh --rollback [--from DIR] [--apply] undo the newest (or given) apply
#
# Dry-run is the default: nothing is written without --apply. With --apply, each original
# is first copied to  ~/.local/share/cli-proxy-backups/<timestamp>/  (override the root with
# CLI_PROXY_BACKUP_ROOT) and the file is then edited IN PLACE (same inode). In-place matters:
# the containers bind-mount config.yaml as a single file, and a rename-style replace would
# leave the running proxy reading the old inode.
#
# Keys set (v8 nested layout; see config-hardening.example.yaml):
#   routing.cooldown.save-cooldown-status       true   persist cooldowns to .cds files (1.5)
#   observability.logs.error-logs-max-files     500    keep this many error-*.log files (1.4)
#   observability.logs.logs-max-total-size-mb   1536   total cap for ALL *.log files (1.4)
# The third is needed because the size cap deletes the oldest .log files, error logs
# included, so 500 error logs cannot survive a 100 MB cap.
# routing.cooldown.quota-reconcile is deliberately left alone (roadmap 1.6).
#
# This script never restarts a container and never prints config values other than these
# keys (diff output is redacted as a second guard). Default targets:
#   ~/.local/share/cli-proxy-api/config.yaml     (Codex instance)
#   ~/.local/share/cli-proxy-claude/config.yaml  (Claude instance)
set -euo pipefail

APPLY=0
ROLLBACK=0
FROM=""
CONFIGS=()
while [ $# -gt 0 ]; do
  case "$1" in
    --apply) APPLY=1 ;;
    --dry-run) APPLY=0 ;;
    --rollback) ROLLBACK=1 ;;
    --from) FROM=${2:?--from needs a directory}; shift ;;
    --config) CONFIGS+=("${2:?--config needs a path}"); shift ;;
    -h|--help) sed -n '2,25p' "$0"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
  shift
done
if [ "${#CONFIGS[@]}" -eq 0 ]; then
  CONFIGS=("$HOME/.local/share/cli-proxy-api/config.yaml" "$HOME/.local/share/cli-proxy-claude/config.yaml")
fi

ROOT=${CLI_PROXY_BACKUP_ROOT:-$HOME/.local/share/cli-proxy-backups}
TS=$(date -u +%Y%m%dT%H%M%SZ)
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

# Show a diff without ever echoing a secret that happens to sit in the context lines.
show_diff() { # old new
  diff -U1 "$1" "$2" | sed -E 's/^([ +-][[:space:]]*(secret-key|api-keys?|api-key|token|password|key)[A-Za-z_-]*:).*/\1 <redacted>/' || true
}
# Rewrite a file in place: same inode, so single-file bind mounts keep seeing it.
write_in_place() { cat "$1" > "$2"; }

if [ "$ROLLBACK" -eq 1 ]; then
  if [ -z "$FROM" ]; then
    for d in $(ls -1d "$ROOT"/*/ 2>/dev/null | sort -r); do
      if [ -f "$d/config.manifest" ]; then FROM=${d%/}; break; fi
    done
  fi
  [ -n "$FROM" ] && [ -f "$FROM/config.manifest" ] || { echo "no config.manifest found (looked in ${FROM:-$ROOT})" >&2; exit 1; }
  echo "Using backup: $FROM"
  while IFS=$'\t' read -r backup dest; do
    echo "=== $dest (live -> backup)"
    show_diff "$dest" "$FROM/$backup"
    if [ "$APPLY" -eq 1 ]; then write_in_place "$FROM/$backup" "$dest"; echo "restored: $dest"; fi
  done < "$FROM/config.manifest"
  echo
  if [ "$APPLY" -eq 1 ]; then
    echo "Restored. The proxy hot-reloads config.yaml; no restart is needed."
  else
    echo "DRY RUN: nothing changed. Re-run with --apply to proceed."
  fi
  exit 0
fi

# The edit: line-based, so comments and managed blocks (for example the claude UA-sync
# block) are untouched. Missing keys are inserted as the first child of their section;
# present keys have only their value replaced. Aborts if the layout is not the v8 nested
# one, or if anything other than the three keys would change.
read -r -d '' EDIT_PY <<'PY' || true
import difflib, re, sys

SETTINGS = [
    (("routing", "cooldown"), "save-cooldown-status", "true"),
    (("observability", "logs"), "error-logs-max-files", "500"),
    (("observability", "logs"), "logs-max-total-size-mb", "1536"),
]


def die(msg):
    sys.stderr.write("error: %s\n" % msg)
    sys.exit(1)


def indent(line):
    return len(line) - len(line.lstrip(" "))


def significant(line):
    s = line.strip()
    return bool(s) and not s.startswith("#")


def key_re(key):
    return re.compile(r"^( *)" + re.escape(key) + r":(.*)$")


def block_end(lines, i):
    for j in range(i + 1, len(lines)):
        if significant(lines[j]) and indent(lines[j]) <= indent(lines[i]):
            return j
    return len(lines)


def find_child(lines, lo, hi, key, want_indent):
    for i in range(lo, hi):
        m = key_re(key).match(lines[i])
        if significant(lines[i]) and m and len(m.group(1)) == want_indent:
            return i
    return None


def section(lines, path):
    lo, hi, want = 0, len(lines), 0
    for name in path:
        i = find_child(lines, lo, hi, name, want)
        if i is None:
            die("section %s not found (v8 nested layout expected)" % ".".join(path))
        if key_re(name).match(lines[i]).group(2).split("#")[0].strip():
            die("section %s is not a plain block" % ".".join(path))
        end = block_end(lines, i)
        kids = [indent(l) for l in lines[i + 1:end] if significant(l)]
        if not kids:
            die("section %s is empty" % ".".join(path))
        lo, hi, want = i + 1, end, min(kids)
    return lo, hi, want


path = sys.argv[1]
orig = open(path).read().split("\n")
lines = list(orig)
for sect, key, value in SETTINGS:
    lo, hi, want = section(lines, sect)
    i = find_child(lines, lo, hi, key, want)
    if i is None:
        lines.insert(lo, " " * want + "%s: %s" % (key, value))
        sys.stderr.write("  %s.%s: (absent) -> %s\n" % (".".join(sect), key, value))
    else:
        m = re.match(r"^(\s*" + re.escape(key) + r":\s*)([^\s#]*)(.*)$", lines[i])
        sys.stderr.write("  %s.%s: %s -> %s\n" % (".".join(sect), key, m.group(2), value))
        lines[i] = m.group(1) + value + m.group(3)

allowed = [key_re(k) for _, k, _ in SETTINGS]
for tag, a0, a1, b0, b1 in difflib.SequenceMatcher(None, orig, lines).get_opcodes():
    if tag == "equal":
        continue
    for l in orig[a0:a1] + lines[b0:b1]:
        if not any(r.match(l) for r in allowed):
            die("refusing: the edit would change a line that is not one of the three keys")
sys.stdout.write("\n".join(lines))
PY

CHANGED=()
i=0
for cfg in "${CONFIGS[@]}"; do
  i=$((i + 1))
  [ -f "$cfg" ] || { echo "config not found: $cfg" >&2; exit 1; }
  echo "=== $cfg"
  python3 -I -c "$EDIT_PY" "$cfg" > "$TMP/new.$i"
  if cmp -s "$cfg" "$TMP/new.$i"; then
    echo "  already applied"
    continue
  fi
  show_diff "$cfg" "$TMP/new.$i"
  CHANGED+=("$i|$cfg")
done

if [ "${#CHANGED[@]}" -eq 0 ]; then echo "Nothing to do."; exit 0; fi

if [ "$APPLY" -ne 1 ]; then
  echo
  echo "DRY RUN: ${#CHANGED[@]} file(s) would change; originals would be backed up to $ROOT/$TS"
  echo "Re-run with --apply to proceed."
  exit 0
fi

BACKUP="$ROOT/$TS"
mkdir -p "$BACKUP"
: > "$BACKUP/config.manifest"
for entry in "${CHANGED[@]}"; do
  IFS='|' read -r n cfg <<< "$entry"
  cp -p "$cfg" "$BACKUP/config.$n.yaml"
  printf 'config.%s.yaml\t%s\n' "$n" "$cfg" >> "$BACKUP/config.manifest"
  before=$(stat -c %i "$cfg")
  write_in_place "$TMP/new.$n" "$cfg"
  [ "$before" = "$(stat -c %i "$cfg")" ] || echo "warning: inode of $cfg changed; a single-file bind mount will not see the edit" >&2
  echo "edited: $cfg"
done
chmod 700 "$BACKUP"
echo
echo "Backup: $BACKUP"
echo "The proxy hot-reloads config.yaml (about a second): no restart is needed for these keys."
echo "Check the reload:  podman logs --since 2m cli-proxy-claude 2>&1 | grep 'config file changed'"
echo "Check persistence: ls ~/.local/share/cli-proxy-claude/auth/*.cds   (one per cooled credential)"
echo "If you want a restart anyway (not done by this script; wait until the .cds files exist):"
echo "  systemctl --user restart cli-proxy-api.service cli-proxy-claude.service"
echo "Undo: $(cd "$(dirname "$0")" && pwd)/$(basename "$0") --rollback --from $BACKUP --apply"
