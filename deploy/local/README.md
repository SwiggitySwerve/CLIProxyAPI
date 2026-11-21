# Local deployment tooling

Tools for building, testing and rolling the proxy (ProxD) on a single host with podman.
The live services are `proxd-codex` (Codex, port 8317), `proxd-claude` (Claude, port 8319)
and `proxd-gemini` (Gemini, port 8321). Their data directories are
`~/.local/share/proxd/{codex,claude,gemini}` and the image repository is `localhost/proxd`.
Before the host cutover these were `~/.local/share/cli-proxy-{api,claude,gemini}` and
`localhost/cli-proxy-api`; the cutover leaves symlinks and image tags at the old names.

```
deploy/local/
  bin/gotest.sh          gofmt -l + go vet + go test in a Go container (no host Go needed)
  bin/roll.sh            roll an image to staging or a live quadlet unit
  staging/               isolated staging stack: proxy + mock upstream in one pod
    up.sh down.sh status.sh logs.sh
    config.template.yaml proxy config template (placeholders only)
    mock/mock_upstream.py mock Anthropic + Codex upstream (Python stdlib)
    scenarios/           end-to-end scripts; cooldown-basic.sh is the template, connect-check.sh covers proxd-connect
  sidecars/              host sidecars installed to ~/bin and ~/.config/systemd/user as proxd-*
    rerank/ exporter/ recovery-check/ catalog/ connect/ claude-ua-sync/
    install.sh rollback.sh tidy-auth-backups.sh
  config/                config hardening for the instances' config.yaml (apply-config.sh)
```

## Sidecars after the ProxD rename

The sidecars read and write the `proxd` layout:

| What | Path | Override (pre-rename name still read) |
| --- | --- | --- |
| Instance map | `~/.config/proxd/instances.json` | `PROXD_INSTANCES` (`CLI_PROXY_INSTANCES`) |
| Management key | `~/.config/proxd/management-key` | `PROXD_MANAGEMENT_KEY_FILE` (`CLI_PROXY_MANAGEMENT_KEY_FILE`) |
| Exporter listen config | `~/.config/proxd/exporter.json` | `PROXD_EXPORTER_CONFIG`, `PROXD_EXPORTER_LISTEN` |
| Plans | `~/.config/proxd/plans.json` | `PROXD_PLANS` |
| Catalog config | `~/.config/proxd/catalog/{catalog,extra-models}.json` | `PROXD_CATALOG_CONFIG` (catalog.json) |
| Instance data | `~/.local/share/proxd/{codex,claude,gemini}` | `PROXD_CLAUDE_CONFIG`, `PROXD_CODEX_AUTH_DIR` |
| Rerank state, recovery log | `~/.local/share/proxd/{rerank,recovery-check}` | `PROXD_RERANK_STATE` |
| Backups | `~/.local/share/proxd/backups/<timestamp>/` | `PROXD_BACKUPS`, `PROXD_BACKUP_ROOT` |

`sidecars/install.sh` (dry run by default) installs the `proxd-*` scripts and units next to
the old `cli-proxy-*` ones and never enables, disables or restarts a unit. It prints the
cutover commands (`disable --now cli-proxy-<x>`, `enable --now proxd-<x>`) for you to run.
The exporter publishes every metric as `proxd_*` (see `sidecars/exporter/README.md`). Starting `proxd-claude-ua-sync.timer` runs the sync at once,
which can restart `proxd-claude`.

## Staging

Staging is a real proxy built from any ref, talking to a mock upstream with scripted
quota behaviour. It uses fake credentials only and never touches live containers,
ports or data.

| Thing | Name |
| --- | --- |
| Pod / containers | `proxd-staging-pod`, `proxd-staging`, `proxd-staging-mock` |
| Proxy (host) | `127.0.0.1:18329` (`STAGING_PROXY_PORT`) |
| Mock upstream and admin (host) | `127.0.0.1:18330` (`STAGING_MOCK_PORT`) |
| Data, config, keys | `~/.local/share/proxd-staging/` (`STAGING_DATA_DIR`, must be named `proxd-staging*`) |
| Image | `localhost/proxd:staging-<shortsha>` (`-dirty` if the worktree has changes) |

```bash
deploy/local/staging/up.sh               # build the current worktree and start
deploy/local/staging/up.sh <git-ref>     # build a ref (git archive, no checkout) and start
deploy/local/staging/status.sh
deploy/local/staging/logs.sh [proxy|mock] [-f]
deploy/local/staging/down.sh [--purge]   # --purge also deletes the data dir and keys
```

All scripts are idempotent and refuse names or ports outside staging (for example 8317,
8318, 8319, 8321, 9787, 18317).

Several stacks can run at once: set `STAGING_NAME=<name>` (`[a-z0-9]+`, not `mock` or
`pod`) plus distinct `STAGING_PROXY_PORT` / `STAGING_MOCK_PORT` for every script, including
`scenarios/*` and `bin/roll.sh staging`. The pod becomes `proxd-staging-<name>-pod`, the
containers `proxd-staging-<name>` and `proxd-staging-<name>-mock`, and the data dir
defaults to `~/.local/share/proxd-staging-<name>/`. Unset keeps the names above. `up.sh`
refuses a concurrent run for the same name, and refuses a pod already owned by a different
data dir or ports, so two agents cannot clobber each other's containers.

```bash
export STAGING_NAME=f7a STAGING_PROXY_PORT=18351 STAGING_MOCK_PORT=18352
deploy/local/staging/up.sh && deploy/local/staging/scenarios/cooldown-basic.sh
```

