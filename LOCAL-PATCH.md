# Local patch: ultrafast account gate

Base: upstream `v8.0.65` (`65de937c`). Branch: `local/ultrafast-v8.0.65`.

Rebased from `v8.0.44` on 2026-11-11. Upstream replaced `tidwall/gjson`/`sjson`
with `MoeForge/moego/ojson` and rewrote the Codex executors and usage parsers on
`ojson.Doc`; the patches now use `ojson` and sit beside upstream's new hosted
image-generation check (the tier guard runs right after it).

This is a deployment-specific policy patch, not an upstream feature. Upstream
already forwards the `ultrafast` service tier (#5924). This patch only decides
which credential may serve it.

## Behavior

- Requests with `service_tier=ultrafast` may only be served by a `codex`
  credential whose `Metadata["email"]` equals `ultrafastAccountEmail`
  (`sdk/cliproxy/auth/service_tier_account.go`, currently `research@astrabit.io`).
- Every other tier is unchanged.
- Selection skips ineligible credentials (`authSelectionEligibility.allows`).
- A final-payload and routing-hint guard (`helps.ValidateCodexServiceTier`,
  `executor.CodexRoutingHintHeader`) runs in the codex execute, stream, images
  and websocket executors, so a rewritten or toggled tier cannot reach a
  different account.
- Violations return a request-scoped 403. The credential is not marked failed
  and the request is never retried on another account.

## Known consequence

If the ultrafast account is cooled down or unavailable, ultrafast has no
eligible credential and fails with 403/503 instead of falling back. This is
intentional (fail closed). A stale `usage_limit_reached` cooldown therefore
turns into a hard ultrafast outage until it is cleared with
`POST /v0/management/reset-quota` or a restart.

## Changing the account

`ultrafastAccountEmail` is a compile-time constant and the tests hard-code the
same address. Update both when moving the entitlement.

## Verification (v8.0.65, golang:1.26-bookworm)

    gofmt -l <changed files>   # clean
    go build ./...
    go vet ./sdk/cliproxy/auth/... ./sdk/cliproxy/executor/... ./internal/runtime/executor/...
    go test -count=1 ./sdk/cliproxy/auth/... ./sdk/cliproxy/executor/... ./internal/runtime/executor/...

## Upstream request compression (v8.0.44)

v8.0.44 can zstd-compress Codex request bodies and retries once uncompressed when
upstream rejects them. Each executor builds headers through `prepareHeaders`, and
the guard runs right after it on the plaintext JSON that `cacheHelper` returns,
never on the compressed wire bytes. The retry reuses the same body and the same
`prepareHeaders`, so it cannot carry a tier the guard did not see.
`TestUltrafastGuardRejectsCompressedRequestBeforeUpstream` covers this; removing
the guard makes it fail.

## Claude Code startup probe

Claude Code sends `HEAD /api/hello` to `ANTHROPIC_BASE_URL` each time it starts.
Upstream answers 404 and writes an error log for each probe. `server_routes.go`
answers it with the `/healthz` handler (HEAD only; GET stays unrouted).
Test: `TestClaudeCodeHelloProbe`.

## Related upstream work

- PR #6740 adds `helps/codex_service_tier.go` and downgrades ultrafast for
  non-`promax` plans. The guard files here are named `codex_ultrafast_guard*.go`
  to avoid colliding with it.

## Quota reconciler

Not local-only: proposed upstream as router-for-me/CLIProxyAPI#6997. It is opt-in.
Set `routing.cooldown.quota-reconcile: true` in config.yaml (this deployment does).
`sdk/cliproxy/auth/quota_reconcile.go`: every 30 seconds, and right after a request
is rejected by a cooldown, each Codex credential holding a quota cooldown is checked
against `/backend-api/wham/usage`. It is cleared through `Manager.ResetQuota` only
when `rate_limit.allowed` is true and `limit_reached` is false. Failed, non-200 or
incomplete probes keep the cooldown. Disabled credentials are never probed.
Providers plug in through `QuotaRecheckAdapter` (`quota_recheck.go`, registered per
provider with `Manager.RegisterQuotaRecheckAdapter`; Codex and Claude have one). A cooldown is
cleared only if it is still the one the probe observed (recover time, credential
version and epoch unchanged), so a newer cooldown set mid-probe survives.
Tests: `quota_reconcile_test.go`, `quota_recheck_test.go`.

`quota-reconcile` takes the old bool (`true` is `codex: clear`, `false` is off) or a
per-provider map, for example
`{codex: {mode: clear}, claude: {mode: observe, interval: 30m, near-reset-interval: 5m, near-reset-window: 1h}}`.
`mode` is `off`, `observe` (log the verdict with provider, credential id, verdict and
signals; never clear) or `clear`. Each cooled credential has its own next-check time:
`interval` normally, `near-reset-interval` within `near-reset-window` before its soonest
known reset. Codex defaults reproduce the original loop (every pass, at least 10s
apart, no near window); other providers default to 30m, 5m and 1h, and no interval
goes below 10s. If a credential the loop cleared gets a quota 429 within 10 minutes,
its interval doubles (cap 6h) until its known reset passes. The loop stays idle in
Home mode, and `service_lifecycle.go` still only starts it outside Home mode.
Config: `internal/config/quota_reconcile.go`. Cadence and backoff:
`quota_recheck_schedule.go`. Tests: `quota_recheck_modes_test.go`,
`internal/config/quota_reconcile_test.go`.

The Codex probe URL follows the credential's `base-url` when one is set (a trailing
`/codex` is dropped, so `https://chatgpt.com/backend-api/codex` still maps to
`.../backend-api/wham/usage`); without one it is the chatgpt.com default.

