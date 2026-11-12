# Usage metrics after CPAMP

The usage collector makes one authenticated request to
`http://127.0.0.1:18320/v1/exporter` per scrape. The bearer comes from
`~/.config/cli-proxy-rerank/management-key`. Prox owns lifetime folding, client
names and current-rate pricing; this shim owns Prometheus serialization. It opens
no usage database, loads no prices and keeps no usage cache. Credential, quota and
rerank collectors retain their existing sources.

Prox's version-1 response must include complete original token counters. A failed,
malformed, unsupported-version or incomplete response makes
`cliproxy_exporter_collector_success{collector="usage"}` zero and omits usage
samples. Validation completes before publishing any usage counters. A failed
usage read does not hide the other collectors. Never log the bearer or response.

`cliproxy_up{service="usage"}` probes prox-usage `/healthz`. For compatibility,
`cliproxy_up{service="dashboard"}` still probes CPAMP's public `/health` while it
runs; it becomes zero when CPAMP is removed, without breaking the scrape. The old
`/management.html` probe is gone. The unit orders after prox-usage instead of
CPAMP and does not require CPAMP to exist.

New gauges have only an `instance` label:

- `cliproxy_usage_source_subscribed`: 1 for a subscribed source, otherwise 0.
- `cliproxy_usage_source_last_event_age_seconds`: seconds since this running
  collector last received an event, or NaN until its first event. Age alone does
  not prove a collector is stuck during quiet traffic. Alert on subscription
  loss for five minutes and separately on missing metrics; do not treat NaN as
  a healthy age of zero.

## Pricing and counter continuity

Prox preserves the old exporter's **billing-model-only** price lookup. A requested
alias's catalog price cannot substitute for an unpriced serving model. Each
event's context band is the greatest strictly exceeded threshold across billing
and requested models. Context takes precedence over service tiers. A context band
without a matching billing-model row falls back to base, skipping priority.
Outside a context band, use the recorded service tier, then a requested name
ending in `-fast` selects priority if no explicit tier row matched, then base.
**Retain this historical `-fast` rule** until the live series comparison shows it
can be removed. The shim does no pricing itself.

Counts below assume the same execution cohort, account snapshots, aliases and
legacy effective rates. Compare snapshots at the switch; new traffic between
reads explains only its own delta. Original source tokens and timing are preserved
separately from analytics' canonical tokens. No event expiry is shipped; raw
buffer expiry cannot lower lifetime counters.

| Series | Expected at the switch |
| --- | --- |
| `cliproxy_requests_total{provider,model,account,client,status}` | Exact same integer for both statuses. |
| `cliproxy_tokens_total{type,provider,model,account,client}` | Exact same original input/output/cached/reasoning counters for every type. Cached and reasoning remain subsets. |
| `cliproxy_cache_tokens_total{type,provider,model,account,client}` | Exact same normalized read/write counters. |
| `cliproxy_latency_seconds_sum{provider,model,account,client}` | Same original latency sum in seconds, within floating-point rounding. |
| `cliproxy_latency_seconds_count{provider,model,account,client}` | Exact same **all-call** count, including unmeasured/zero latency. |
| `cliproxy_ttft_seconds_sum{provider,model,account,client}` | Same positive-only TTFT sum in seconds, within floating-point rounding. |
| `cliproxy_ttft_seconds_count{provider,model,account,client}` | Exact same positive-only TTFT count. |
| `cliproxy_zero_token_success_total{provider,model,account,client}` | Exact same successes whose original total is zero. |
| `cliproxy_api_equivalent_cost_usd_total{provider,model,account,client}` | Existing priced cohorts stay equal within cost tolerance. D5 newly prices `claude-fable-5-2` and `gpt-6.1-terra`: their series appear/increase by their **lifetime** cost, and aggregate cost steps UP by that sum. No live dollar amount has been measured here. |
| `cliproxy_last_request_timestamp_seconds` | Same newest event epoch seconds. |
| `cliproxy_models_priced` | UP by two after both D5 rates are approved; all-null placeholders do not count. |
| `cliproxy_exporter_collector_success{collector="usage"}` | Remains 1 when ready; 0 on endpoint/data failure. |
| `cliproxy_exporter_collector_seconds{collector="usage"}` | Measures the new HTTP collector's runtime; expected to change. |
| `cliproxy_up{service="dashboard"}` | Remains 1 while CPAMP's health is reachable, then 0 after retirement. |
| `cliproxy_up{service="usage"}` and `cliproxy_usage_source_*` | New series; no prior baseline. |

**Any existing counter series that steps DOWN is a Prometheus counter reset.**
That can produce a rate spike; do not present the swap as a seamless baseline if
it happens. Missing history, changed aliases, changed effective prices or an
incomplete reconciliation must be investigated. No downward step is expected
after a clean parity comparison. Newly priced D5 costs and the priced-model gauge
are the planned changes. Gauges can legitimately fall; they are not counters.

## Operator switch (not performed by this PR)

1. Deploy the reviewed Prox endpoint first. Run its governed reconcile and
   require HTTP 200 with `missing_reported_counters: 0`. A schema-1 Prox-only row
   without source metadata blocks cutover; repair it from source evidence.
2. Put the exported client aliases at
   `~/.config/prox-usage/client-aliases.json`, mode 0600. For a new key, add its
   lowercase SHA-256 hash and chosen name to the `client_alias` array. Hash the
   exact key without a trailing newline through stdin, never shell argv. Prox's
   read-only config mount reloads it on mtime changes; no CPAMP UI or restart is
   needed. Unknown keys remain `key <first 8 hash characters>` or `unknown`.
3. Approve the D5 rate book and compare every usage series against the existing
   exporter at the same cohort. Preserve E0's seven-day baseline and record the
   planned D5 lifetime cost increase separately. Do not silently accept resets.
4. Only after approval, keep a rollback copy, install the script and unit, and
   restart the exporter. Keep CPAMP running through the three-day soak. Repoint
   alerts/panels to usage health before removing the dashboard compatibility
   probe and alert in the later retirement work.

Validation uses only synthetic tmpfs files and loopback stubs:
`PYTHONDONTWRITEBYTECODE=1 TMPDIR=/dev/shm ionice -c3 nice -n10 python3 -m unittest deploy/local/sidecars/exporter/test_exporter.py`.
No live exporter, unit, service, database or monitoring rule is changed by tests.