`up.sh` renders `config.template.yaml` into the data dir and generates a random
management key and client key there (mode 600, never in the repo). The config mirrors
the live Claude instance's routing: `strategy: soonest-reset`, session affinity on,
`quota-reconcile: true`. Two Claude credentials (A at priority 10, B at priority 0) and
one Codex credential are API-key entries whose `base-url` is the mock.

### Mock upstream

Per-credential scenarios are set over HTTP; the credential is the fake API key.

```bash
M=http://127.0.0.1:18330
curl -X POST $M/admin/scenario -d '{"credential":"sk-staging-fake-claude-a","mode":"rejected-7d","reset_in":7200}'
curl -X POST $M/admin/scenario -d '{"credential":"sk-staging-fake-claude-a","mode":"lift"}'
curl $M/admin/log?credential=sk-staging-fake-claude-a   # request log
curl -X POST $M/admin/reset                              # clear scenarios and log
```

Modes: `allowed`, `warning` (with `percent`), `rejected-7d`, `rejected-5h`,
`rejected-7d_oi`, `lift` (rejected becomes allowed). Optional `reset_in` (seconds) or
`reset_at` (epoch) and `retry_after`. It serves `POST /v1/messages` (JSON and SSE),
`/v1/messages/count_tokens`, Codex `POST /responses` (SSE or JSON) and `GET /wham/usage`,
with the `anthropic-ratelimit-unified-*` and `x-codex-primary-*` / `x-codex-secondary-*`
headers for the active scenario.

### Scenarios

```bash
deploy/local/staging/scenarios/cooldown-basic.sh          # ~25s
deploy/local/staging/scenarios/cooldown-basic.sh --quick  # skips the timing part
```

`scenarios/connect-check.sh` exercises `sidecars/connect/proxd-connect` (verify, listing,
exporter check, and the login start/poll/cancel; a completed OAuth login is not covered).

A scenario sources `scenarios/common.sh` (helpers: `mock_set`, `mock_log`,
`claude_request`, `check`, `restart_proxy`) and exits non-zero on the first failed check.
Copy `cooldown-basic.sh` for new ones. Session affinity is on, so requests with the same
first user text stay on one credential; pass different text to start a new session.

## Building an image

```bash
deploy/local/bin/build-image.sh --dry-run <git-ref|worktree> <image-ref>   # print the plan only
VERSION=v8.0.65 deploy/local/bin/build-image.sh <git-ref> localhost/proxd:v8.0.65-<shortsha>
```

Build images with this script, not with a plain `podman build` of the Dockerfile. A plain
build downloads the modules and compiles inside image layers, which is gigabytes of
writes; on a host with slow storage one such build stalled IO long enough to restart
unrelated services. The script unpacks the source on tmpfs, compiles there with the
Dockerfile's own builder image and `go build` line (module cache, build cache and `/tmp`
all on tmpfs), then runs the Dockerfile with the builder stage replaced by the compiled
binary, so only the final stage's layers reach the disk. It refuses a Dockerfile whose
builder stage no longer has the shape it expects.

Each step first waits, for up to ten minutes, until the kernel's IO pressure
(`/proc/pressure/io`, `full avg10`) is below 5, and gives up if `full avg60` is 15 or
more. `VERSION` sets `main.Version` (default `staging`); `BUILD_CACHE_DIR` reuses an
existing Go cache instead of a throwaway one. The compile uses 1.5 to 3 GB
of memory under `/dev/shm` and leaves nothing there afterwards. tmpfs is memory, and
memory under pressure is swapped to disk, so the script refuses to start with less than
6 GB available (`BUILD_MIN_MEM_MB`); on a host whose swap is on slow storage, also keep
other tmpfs use low while it runs. `staging/up.sh` builds through
this script.

## Roll and rollback

```bash
deploy/local/bin/roll.sh staging <image-tag>
deploy/local/bin/roll.sh --dry-run claude <image-tag>      # print only, touches nothing
deploy/local/bin/roll.sh --approved claude <image-tag>     # live: proxd-claude.container
deploy/local/bin/roll.sh --approved codex  <image-tag>     # live: proxd-codex.container
```

A live roll records the running image under `<data dir>/rollback/`, rewrites the unit's
`Image=` line, runs `daemon-reload`, restarts only that unit, smoke-checks `/healthz`
and prints the rollback command, which is the same script with the previous image:
`roll.sh --approved <target> <previous-image>`. Without `--approved` a live target is
refused. If the health check fails the script exits non-zero and prints the rollback
command; it does not roll back by itself.

## Tests

`deploy/local/bin/gotest.sh [packages...]` (default `./...`) fails on any file `gofmt -l`
lists, then runs `go vet` and `go test`.

## Rule: never copy live auth

Staging uses fake keys that exist only in the template. Never copy or mount anything from
the live auth directories into staging. OAuth refresh tokens rotate on use, so a refresh
from staging would invalidate the production credential.

## Known limits

- `GET /v8/management/credentials` does not list config API-key credentials (only auth
  files and runtime-only entries), so the cooldown deadline is checked by behaviour, not
  by `next_retry_after`.
- The quota reconciler probes a hardcoded `https://chatgpt.com/backend-api/wham/usage`
  (`sdk/cliproxy/auth/quota_reconcile.go`), so the mock's `/wham/usage` is not reached by
  the proxy. If a staging Codex credential is cooled, the reconciler sends one probe with
  the fake key to the real host.
- Credentials cannot be OAuth files here: the Claude and Codex executors take a custom
  base URL only from API-key config entries.