### Claude adapter

`quota_recheck_claude.go`, registered for `claude`. It only rechecks cooldowns set
by a unified rate-limit rejection (credential-scoped `credential_quota`, or a model
state carrying rejected unified windows); ordinary model-level 429s keep their own
short backoff.

- Verdict: one minimal real inference through `Manager.VerifyInference` (16 output
  tokens) on the cheapest registered model (haiku, then sonnet, then the rest). Under
  model-level cooling it skips models that have their own cooldown, because a success
  clears that model through `MarkResult`. Allowed needs a completed 200 whose
  `Anthropic-Ratelimit-Unified-Status` is `allowed` or `allowed_warning`, no window
  `rejected`, and every window that set the cooldown reported as allowed. A 429 or a
  rejected status is limited; `MarkResult` inside `VerifyInference` re-derives the
  deadline once and the loop adds nothing. Everything else is unknown and keeps the
  cooldown. A clear goes through the same guarded `resetQuota` as Codex.
- Usage snapshot (advisory, never clears): `GET /api/oauth/usage` with
  `anthropic-beta: oauth-2025-04-20` through the executor, at most every 10 minutes
  per cooled OAuth credential. A moved reset time (more than 1 minute), a dropped
  percentage, a changed limit severity or `is_active`, or toggled extra usage or spend
  requests an early verdict, at least 2 minutes after the previous one. A failed
  snapshot (including the endpoint's occasional 429) keeps the baseline and doubles
  the snapshot interval, capped at 1 hour.
- In `observe` mode verdicts are logged at info, including unknown ones with their
  reason. Claude is only rechecked when `quota-reconcile` lists it; the legacy `true`
  still means Codex only.

Tests: `quota_recheck_claude_test.go`.

### All-cooled recheck round

When every candidate for a request is cooled, the request runs one recheck round before
it returns the cooldown 429 (`quota_all_cooled.go`, hooked into `executeMixedOnce` and
`executeStreamMixedOnce` where selection fails). It only applies to credentials holding a
quota cooldown whose provider mode is not `off`, never runs while an uncooled candidate
exists, bypasses the per-credential schedule above, and skips Home mode. Providers listed
in `allCooledTrialVerdicts` (Claude) get a half-open trial: the user's own request is sent
on the cooled credential with the oldest last check, then the next, up to
`request-retry + 1` trials. A 200 whose `Anthropic-Ratelimit-Unified-Status` is `allowed`
or `allowed_warning` is served and, in `clear` mode, clears the cooldown (guarded by the
cooldown identity); `observe` serves it but keeps the cooldown. A 429 goes through
`MarkResult` as usual. Other providers with a registered adapter (Codex) get a synchronous
`Verdict` per cooled credential and then selection runs again. Each credential is rechecked
at most once per 60s; inside that gap, and when nothing succeeds, the request returns the
same error it returned before this round existed. Token counting is not covered.
Tests: `quota_all_cooled_test.go`. Staging: `deploy/local/staging/scenarios/all-cooled-one-lifts.sh`.

## Quota recheck probe deadlines

Named exception to the AGENTS.md rule that forbids timeouts once an upstream connection
exists: the quota recheck probe deadlines in `sdk/cliproxy/auth/quota_reconcile.go`
(`quotaProbeTimeout`, 20s, wraps each adapter `Verdict` and each `UsageSnapshot`; for
Claude the verdict is a 16-token inference request, which also runs under it). The probe is a background
check, not a user request, and a hung probe would stall the loop for every other
credential. AGENTS.md cannot change in a PR here (a workflow auto-closes such PRs),
so the exception is recorded here and in the PR body.

## Soonest-reset routing

Local only. Set `routing.strategy: soonest-reset` (also `soonestreset`, `sr`).
It works bare and as the `routing.session-affinity` fallback.
`sdk/cliproxy/auth/selector_soonest_reset.go` takes the same candidates as fill-first
(availability, highest priority tier, Codex websocket preference) and picks the
credential whose longest quota window resets soonest, so quota that is about to
expire is spent first. Lowest rank wins; ties keep ID order:

1. Not observed since start (`Quota.ObservedAt` zero), or the observed reset has
   passed: tried first. A response without quota headers leaves the old snapshot in
   place, so such a credential keeps this rank on every pick until a response with
   headers arrives (meanwhile the strategy keeps choosing it, like fill-first).
2. Known future reset: soonest first.
3. Observed but no usable reset time: last.

Reset time: Claude `Anthropic-Ratelimit-Unified-7d-Reset`, else
`Anthropic-Ratelimit-Unified-Reset`. Codex: the secondary window, else the primary,
using `X-Codex-<Window>-Reset-At`, else `X-Codex-<Window>-Reset-After-Seconds` added to
`ObservedAt` (zero means the window just reset). Other providers have no reset time and fall back to fill-first order.
It only reads the quota headers the hub already stores after each response. It makes
no extra upstream calls and keeps no state.
Tests: `selector_soonest_reset_test.go`, `service_config_soonest_reset_test.go`,
`config_basic_soonest_reset_test.go`.
