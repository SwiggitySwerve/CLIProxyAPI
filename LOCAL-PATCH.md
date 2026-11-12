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
  credential listed under `routing.ultrafast` in config
  (`sdk/cliproxy/auth/service_tier_account.go`). A credential matches when its
  Codex `account_id` (auth file metadata) is in `account-ids`, its credential id
  (the auth file name) is in `auth-ids`, or its `email` metadata is in `emails`.
  Prefer `account-ids`; `emails` is a fallback and is used only when listed.
- **Unset means denied.** If `routing.ultrafast` is missing or every list is empty,
  ultrafast is denied for every credential (fail closed). The operator must set the
  key before rolling out a build with this change, or ultrafast stops working.
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
`POST /v8/management/routing/cooldown/reset` (body `{"auth_index": "..."}`) or a restart.

## Changing the account

The entitlement lives in config, not in source; no rebuild is needed to move it.
Edit `routing.ultrafast` in `config.yaml` (applied on config reload; the policy is replaced
on every reload, so removing an entry revokes it immediately):

    routing:
      ultrafast:
        account-ids: ["<codex-account-id>"]

The value for `account-ids` is the `account_id` field in the target Codex auth
file. Read it from the auth file itself, or from the credential listing, and do
not paste it into commits, issues or PR text.

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
provider with `Manager.RegisterQuotaRecheckAdapter`; `NewManager` registers the Codex and Claude
adapters from the provider registry, `provider_registry.go`). A cooldown is
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
exists, bypasses the per-credential schedule above, and skips Home mode. Providers whose
registry entry has a `trialVerdict` (Claude) get a half-open trial: the user's own request is sent
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

1. No current observation: tried first. That is never observed since start
   (`Quota.ObservedAt` zero; with `save-cooldown-status: true` a restored observation
   counts as observed), observed longer ago than the staleness threshold, or the
   observed reset has passed. A response without quota headers leaves the old snapshot
   in place, so such a credential keeps this rank on every pick until a response with
   headers arrives (meanwhile the strategy keeps choosing it, like fill-first).
2. Known future reset: soonest first.
3. Observed recently but no usable reset time: last.

Staleness: an observation older than `soonestResetStaleAfter` (6h) is Unknown and
ranks in the refresh bucket, whatever its reset time or signals say, so an idle
credential gets traffic and a new observation instead of ranking on an old snapshot.
6h is just over the 5h Claude window: an older snapshot has outlived a whole short
window. Without `save-cooldown-status` observations are in memory only, so after a
restart every credential is unobserved; a restored one is still judged by its age. The check uses the same `now` the pick uses.

Reset time comes from a per-provider function, the `resetWindow` of the credential's
provider in the provider registry (`provider_registry.go`). Each function names its windows and returns the reset time
to rank on plus the window name:

- Claude (`Anthropic-Ratelimit-Unified-<window>-Reset`, epoch seconds), first usable
  of: `7d_oi` (only for Fable models), `7d`, `unified`. `5h` is named but never ranked
  on. The `7d_oi` window is model-scoped, so it never ranks a model outside its scope;
  the model test is `claudeModelHas7dOIWindow` (the executor tells `7d_oi` apart only
  by response headers, so there was no existing model check to reuse). Whether the
  Fable window really is `7d_oi` is a measure-first item in the roadmap.
- Codex: `secondary`, else `primary`, using `X-Codex-<Window>-Reset-At`, else
  `X-Codex-<Window>-Reset-After-Seconds` added to `ObservedAt` (zero means the window
  just reset).
- Other providers have no entry, so no reset time, and fall back to fill-first order.
It only reads the quota headers the hub already stores after each response. It makes
no extra upstream calls and keeps no state.
Tests: `selector_soonest_reset_test.go`, `service_config_soonest_reset_test.go`,
`config_basic_soonest_reset_test.go`.

## Persisted quota observations

