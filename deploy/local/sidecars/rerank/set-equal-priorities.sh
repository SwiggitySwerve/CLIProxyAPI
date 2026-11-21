#!/usr/bin/env bash
# Put every credential of the Claude instance on one equal priority, so the proxy's
# soonest-reset routing strategy compares them all instead of finding each alone in
# its own priority tier.
#
#   set-equal-priorities.sh [--value N] [--apply]       default value 100
#   set-equal-priorities.sh --restore <file> [--apply]  put the saved priorities back
#
# Every run first reads the current priorities (GET) and saves them to
#   ~/.local/share/proxd/backups/<timestamp>/priorities-<instance-id>.json
# (override the root with PROXD_BACKUPS). That file is what --restore takes.
# Dry-run is the default: only GETs are issued and the planned changes are printed.
# --apply sends one PATCH /credentials/fields per credential that needs changing.
# The instance comes from ~/.config/proxd/instances.json (PROXD_INSTANCES);
# the management key from ~/.config/proxd/management-key (PROXD_MANAGEMENT_KEY_FILE) and is
# never printed. Each PROXD_* variable falls back to its pre-rename CLI_PROXY_* name.
SELF=$0 exec python3 - "$@" <<'PY'
import json
import os
import sys
import time
import urllib.request
from pathlib import Path

HOME = Path.home()


def env_path(name, default):
    """PROXD_<name>, else the pre-rename CLI_PROXY_<name>, else the default path."""
    return Path(os.environ.get(f"PROXD_{name}") or os.environ.get(f"CLI_PROXY_{name}") or default)


INSTANCES = env_path("INSTANCES", HOME / ".config/proxd/instances.json")
KEY_FILE = env_path("MANAGEMENT_KEY_FILE", HOME / ".config/proxd/management-key")
BACKUPS = env_path("BACKUPS", HOME / ".local/share/proxd/backups")
BUILTIN = [{"id": "claude-primary", "provider": "claude", "base_url": "http://127.0.0.1:8319", "management_path": "/v8/management"}]


def main(argv):
    apply = "--apply" in argv
    value, restore = 100, None
    args = [a for a in argv if a != "--apply"]
    while args:
        a = args.pop(0)
        if a == "--value":
            value = int(args.pop(0))
        elif a == "--restore":
            restore = Path(args.pop(0))
        elif a == "--dry-run":
            pass
        else:
            sys.exit(f"unknown argument: {a}")

    try:
        instances = json.loads(INSTANCES.read_text())["instances"]
    except FileNotFoundError:
        instances = BUILTIN
    inst = next((i for i in instances if i["provider"] == "claude"), None)
    if inst is None:
        sys.exit("no claude instance in the instance map")
    base = inst["base_url"].rstrip("/") + inst.get("management_path", "/v8/management")
    key = KEY_FILE.read_text().strip()

    def call(method, path, body=None):
        req = urllib.request.Request(base + path, method=method,
                                     data=json.dumps(body).encode() if body is not None else None,
                                     headers={"Authorization": f"Bearer {key}", "Content-Type": "application/json"})
        with urllib.request.urlopen(req, timeout=30) as resp:
            return json.load(resp)

    current = {c["name"]: c.get("priority") for c in call("GET", "/credentials")["files"]
               if c.get("provider") == "claude"}
    # Credential names embed account names, so output refers to credentials by position only.
    if restore:
        saved = json.loads(restore.read_text())
        if saved.get("instance") != inst["id"]:
            sys.exit(f"{restore.name} is for instance {saved.get('instance')}, not {inst['id']}")
        target = {n: p for n, p in saved["priorities"].items() if p is not None}
        label = "restore"
    else:
        folder = BACKUPS / time.strftime("%Y%m%dT%H%M%SZ", time.gmtime())
        folder.mkdir(parents=True, exist_ok=True)
        out = folder / f"priorities-{inst['id']}.json"
        out.write_text(json.dumps({"instance": inst["id"], "saved_at": time.time(), "priorities": current}, indent=1))
        print(f"saved current priorities: {out}")
        print(f"restore with: {os.environ['SELF']} --restore {out} --apply")
        target = {n: value for n in current}
        label = f"set to {value}"

    changes = [(n, current.get(n), p) for n, p in target.items() if n in current and current[n] != p]
    missing = [n for n in target if n not in current]
    for i, (n, old, new) in enumerate(changes, 1):
        print(f"credential #{i}: priority {old} -> {new}")
    if missing:
        print(f"{len(missing)} saved credential(s) no longer exist on {inst['id']}; skipped")
    if not changes:
        print(f"nothing to change ({len(current)} credential(s) already {label})")
        return
    if not apply:
        print(f"DRY RUN: {len(changes)} PATCH(es) would be sent to {inst['id']}. Re-run with --apply.")
        return
    for n, _, new in changes:
        call("PATCH", "/credentials/fields", {"name": n, "priority": new})
    print(f"applied {len(changes)} change(s) on {inst['id']}")


main(sys.argv[1:])
PY
