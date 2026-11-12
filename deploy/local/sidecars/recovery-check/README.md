# cli-proxy-recovery-check

Records a masked snapshot of every credential's quota signals and cooldown deadlines, so a
quota incident (roadmap 1.4) can be diagnosed after the fact. The proxy's own error logs
carry only its aggregate 429, not the upstream rate-limit headers, and the proxy keeps
quota signals in memory only.

## What it does

A systemd user timer runs `cli-proxy-recovery-check` every 5 minutes. Each run sends one
read-only `GET <base_url><management_path>/credentials` per instance in
`~/.config/cli-proxy/instances.json` (schema: `deploy/local/instances.example.json`),
using the management key the rerank sidecar uses, and appends one JSON line to
`~/.local/share/cli-proxy-recovery-check/recovery.jsonl` (mode 600; rotates to
`recovery.jsonl.1` above 64 MB). `--out PATH` writes elsewhere. It never calls a
mutating endpoint and never touches the proxy's data directories.

Before this change the script (an earlier version, with no systemd unit referencing it)
wrote email-masked rows to `~/.local/share/cli-proxy-checks/recovery.jsonl`. That file is
left alone; the new data goes to the new directory.

### Interval

5 minutes. A quota 429 sets a cooldown that lasts hours to days, so a 5-minute sample
brackets when it started to within 5 minutes and shows the reset headers that were live
then. Rerank already polls every 10 minutes, which is too coarse to place a cooldown
against an error log. It costs 2 local GETs per run (the management UI polls the same
endpoint every 5 seconds). At about 1.5 KB per credential per run the file grows by about
2 MB a day for 5 credentials, so the 64 MB rotation holds a month or more.

## Record shape

One line per run (values below are placeholders):

```json
{"at": "<UTC time>",
 "credentials": [{
   "instance": "claude-primary",      "auth_index": "<16 hex chars>",
   "provider": "claude",              "status": "error",
   "unavailable": true,               "disabled": false,
   "priority": 80,                    "quota_blocked": true,
   "recovery_stage": "quota_blocked", "success": 0, "failed": 1,
   "cooldowns": [{"scope": "credential", "reason": "credential_quota",
                  "retry_at": "<time>", "remaining_seconds": 1234}],
   "quota": {"observed_at": "<time>",
             "signals": {"Anthropic-Ratelimit-Unified-7d-Status": "rejected",
                         "Anthropic-Ratelimit-Unified-7d-Utilization": "<0..1>",
                         "Anthropic-Ratelimit-Unified-7d-Reset": "<epoch>",
                         "Retry-After": "<seconds>"}},
   "model_quotas": {"<model>": {"observed_at": "<time>",
                                "signals": {"<only headers that differ from quota.signals>": "..."}}}
 }],
 "errors": [{"instance": "<id>", "error": "URLError"}]}
```

`errors` appears only when an instance could not be read; the exit status is non-zero
only if every instance failed. Codex credentials carry `X-Codex-*` signals instead of
`Anthropic-Ratelimit-*`. `model_quotas` keeps per-model headers only where they differ
from the credential-level ones, which is how the Fable-only `7d_oi` window shows up.

### Privacy

Credentials are labelled by instance id and `auth_index` (an opaque hash) only. The script
never copies `email`, `name`, `label`, `account`, `path`, `id_token` or any token field into
a record. Signals are filtered by an allow-list of header names (`Anthropic-Ratelimit-*`,
`X-Codex-*`, `Retry-After`); `Anthropic-Ratelimit-Unified-Upgrade-Paths` is dropped.

## Install

`deploy/local/sidecars/install.sh` copies the script and the two unit files (dry run by
default, backups to `~/.local/share/cli-proxy-backups/<timestamp>/`, undo with
`rollback.sh`). It does not enable anything. On first install:

```bash
systemctl --user daemon-reload
systemctl --user enable --now cli-proxy-recovery-check.timer
```

## Read the data

```bash
# cooldown deadlines per credential over time
jq -c '.at as $t | .credentials[] | select(.cooldowns|length>0)
  | {t:$t, i:.instance, a:.auth_index, c:[.cooldowns[]|{scope,reason,retry_at}]}' recovery.jsonl
```
