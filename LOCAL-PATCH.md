# Local patch: ultrafast account gate

Base: upstream `v8.0.38` (`0db19025`). Branch: `local/ultrafast-account-gate`.

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

## Verification (v8.0.38, golang:1.26-bookworm)

    gofmt -l <changed files>   # clean
    go build ./...
    go vet ./sdk/cliproxy/auth/... ./sdk/cliproxy/executor/... ./internal/runtime/executor/...
    go test -count=1 ./sdk/cliproxy/auth/... ./sdk/cliproxy/executor/... ./internal/runtime/executor/...

## Related upstream work

- PR #6740 adds `helps/codex_service_tier.go` and downgrades ultrafast for
  non-`promax` plans. The guard files here are named `codex_ultrafast_guard*.go`
  to avoid colliding with it.
