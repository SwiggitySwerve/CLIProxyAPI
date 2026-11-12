# Prox usage health during the dual run

This PR is the additive health step. Usage counters still come from CPAMP's
SQLite, price tables and aliases, exactly as before. No counter label or value
changes. The dashboard probe remains `/management.html` and
`cliproxy_up{service="dashboard"}` keeps its existing meaning.

After the additive Prox endpoint (SwiggitySwerve/prox#77) is deployed, one bearer
read of `http://127.0.0.1:18320/v1/exporter` adds:

- `cliproxy_up{service="usage"}`: HTTP reachability of the Prox usage endpoint.
- `cliproxy_usage_source_subscribed{instance}`: 1 while the source is subscribed,
  otherwise 0.
- `cliproxy_usage_source_last_event_age_seconds{instance}`: seconds since this
  process last received an event, or NaN until its first event.

Health does not require the rate book or client aliases to have loaded: those
configuration flags affect the later counter swap, while these series only report
collection state. Age alone cannot prove a collector is stuck during quiet
traffic. A five-minute subscription-loss alert and missing-metrics alert can now
be enabled during the dual run without changing E0's counter baselines.

The management key is read from
`~/.config/cli-proxy-rerank/management-key`, sent only in the bearer header, and
never put in the URL or logs. The request has one absolute four-second deadline
including headers and body. At most one daemon request may remain outstanding;
a trickling server cannot hold the scrape open or create an unlimited request
queue. A late response is never reused by another scrape. Usage health does not
perform a second `/healthz` probe. Timeouts mark usage down while the existing
CPAMP, quota, credential and plan collectors still render.

The later Step E PR will switch counters to Prox's canonical analytics values,
with `source="prox-usage"` on usage counters/gauges. It must wait for the history
import. That PR owns startup-readiness checks, the one-time series-identity and
lifetime-dashboard changes, rate corrections and rollback guidance. This additive
PR can be deployed during the dual run; it performs no live installation itself.

Tests use synthetic tmpfs databases and loopback stubs:

```sh
PYTHONDONTWRITEBYTECODE=1 TMPDIR=/dev/shm ionice -c3 nice -n10 python3 -m unittest deploy/local/sidecars/exporter/test_exporter.py
```

The existing archive, incremental-scrape and rewritten-token regressions remain.
Health tests cover exact labels, never-loaded rates/aliases and a slow server.
