# Phase 1 config hardening

`apply-config.sh` prepares the live `config.yaml` changes for roadmap items 1.4 and 1.5
and documents 1.6. It is a dry run unless you pass `--apply`, and it never restarts a
container. Both live instances use the v8 nested layout.

| Roadmap | Key | Live now | Set to |
| --- | --- | --- | --- |
| 1.5 | `routing.cooldown.save-cooldown-status` | absent (false) | `true` |
| 1.4 | `observability.logs.error-logs-max-files` | absent (10) | `500` |
| 1.4 | `observability.logs.logs-max-total-size-mb` | `100` | `1536` |
| 1.6 | `routing.cooldown.quota-reconcile` | `true` | unchanged, see below |

`config-hardening.example.yaml` shows just these keys; none of them is a secret. Never
commit a live config: it holds `api-keys` and a `secret-key`.

## Why a third key

The log cleaner (`internal/logging/log_dir_cleaner.go`) enforces `logs-max-total-size-mb`
over every `*.log` file in the logs directory, `error-*.log` included, deleting the oldest
first. The instances run with a 100 MB cap and `main.log` rotation has no backup limit
(`MaxBackups: 0`), so that cap is the only bound on the main logs. With the cap at 100 MB,
`error-logs-max-files: 500` would be cut to whatever fits in 100 MB. The script therefore
raises the cap to 1536 MB on both instances (the same value keeps the script simple; it is
a ceiling, not a target).

### Disk impact

Measured on the live logs directories (`<auth-dir>/logs`) on 2026-11-11, from the 10 error
logs each instance currently holds:

| Instance | Avg error log | Largest | 500 files | Cap |
| --- | --- | --- | --- | --- |
| Claude (`cli-proxy-claude`) | 2.2 MB | 5.6 MB | about 1.1 GB (worst case 2.8 GB) | 1.5 GB |
| Codex (`cli-proxy-api`) | 0.7 MB | 3.6 MB | about 0.36 GB (worst case 1.8 GB) | 1.5 GB |

The two caps bound the total at 3 GB (the disk has 59 GB free). Expected use is about
1.5 GB for the error logs plus the existing main logs, which today fill roughly 100 MB
per instance. The Claude instance wrote 10 error logs in about 70 minutes during the
current incident, so 500 files cover days, not hours.

## Hot reload or restart

No restart is needed for any of the three keys. The watcher (`internal/watcher/config_reload.go`)
reloads `config.yaml` about 150 ms after a write, and:

- `error-logs-max-files` is applied by `internal/api/server_reload.go` (`SetErrorLogsMaxFiles`);
- `logs-max-total-size-mb` reconfigures the log cleaner on change (same file);
- `save-cooldown-status` swaps the cooldown store in (`sdk/cliproxy/service_auth.go`).

Verified in staging, with the config bind-mounted as a single file as in production: the
proxy logged `config file changed, reloading`, and 16 more failing requests then left 26
error logs where the default had stopped at 10.

Two things to know:

1. **The edit must be in place.** The containers bind-mount `config.yaml` as a single
   file, so a rename-style replace (`mv`, `sed -i`, most editors) leaves the proxy on the
   old inode and the change is silently ignored. The script rewrites the file in place and
   warns if the inode changed.
2. **Enabling persistence does not flush cooldowns already in memory.** The reload swaps
   the store in with `persistOld=false` (`sdk/cliproxy/service_auth.go:677`). In staging,
   no `.cds` file appeared after the reload even with both credentials cooled; the first
   one appeared when a credential was next put into cooldown. So a cooldown that is already
   running when you apply (for example a weekly reset 18 hours out) is still lost by a
   restart, and is relearned with one 429 per credential. Cooldowns set after the apply
   survive. Do not restart on the assumption that current cooldowns are saved; wait for
   `ls <auth-dir>/*.cds` to show the credentials you care about, or accept the one-time 429.

## Apply

```bash
deploy/local/config/apply-config.sh                  # dry run: prints the diff, writes nothing
deploy/local/config/apply-config.sh --apply          # backs up, edits in place
deploy/local/config/apply-config.sh --rollback --apply   # restores the newest backup
```

Originals go to `~/.local/share/cli-proxy-backups/<timestamp>/` (`CLI_PROXY_BACKUP_ROOT`
overrides the root). The script edits only the three keys, line by line, so comments and
the managed UA-sync block in the Claude config stay as they are. It aborts if the layout is
not the v8 nested one or if the edit would touch any other line. Use `--config PATH`
(repeatable) to target other files, for example the staging config.

## 1.6: quota-reconcile stays as it is

`routing.cooldown.quota-reconcile: true` stays on both instances. It is a no-op on the
Claude instance because the reconciler only handles Codex. Once PRs #2 and #5 are
deployed, replace it on each instance with the per-provider map (the bool still parses as
`codex: clear`, so the order of deployment is safe):

```yaml
routing:
  cooldown:
    quota-reconcile:
      codex: {mode: clear}     # Codex instance
      claude: {mode: observe}  # Claude instance: observe first, switch to clear only after
                               # the logs show the Claude probe agrees with real 429s
```

Do this by hand or extend `apply-config.sh` then; nothing here depends on it.
