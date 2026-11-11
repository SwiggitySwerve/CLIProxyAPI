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