Active only with `routing.cooldown.save-cooldown-status: true` and never under Home
(the cooldown store is nil there). Each `.cds` record may carry an `observation`
object: `signals` (quota headers only, re-filtered through
`isQuotaSignalHeaderForProvider` on write and on restore), `observed_at` and
`source` (`response_headers`). A credential or model that is not cooling gets an
observation-only record with `status: observed` and a zero `next_retry_after`.
Restore sets only `Quota.Signals`/`ObservedAt` (newest wins) and never creates,
extends or shortens a cooldown. Saves caused only by an observation change are
throttled to one per 30s; cooldown saves carry the latest observations, and
shutdown flushes pending ones. A crash loses at most the observations since the
last save. Older builds ignore the `observation` field and the observation-only
records (their `next_retry_after` is zero) and drop them on their next save.
Code: `sdk/cliproxy/auth/quota_observation_state.go`; tests:
`quota_observation_state_test.go`.

## Provider registry (3.2) — inventory and plan

Roadmap 3.2 replaces provider-name switches with a registry, one decision group per
slice, each behind characterization tests written and run green on the unchanged code.

Inventory (base `local/ultrafast-v8.0.65` at 8ada42e7): a grep for `case`/`==`/`!=`/`EqualFold` on
provider string literals in `sdk/` and `internal/`, excluding `internal/translator/`
and `internal/runtime/executor/`, gives 204 lines in 75 functions across 43 files,
plus three provider-keyed maps. Grouped by the decision they make:

1. **Quota observation and recheck (slice 1, done).** Now one `providerQuota` entry
   per provider in `sdk/cliproxy/auth/provider_registry.go`:
   - `quota_signals.go:20` `ProviderSupportsQuotaObservation` (claude, codex, devin);
     its callers are unchanged: `quota_signals.go:42`, `cooldown_evidence_log.go:36`,
     `quota_observation_state.go:92`, `internal/api/handlers/management/auth_files.go:898,918`.
   - `selector_soonest_reset.go:60` `soonestResetWindows` (claude, codex).
   - `quota_all_cooled.go:28` `allCooledTrialVerdicts` (claude), read at :151, :185, :202.
   - `conductor.go:290-291` default `QuotaRecheckAdapter`s registered by `NewManager`.
   Tests: `provider_quota_characterization_test.go`.
2. **Quota-signal parsing into named windows (slice 2, done; 2b done in slice 4).** The
   `providerQuota` entry gained three fields (`provider_registry.go:16-25`):
   - `signalRank`: the per-provider header allowlist and retention rank, formerly
     `isQuotaSignalHeaderForProvider` and `quotaSignalRetentionRank`. Now
     `quota_signals.go:181` `claudeSignalRank` and `:186` `codexSignalRank` (+ `:214`
     `hasCodexLimitMarker`), read by `collectQuotaSignals` at `:118`.
   - `rejects`: Claude's `hasRejectedClaudeUnifiedSignals`, read by
     `rejectedClaudeSignalsForCooldown` at `quota_signals.go:92`.
     `sanitizePersistedClaudeCooldownSignals` (`:104`) keeps its Claude-only
     unified-prefix filter and reaches the rule through that function. Both keep
     their names, so their `conductor_cooldown.go` call sites are untouched.
   - `recheckCadence`: Codex's probe-every-pass default, read by
     `quotaRecheckPolicyFor` at `quota_recheck_schedule.go:61`. Providers without
     one keep the slow `claudeRecheck*` defaults (`:20-22`).
   Tests: the `SignalAllowlist`, `SignalRetentionOrder`, `SignalRankOrder` (the rank of
   each header class per provider), `RejectedCooldownSignals` and `RecheckCadence` cases
   in `provider_quota_characterization_test.go`.

   **Slice 2b (done in slice 4): legacy `quota-reconcile: true` = codex clear.**
   `internal/config/quota_reconcile.go` `QuotaReconcileFromBool` (parse) and
   `MarshalYAML` (save back as `true`) now ask `providerspec.LegacyQuotaReconcileProvider()`.
   The import cycle (`sdk/cliproxy/auth` imports `internal/config`) is broken by the
   leaf package `internal/providerspec`, which imports nothing from this module.
   Tests: `quota_reconcile_legacy_characterization_test.go`.
