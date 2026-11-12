"""Black-box test of cli-proxy-exporter's proxy-state metrics against a stub management API.

Run: python3 -m unittest deploy/local/sidecars/exporter/test_exporter.py
"""
import json
import os
import subprocess
import sys
import tempfile
import threading
import unittest
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path

SCRIPT = Path(__file__).with_name("cli-proxy-exporter")
FAR_FUTURE = "2099-01-01T00:00:00.123456789+08:00"  # nanoseconds and a non-UTC offset, as the proxy sends
CREDS = [
    {"auth_index": "idle", "provider": "claude", "email": "idle@example.test", "status": "active",
     "unavailable": False, "cooldowns": [],
     "quota": {"observed_at": "2026-01-01T00:00:00Z",
               "signals": {"Anthropic-Ratelimit-Unified-7d-Utilization": "0.5",
                           "Anthropic-Ratelimit-Unified-7d-Reset": "1800000000"}}},
    {"auth_index": "cool", "provider": "claude", "email": "cool@example.test", "status": "error",
     "unavailable": True, "next_retry_after": FAR_FUTURE, "quota_blocked": True,
     "cooldowns": [{"scope": "credential", "reason": "credential_quota", "retry_at": FAR_FUTURE},
                   {"scope": "model", "model_key": "m1", "reason": "quota", "retry_at": FAR_FUTURE}],
     "model_quotas": {"m1": {"observed_at": "2026-01-01T00:00:00Z",
                             "signals": {"Anthropic-Ratelimit-Unified-Status": "rejected"}}}},
]


class Stub(BaseHTTPRequestHandler):
    def do_GET(self):
        body = json.dumps({"files": CREDS}).encode() if self.path.endswith("/credentials") else b"ok"
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


class ExporterProxyTruth(unittest.TestCase):
    def test_cooling_credential_reports_the_proxys_deadline(self):
        srv = HTTPServer(("127.0.0.1", 0), Stub)
        threading.Thread(target=srv.serve_forever, daemon=True).start()
        with tempfile.TemporaryDirectory() as tmp:
            tmp = Path(tmp)
            (tmp / ".config/cli-proxy-rerank").mkdir(parents=True)
            (tmp / ".config/cli-proxy-rerank/management-key").write_text("test-key\n")
            instances = tmp / "instances.json"
            instances.write_text(json.dumps({"instances": [{
                "id": "stub", "provider": "claude", "base_url": f"http://127.0.0.1:{srv.server_port}",
                "management_path": "/m"}]}))
            env = dict(os.environ, HOME=str(tmp), CLI_PROXY_INSTANCES=str(instances))
            out = subprocess.run([str(SCRIPT), "--once"], env=env, check=True, capture_output=True, text=True).stdout
        srv.shutdown()
        lines = out.splitlines()

        def value(prefix):
            (line,) = [x for x in lines if x.startswith(prefix)]
            return float(line.rsplit(" ", 1)[1])

        lab = 'instance="stub",provider="claude",auth_index='
        deadline = datetime(2098, 12, 31, 16, tzinfo=timezone.utc).timestamp() + 0.123456  # FAR_FUTURE in UTC
        self.assertEqual(value(f"cliproxy_credential_available{{{lab}\"idle\"}}"), 1)
        self.assertEqual(value(f"cliproxy_credential_available{{{lab}\"cool\"}}"), 0)
        self.assertAlmostEqual(value(f"cliproxy_credential_next_retry_timestamp_seconds{{{lab}\"cool\"}}"), deadline, places=3)
        self.assertTrue(any(l.startswith("cliproxy_credential_state{state=\"cooling\",status=\"error\"") for l in lines))
        model_cd = [l for l in lines if l.startswith("cliproxy_credential_cooldown_retry_timestamp_seconds{")
                    and 'scope="model"' in l and 'model="m1"' in l]
        self.assertEqual(len(model_cd), 1)
        self.assertEqual(value(f"cliproxy_credential_quota_used_ratio{{{lab}\"idle\",model=\"all\",window=\"7d\"}}"), 0.5)
        self.assertTrue(any(l.startswith("cliproxy_credential_quota_status_info{") and 'status="rejected"' in l
                            and 'model="m1"' in l for l in lines))
        # age is reported for the stale reading, and the new metrics carry no account names
        self.assertGreater(value(f"cliproxy_credential_quota_age_seconds{{{lab}\"idle\",model=\"all\"}}"), 3600)
        self.assertFalse([l for l in lines if l.startswith("cliproxy_credential_") and "@" in l
                          and not l.startswith("cliproxy_credential_info")])


