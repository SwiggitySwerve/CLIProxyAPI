# proxd-claude-ua-sync

Keeps the Claude hub's cloaked Claude Code fingerprint on the current release. The hub
(`proxd-claude`) sends `upstream.claude.header-defaults` upstream for clients it cannot
verify, and Anthropic gates new models on that Claude Code version, so a stale baseline
blocks new models.

An hourly timer (`proxd-claude-ua-sync.timer`) runs it. Each run:

1. runs `claude update` (skipped with `--no-update`);
2. runs the real `claude` once against a local listener and reads the `User-Agent` and
   SDK/runtime headers it sends;
3. if that version is newer than the hub's, rewrites the managed block in
   `~/.local/share/proxd/claude/config.yaml` in place (same inode, because the file is
   bind-mounted), after saving `config.yaml.bak-uasync-<time>` beside it;
4. waits up to 30 s for the hub to log `configuration updated`, and **restarts
   `proxd-claude` if it does not**. A restart drops every running Claude session on the hub.

It never downgrades and edits only its own block. `PROXD_CLAUDE_CONFIG` (or the
pre-rename `CLI_PROXY_CLAUDE_CONFIG`) points it at another config.

## Managed block

```yaml
# BEGIN proxd-claude-ua-sync (managed, do not edit)
# <time>: measured from <claude binary>
upstream:
    claude:
        header-defaults:
            user-agent: "claude-cli/<version> (external, cli)"
            ...
# END proxd-claude-ua-sync
```

Before the rename the markers read `cli-proxy-claude-ua-sync`. Such a block is read as
the current baseline and replaced where it stands at the next update, so a config never
holds two blocks. A config that is already up to date keeps its old markers until then;
the script does not rewrite the file only to rename them.

## Install and test

`deploy/local/sidecars/install.sh` installs the script and both units (dry run by
default) and prints the cutover from `cli-proxy-claude-ua-sync.timer`. Starting the timer
runs the sync at once (its `OnBootSec` has passed), so start it when no Claude session on
the hub would mind a restart.

Tests (no claude, podman or systemctl is run):
`python3 -I deploy/local/sidecars/claude-ua-sync/test_claude_ua_sync.py`.