3. **429 classification and cooldown scope (slice 3, done).** Whether a provider's
   rejection signals pin its cooldowns, are restored on restart and are echoed on the
   429 the hub returns is now `providerPinsCooldownSignals` (`provider_registry.go`),
   true when the entry has a `rejects` rule (Claude only). It replaced the `"claude"`
   literals in `selector.go` `newModelCooldownErrorWithCause` (was
   `isClaudeCooldownProvider`) and `claudeUnifiedCooldownForAuth`, `scheduler.go`
   `claudeCooldownSummaryLocked`, and both signal restores in `conductor_cooldown.go`
   `restoreCooldownRecordLocked`. The unified-rejection capture already went through
   the registry (`rejectedClaudeSignalsForCooldown`, slice 2).
   The signal *format* stays Claude's: `hasRejectedClaudeUnifiedSignals`, the
   unified-prefix filter in `sanitizePersistedClaudeCooldownSignals` and
   `appendClaudeUnifiedHeaders` read and write Anthropic header names. They become
   per-provider only when a second provider gets a `rejects` rule.
   Reclassified from this group: `conductor_cooldown.go` `providerCoolingOverrideForAuth`
   has no provider literal (it reads the OpenAI-compatible config);
   `conductor_request_scoped_errors.go` maps a provider to its config key section
   (group 7); `isTerminalRefreshError`'s Codex `invalid_grant` rule
   (`conductor_cooldown.go`) is refresh behaviour (slice 4).
   Tests: `CooldownSignalGates` in `provider_quota_characterization_test.go`.
4. **Authenticator, login and refresh (slice 4, done).** The exported part of the
   registry is `internal/providerspec` (`Spec` per provider, `For`, `OAuthFlow`,
   `LegacyQuotaReconcileProvider`): static facts only, no imports from this module, so
   management handlers, `internal/config` and `sdk/cliproxy/auth` can all read it. Moved:
   - `DefaultRefreshLead` (Codex 96h): `types.go` `ProviderRefreshLead` and the
     `CodexOuterNeedsRefresh` fallback, both through `providerDefaultRefreshLead`.
     `auth.CodexOuterRefreshLead` stays exported and equals the registry value.
   - `InvalidGrantIsTerminal` (Codex): `conductor_cooldown.go` `isTerminalRefreshError`.
   - `OAuthFlow`/`OAuthAliases`: `oauth_sessions.go` `NormalizeOAuthProvider`.
   - `IdentitylessLogin` (kimi, kimi-ai): `auth_files_provider_oauth.go`
     `isIdentitylessOAuthProvider`.
   - `LegacyQuotaReconcile` (Codex): slice 2b above.
   Tests: `provider_refresh_characterization_test.go` (auth),
   `provider_login_characterization_test.go` (management) and the 2b test.
   Left as switches, with the reason:
   - `auth_files_v8.go` `StartOAuthV8` and `ImportOAuthV8`: dispatch to `Handler`
     methods, a method table rather than a provider fact; the Kimi domain choice
     (`normalizeOAuthProviderWithDomain`, `resolveTargetOAuthProvider`) reads Kimi's own
     domain helpers. `oauth_siwc.go`: SIWC is a Codex-only flow by definition.
   - `internal/watcher/synthesizer/file.go` (gemini to gemini-cli rename, Kimi domain,
     Codex `plan_type` from the id_token, Vertex) and `sdk/auth/filestore.go:254`
     (gemini skip): auth-file format parsing, each tied to that provider's file shape.
   - `sdk/cliproxy/service_auth.go` websocket teardown on removal: executor wiring
     (group 6).
5. **Model catalog source (slice 5).** `sdk/cliproxy/service_models.go:114-200,229,240,342`;
   `internal/registry/model_definitions.go:572-590` `GetStaticModelDefinitionsByChannel`;
   `sdk/cliproxy/auth/conductor_models.go:541-561,701-721,837-847`;
   `api_key_model_capabilities.go:373-399`; `oauth_model_alias.go:531-539`;
   `sdk/cliproxy/antigravity_models.go:479,619,654`; `internal/registry/model_registry.go:342-344,2007,2034`;
   `internal/client/codex/models/models.go:350,356`; `internal/api/server_routes.go:833,864`.