if __name__ == "__main__":
    unittest.main()


def load_exporter(home: Path):
    """Import the script as a module against a throwaway HOME (it reads paths at import)."""
    import importlib.machinery
    import importlib.util
    (home / ".config/cli-proxy-rerank").mkdir(parents=True, exist_ok=True)
    (home / ".config/cli-proxy-rerank/management-key").write_text("test-key\n")
    instances = home / "instances.json"
    instances.write_text(json.dumps({"instances": []}))
    old = {k: os.environ.get(k) for k in ("HOME", "CLI_PROXY_INSTANCES")}
    os.environ.update(HOME=str(home), CLI_PROXY_INSTANCES=str(instances))
    sys.dont_write_bytecode = True  # the loader would leave a .pyc beside the script
    try:
        loader = importlib.machinery.SourceFileLoader(f"exporter_{id(home)}", str(SCRIPT))
        mod = importlib.util.module_from_spec(importlib.util.spec_from_loader(loader.name, loader))
        loader.exec_module(mod)
        return mod
    finally:
        for k, v in old.items():
            os.environ.pop(k, None) if v is None else os.environ.__setitem__(k, v)


USAGE_SCHEMA = """
CREATE TABLE usage_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT, timestamp_ms INTEGER, provider TEXT, requested_model TEXT,
    model TEXT, resolved_model TEXT, account_snapshot TEXT, auth_label_snapshot TEXT, auth_index TEXT,
    api_key_hash TEXT, failed INTEGER, input_tokens INTEGER, output_tokens INTEGER, cached_tokens INTEGER,
    reasoning_tokens INTEGER, total_tokens INTEGER, normalized_uncached_input_tokens INTEGER,
    normalized_cache_read_tokens INTEGER, normalized_cache_creation_tokens INTEGER,
    normalized_total_input_tokens INTEGER, latency_ms INTEGER, ttft_ms INTEGER, service_tier TEXT);
CREATE TABLE model_prices (model TEXT, prompt_per_1m REAL, completion_per_1m REAL, cache_per_1m REAL,
    cache_read_per_1m REAL, cache_creation_per_1m REAL, cache_read_configured INTEGER,
    cache_creation_configured INTEGER);
CREATE TABLE model_price_context_tiers (model TEXT, threshold_tokens INTEGER, prompt_per_1m REAL,
    completion_per_1m REAL, cache_per_1m REAL, cache_read_per_1m REAL, cache_creation_per_1m REAL,
    prompt_configured INTEGER, completion_configured INTEGER, cache_configured INTEGER,
    cache_read_configured INTEGER, cache_creation_configured INTEGER);
CREATE TABLE model_price_service_tiers (model TEXT, mode TEXT, service_tier TEXT, prompt_per_1m REAL,
    completion_per_1m REAL, cache_per_1m REAL, cache_read_per_1m REAL, cache_creation_per_1m REAL,
    prompt_configured INTEGER, completion_configured INTEGER, cache_configured INTEGER,
    cache_read_configured INTEGER, cache_creation_configured INTEGER);
CREATE TABLE usage_data_migrations (name TEXT PRIMARY KEY, status TEXT, applied_rows INTEGER,
    updated_at_ms INTEGER);
INSERT INTO model_prices VALUES ('m1', 2.0, 10.0, NULL, NULL, NULL, 0, 0);
INSERT INTO model_price_context_tiers VALUES ('m1', 1000, 4.0, 20.0, NULL, NULL, NULL, 1, 1, 0, 0, 0);
INSERT INTO usage_data_migrations VALUES ('usage_cache_accounting_v2', 'completed', 0, 1);
"""


