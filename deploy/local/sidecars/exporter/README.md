# Prox usage counter cutover (Step E only)

The additive health PR (#51) keeps CPAMP counters during the dual run. This PR is
the later swap: one authenticated `GET http://127.0.0.1:18320/v1/exporter` supplies
canonical lifetime usage, using the same P4 pricing as Prox analytics. The shim
opens no usage database and loads no prices or client aliases. Credential, quota,
plan and dashboard probes keep their existing sources and labels.

All 40 existing metric names and types remain. Every usage counter/gauge gains
`source="prox-usage"`, including newest-request time and priced-model count. The
other label keys and visible requested-model/account/provider/client values stay
as before. Health series retain their additive identities, including
`cliproxy_up{service="usage"}` and the two `cliproxy_usage_source_*{instance}`
gauges. Health and counters share the additive four-second absolute HTTP deadline
and at most one outstanding daemon request. The key stays only in the bearer
header; it is never put in a URL or logs.

The endpoint's `rate_book_loaded` and `client_aliases_loaded` must both be true.
Otherwise the usage collector reports success=0 and publishes no usage counters;
source health and unrelated collectors remain available. Reload errors with a
last-good configuration can keep publishing. An explicit empty alias map counts
as loaded and deliberately uses `key <hash[:8]>` / `unknown` client fallbacks.

swervebox #80 adds `honorLabels: true` to the exporter ServiceMonitor so the
exporter's health `service` and `instance` labels remain those names instead of
`exported_service` / `exported_instance`. Coordinate that monitoring change with
health alert queries. Usage counters carry neither label, so their source-identity
swap and label-set gate are unaffected.

## Required pre-swap gates

1. **Finish Step B's full history import BEFORE switching.** Otherwise the import
   would appear as a huge increase in an already-active new counter series.
   Confirm the import/verification report and the agreed analytics parity gates.
2. Deploy Prox #77. Install the private 0600 client alias file and approved rates.
   Ensure both loaded flags are true; retain the dual run while checking labels.
3. **Block on any distinct label-set difference.** Before replacing the OLD
   exporter, run this read-only check from the CLIProxyAPI repo on the host:

   ```sh
   python3 deploy/local/sidecars/exporter/check-usage-labels.py \
     --old-metrics-url http://127.0.0.1:9787/metrics
   ```

   It reads old request/token samples and Prox `/v1/exporter` (management key from
   `~/.config/cli-proxy-rerank/management-key`). It compares distinct `account`,
   `client`, `provider` and `model` value sets, preserving visible escapes, case,
   whitespace and Unicode. Exit 0 is required. Exit 1 lists `only_old` and
   `only_prox` for every differing label; exit 2 means missing/invalid input or
   never-loaded configuration. **Do not swap on either failure.** No database is
   opened. For archived evidence or offline checking, use:

   ```sh
   python3 deploy/local/sidecars/exporter/check-usage-labels.py \
     --old-metrics old.prom --exporter-json prox-exporter.json
   ```

   Prox intentionally keeps its analytics normalization. Imported account labels
   with surrounding whitespace, or an empty non-null auth-label snapshot, can
   differ from the old exporter's SQL. The coordinator's all-history account-set
   evidence contains neither case. The gate still blocks either discrepancy,
   and differences caused by live credential snapshot refreshes or alias edits.
4. Apply the Sonnet 5.5 cache-read correction **$0.20 → $0.10 at this swap**, within
   the source identity change. Any later current-rate decrease can step the cost
   counter down (a Prometheus reset). Bump `USAGE_SOURCE`, for example to
   `prox-usage-2`, **before** applying any later decrease.
5. Land and run the proposed `swap-series.test.yaml` case in
   `swervebox/kubernetes/apps/cli-proxy-api/tests/exporter-rules.test.yaml`, against
   its extracted `rules.yaml`. It ends the old series, starts the source-labelled
   series at large lifetime totals and requires both E0 alerts plus
   `CliProxyHighFailureRate` to stay inactive. The coordinator owns that repo;
   this PR only supplies the case, also included in the PR body.
6. After approval, retain a rollback copy, install the exporter and keep CPAMP
   running for the three-day soak. This PR performs none of those live actions.

## Expected value and identity changes

Old counter series end and the new source-labelled series begin at canonical
lifetime totals. Prometheus `rate()`/`increase()` does not count a new series'
first sample; the initial lifetime value cannot create a reset spike. E0 and
failure-rate rules sum rates across source identities. A transition can briefly
have fewer samples, so preserve the before/after evidence.

Lifetime-total dashboards jump once. Input/output/cache/reasoning values now use
canonical event fields (output includes canonical reasoning). Cost now includes
Fable and Terra when their D5 rates are configured. The old generic-cache price
fallback, `-fast` priority inference, billing-only lookup and Python summation
order are gone: costs follow Prox analytics' P4 path. Latency count now follows
retained analytics measurements rather than the old all-call denominator.
Requested-model label strings remain unchanged even when canonical pricing uses
a different model candidate. Cost HELP changes from "synced in the dashboard" to
"current configured API list prices"; names, types and units remain unchanged.

Nothing syncs prices any more, so a model missing from the rate book has no cost
series and would go unnoticed. Two gauges make it visible:
`cliproxy_usage_unpriced_models{source="prox-usage"}` is the number of distinct
(provider, model) pairs that have requests and no price in the snapshot (0 when
all are priced), and `cliproxy_usage_unpriced_model_info{provider,model,source}`
is 1 for each such pair, for an alert's labels.

**Rollback caveat:** reverting to the old exporter resumes its old counter
series. CPAMP kept counting traffic while Prox series were active, so the resumed
old-series delta includes traffic already counted by the new series: rollback
can double-count it in rate/increase windows. Record the rollback interval and
account for it in alerts/dashboards; do not claim rollback preserves the baseline.

The shared `usageExporterLabels.json` is byte-identical to Prox's fixture. Prox
loads it through import and its real endpoint; this suite compares the frozen
old SQL label choices with serialized endpoint groups. Guard tests additionally
cover the two lossy historical account cases, escapes, and changes in each set.

```sh
PYTHONDONTWRITEBYTECODE=1 TMPDIR=/dev/shm ionice -c3 nice -n10 python3 -m unittest deploy/local/sidecars/exporter/test_exporter.py
```

Promtool 3.15.0 passes `swap-series.test.yaml` against the 13 rules extracted from
swervebox `origin/main`. The `_x2005` prefixes place the new series at 167h5m.

All tests use tmpfs or in-memory synthetic data and ephemeral loopback stubs. A
suite-wide socket guard fails any attempted access to live service ports.
