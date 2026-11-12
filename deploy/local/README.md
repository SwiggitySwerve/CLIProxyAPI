# Local deployment tooling

Tools for building, testing and rolling the proxy on a single host with podman.

```
deploy/local/
  bin/gotest.sh          gofmt -l + go vet + go test in a Go container (no host Go needed)
  bin/roll.sh            roll an image to staging or a live quadlet unit
  staging/               isolated staging stack: proxy + mock upstream in one pod
    up.sh down.sh status.sh logs.sh
    config.template.yaml proxy config template (placeholders only)
    mock/mock_upstream.py mock Anthropic + Codex upstream (Python stdlib)
    scenarios/           end-to-end scripts; cooldown-basic.sh is the template, connect-check.sh covers cli-proxy-connect
```

## Staging

Staging is a real proxy built from any ref, talking to a mock upstream with scripted
quota behaviour. It uses fake credentials only and never touches live containers,
ports or data.

| Thing | Name |
| --- | --- |
| Pod / containers | `cli-proxy-staging-pod`, `cli-proxy-staging`, `cli-proxy-staging-mock` |
| Proxy (host) | `127.0.0.1:18329` (`STAGING_PROXY_PORT`) |
| Mock upstream and admin (host) | `127.0.0.1:18330` (`STAGING_MOCK_PORT`) |
| Data, config, keys | `~/.local/share/cli-proxy-staging/` (`STAGING_DATA_DIR`) |
| Image | `localhost/cli-proxy-api:staging-<shortsha>` (`-dirty` if the worktree has changes) |

```bash
deploy/local/staging/up.sh               # build the current worktree and start
deploy/local/staging/up.sh <git-ref>     # build a ref (git archive, no checkout) and start
deploy/local/staging/status.sh
deploy/local/staging/logs.sh [proxy|mock] [-f]
deploy/local/staging/down.sh [--purge]   # --purge also deletes the data dir and keys
```

All scripts are idempotent and refuse names or ports outside staging (for example 8317,
8318, 8319, 9787, 18317).

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

`scenarios/connect-check.sh` exercises `sidecars/connect/cli-proxy-connect` (verify, listing,
exporter check, and the login start/poll/cancel; a completed OAuth login is not covered).

A scenario sources `scenarios/common.sh` (helpers: `mock_set`, `mock_log`,
`claude_request`, `check`, `restart_proxy`) and exits non-zero on the first failed check.
Copy `cooldown-basic.sh` for new ones. Session affinity is on, so requests with the same
first user text stay on one credential; pass different text to start a new session.

## Roll and rollback

```bash
deploy/local/bin/roll.sh staging <image-tag>
deploy/local/bin/roll.sh --dry-run claude <image-tag>      # print only, touches nothing
deploy/local/bin/roll.sh --approved claude <image-tag>     # live: cli-proxy-claude.container
deploy/local/bin/roll.sh --approved codex  <image-tag>     # live: cli-proxy-api.container
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