def add_events(db, n, *, tokens=100, account="acct"):
    db.executemany(
        "INSERT INTO usage_events (timestamp_ms, provider, requested_model, model, resolved_model, account_snapshot,"
        " api_key_hash, failed, input_tokens, output_tokens, cached_tokens, reasoning_tokens, total_tokens,"
        " normalized_uncached_input_tokens, normalized_cache_read_tokens, normalized_cache_creation_tokens,"
        " normalized_total_input_tokens, latency_ms, ttft_ms, service_tier)"
        " VALUES (?, 'claude', 'm1', 'm1', 'm1', ?, 'k', 0, ?, 10, 0, 0, ?, ?, 0, 0, ?, 500, 100, '')",
        [(1_700_000_000_000 + i, account, tokens, tokens + 10, tokens, tokens) for i in range(n)])
    db.commit()


class ExporterUsageCounters(unittest.TestCase):
    """Usage counters are running sums over usage_events, folded in by id instead of re-reading the
    whole table each scrape (a full GROUP BY spilled ~230 MB of temp files per scrape)."""

    def setUp(self):
        import sqlite3
        self.tmp = tempfile.TemporaryDirectory()
        home = Path(self.tmp.name)
        db_path = home / ".local/share/cpa-manager-plus/usage.sqlite"
        db_path.parent.mkdir(parents=True)
        self.db = sqlite3.connect(db_path)
        self.db.executescript(USAGE_SCHEMA)
        self.home = home

    def tearDown(self):
        self.db.close()
        self.tmp.cleanup()

    def scrape(self, mod):
        m = mod.Metrics()
        mod.collect_usage(m)
        return {s: v for s, v in (l for l in m.lines if not isinstance(l, str))}

    def requests(self, out):
        return sum(v for s, v in out.items() if s.startswith("cliproxy_requests_total{"))

    def test_counters_never_drop_when_old_events_are_archived(self):
        # CPA-Manager-Plus archives by deleting old usage_events rows. A counter that drops
        # reads as a reset to Prometheus and rate() shows a false spike.
        mod = load_exporter(self.home)
        add_events(self.db, 5)
        self.assertEqual(self.requests(self.scrape(mod)), 5)
        self.db.execute("DELETE FROM usage_events WHERE id <= 3")
        self.db.commit()
        add_events(self.db, 2)
        self.assertEqual(self.requests(self.scrape(mod)), 7)

    def test_incremental_scrapes_match_a_full_aggregation(self):
        mod = load_exporter(self.home)
        add_events(self.db, 4)
        self.scrape(mod)
        add_events(self.db, 3, tokens=5000)  # above the 1000-token context tier: its own price band
        add_events(self.db, 2, account="other")
        incremental = self.scrape(mod)
        fresh = self.scrape(load_exporter(self.home))  # a new process does one full pass
        self.assertEqual(incremental, fresh)
        self.assertEqual(self.requests(incremental), 9)

    def test_rewritten_token_columns_are_picked_up_after_a_migration(self):
        # The cache-accounting migration rewrites normalized tokens on existing rows; the
        # running sums must be rebuilt rather than keep the old values.
        mod = load_exporter(self.home)
        add_events(self.db, 2)
        before = self.scrape(mod)
        self.db.execute("UPDATE usage_events SET normalized_uncached_input_tokens = 0")
        self.db.execute("UPDATE usage_data_migrations SET applied_rows = 2, updated_at_ms = 2")
        self.db.commit()
        after = self.scrape(mod)
        cost = lambda out: sum(v for s, v in out.items() if s.startswith("cliproxy_api_equivalent_cost_usd_total{"))
        self.assertLess(cost(after), cost(before))
        self.assertEqual(cost(after), cost(self.scrape(load_exporter(self.home))))
