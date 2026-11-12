# cli-proxy-connect

One command to connect an account to a proxy instance and prove it works (roadmap 3.1).
Today a new credential means a login in the right container, then a hand-check of the
auth file, priority, rerank, exporter and the other places that list credentials. This
script does the login through the v8 management API and then reads back what the proxy
and the exporter say about the new credential.

```
cli-proxy-connect <provider> [instance] [--credential ID] [--model M]
                  [--no-exporter | --exporter-url URL] [--timeout SECONDS]
```

`provider` is the OAuth provider (`claude`, `codex`, ...). `instance` is an id from the
instance map (`~/.config/cli-proxy/instances.json`, schema `deploy/local/instances.example.json`);
it may be omitted when exactly one instance serves the provider. The management key is
read from the file the other sidecars use (`~/.config/cli-proxy-rerank/management-key`).

## Steps

1. **oauth**: `GET /v8/management/oauth/auth-url?provider=<p>&verify=true`, prints the
   login URL and polls `/oauth/status` until `ok` or `error`. If the browser ends on a
   page that cannot reach the proxy, paste that page's full URL into the terminal (or pipe
   it on stdin); it is sent to `POST /oauth/callback`. On timeout, error or Ctrl-C the
   pending session is cancelled. The proxy gives up after 5 minutes.
2. **verify**: `POST /v8/management/credentials/verify`, an account-pinned 1-token
   inference. This is the only request that spends quota.
3. **listing**: prints id, `auth_index`, provider, `instance_id`, priority, models, quota
   windows with `observed_at` (per-model windows only where they differ, for example
   `7d_oi`), and the cooldown state (`status`, `quota_blocked`, `next_retry_after`, every
   cooldown with scope, reason and deadline).
4. **exporter**: one `GET /metrics` must contain `cliproxy_credential_state` for the
   credential's instance and `auth_index`. The URL comes from the exporter's own config
   (`~/.config/cli-proxy/exporter.json`, `CLI_PROXY_EXPORTER_LISTEN`; loopback listener
   preferred, default `127.0.0.1:9787`). `--exporter-url` overrides it; `--no-exporter`
   skips the step when no exporter is configured.

Each step prints `[  ok]`, `[FAIL]` or `[skip]`. Exit status is 0 only when every step that
ran passed. A failed verify (for example a quota 429) still prints the listing, so the
cooldown shows. `--credential ID` skips the login and runs steps 2 to 4 on an existing
credential (id, name or `auth_index`), which is also how to re-check an account later.

## Safety

The only writes are the OAuth login itself (the proxy saves its own auth file) and the
verify inference. The script never edits config, priorities, exclusions or auth files.
It prints the account email shown in the listing, so keep its output out of shared logs.

## Install and test

`deploy/local/sidecars/install.sh` copies it to `~/bin/cli-proxy-connect` (dry run by
default; undo with `rollback.sh`). Tests, no proxy needed:
`python3 -I deploy/local/sidecars/connect/test_connect.py`. Against the staging stack:
`deploy/local/staging/scenarios/connect-check.sh`. The staging mock cannot complete a real
OAuth login, so that scenario covers login start, poll and cancel plus steps 2 to 4 on
fake API-key credentials.

Environment overrides: `CLI_PROXY_INSTANCES`, `CLI_PROXY_MANAGEMENT_KEY_FILE`,
`CLI_PROXY_EXPORTER_CONFIG`, `CLI_PROXY_EXPORTER_LISTEN`.