6. **Executor and transport wiring (last; may stay a switch).**
   `sdk/cliproxy/service_executors.go:233,302-326,578`; `conductor_execution.go:480-494`
   provider to translator format; `scheduler.go:465` and `conductor_selection.go:2577`
   websocket preference; `selector.go:1950-1956` `canonicalLCPProvider`;
   `sdk/api/handlers/openai/openai_responses_websocket.go:504,583,735`,
   `openai_responses_websocket_session.go:117`, `openai_images_handlers.go:293`,
   `openai_videos_handlers.go:157`; `sdk/cliproxy/service_plugins.go:365`;
   `sdk/cliproxy/usage/accounting.go:347`.
7. **Config key sections (not planned).** These map a provider to its own config list
   (`claude-api-key`, `codex-api-key`, ...): `types.go:707-717`,
   `internal/api/handlers/management/config_auth_index.go:454-617`, `api_tools.go:946-962`,
   `internal/config/config_v8.go:410,490`, `weight.go:47`, `config_yaml.go:918,924`,
   `internal/watcher/synthesizer/config.go:328,331`, `internal/util/provider.go:20`.
8. **Out of scope.** `internal/thinking/` (`apply.go`, `strip.go`, `summary.go`,
   `validate.go`, `configuration_update.go`) switches on translator formats inside the
   canonical-to-provider thinking pipeline, not on credential providers.
   `internal/signature/provider_compatibility.go`, `internal/cache/signature_cache.go`,
   `internal/pluginhost/adapters.go:93` and `internal/auth/kimi/kimi.go:71` classify
   model families or domains.

Slice order: 1, 2, 3, 4, 5, then decide on 6. Done: 1, 2, 2b, 3 and 4. Remaining: 5
(model catalog source), then the decision on 6. Slices 1-3 extend the unexported
`providerQuota` entry in `sdk/cliproxy/auth` (behaviour that needs auth types); slice 4
added the exported static-facts registry `internal/providerspec`. Slice 5 should add its
facts there when another package needs them.

## Management key failures count distinct keys

`AuthenticateManagementKey` (`internal/api/handlers/management/handler.go`) bans a client
address for 30 minutes after 5 failed management-key attempts. Upstream counts every
attempt. Here each distinct wrong key (a SHA-256 of it, never the key) counts once per
address until the next success or ban, and a missing key counts as one more key.

Why: rootless podman delivers every host-side caller (the dashboard, a browser through it,
local scripts, and LAN or tailnet clients through the published port) from the same
address, `10.0.2.100`. On 2026-11-12 one page firing nine requests with a stale key banned
that address, which locked every management client out of the hub for 30 minutes.
Trusting that address instead would turn off brute-force protection for everyone, because
remote callers arrive as the same address. Guessing still needs many distinct keys, so
five of them still ban.

Known limits:
- A repeated wrong key is no longer throttled by a ban, so each repeat still runs one
  bcrypt compare (about 45 ms when `secret-key` is a bcrypt hash). Accepted: it costs CPU,
  not guessing strength.
- A repeated key keeps its slot alive. Five *different* stale, missing or mistyped keys
  from the shared address with no success in between still ban everyone on it.
- Pre-existing, not changed here: any successful login from an address resets its
  counter. On the shared `10.0.2.100`, a dashboard polling with the correct key keeps
  resetting it, so the five-guesses-per-30-minutes bound does not hold for that address.
  A fix would count per key fingerprint across successes; it is a separate decision.

Tests: `TestAuthenticateManagementKey_RepeatedSameWrongKeyDoesNotBan`,
`TestAuthenticateManagementKey_RepeatedKeysShareTheFiveSlots`,
`TestAuthenticateManagementKey_LocalhostIPBan_BlocksCorrectKeyDuringBan` (now five
distinct keys).
