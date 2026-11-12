"""Black-box test of cli-proxy-exporter's proxy-state metrics against a stub management API.

Run: python3 -m unittest deploy/local/sidecars/exporter/test_exporter.py
"""
import json
import os
import math
import copy
import io
import sys
import tempfile
import threading
import unittest
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
from unittest.mock import patch

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
        body = (json.dumps({"files": CREDS}).encode() if self.path.endswith("/credentials") else
                json.dumps(usage_payload()).encode() if self.path.endswith("/v1/exporter") else b"ok")
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
        with tempfile.TemporaryDirectory(dir="/dev/shm") as tmp:
            tmp = Path(tmp)
            (tmp / ".config/cli-proxy-rerank").mkdir(parents=True)
            (tmp / ".config/cli-proxy-rerank/management-key").write_text("test-key\n")
            instances = tmp / "instances.json"
            instances.write_text(json.dumps({"instances": [{
                "id": "stub", "provider": "claude", "base_url": f"http://127.0.0.1:{srv.server_port}",
                "management_path": "/m"}]}))
            mod = load_exporter(tmp)
            mod.PROXIES = {"claude": (f"http://127.0.0.1:{srv.server_port}",
                                       f"http://127.0.0.1:{srv.server_port}/m", "stub")}
            mod.USAGE = mod.DASHBOARD = f"http://127.0.0.1:{srv.server_port}"
            with patch.object(mod, 'collect_quota'), patch.object(mod, 'collect_plans'):
                out = mod.render()
        srv.shutdown()
        srv.server_close()
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


def usage_group(**overrides):
    return dict(provider="claude", model="m1", account="acct", client="Work laptop", status="success",
                requests=5, tokens=dict(input=123, output=45, cached=6, reasoning=7),
                cache=dict(read=80, write=9), latency_seconds_sum=2.5, latency_seconds_count=5,
                ttft_seconds_sum=0.4, ttft_seconds_count=2, zero_token_success=1,
                cost_usd=1.25, **overrides)


def usage_payload():
    return dict(schema_version=1, missing_reported_counters=0, groups=[usage_group()],
                newest_event_time_seconds=1700000000.123, models_priced=34,
                rate_book_error=False, client_alias_error=False, sources=[
                    dict(instance="claude-primary", state="subscribed", subscribed=True, last_event_age_seconds=1.5),
                    dict(instance="codex-primary", state="backoff", subscribed=False, last_event_age_seconds=None)])


class ExporterUsageCounters(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(dir="/dev/shm")
        self.home = Path(self.tmp.name)
        self.mod = load_exporter(self.home)

    def tearDown(self):
        self.tmp.cleanup()

    def scrape(self, data=None):
        data = usage_payload() if data is None else data
        m = self.mod.Metrics()
        with patch.object(self.mod, "http_ok", return_value=(True, json.dumps(data).encode())) as fetch:
            self.mod.collect_usage(m)
        fetch.assert_called_once_with("http://127.0.0.1:18320/v1/exporter",
                                      {"Authorization": "Bearer test-key"})
        return m, {s: v for s, v in (l for l in m.lines if not isinstance(l, str))}

    def test_every_legacy_usage_series_keeps_its_name_labels_units_and_value(self):
        m, out = self.scrape()
        base = 'provider="claude",model="m1",account="acct",client="Work laptop"'
        expected = {
            f'cliproxy_requests_total{{status="success",{base}}}': 5,
            f'cliproxy_latency_seconds_sum{{{base}}}': 2.5,
            f'cliproxy_latency_seconds_count{{{base}}}': 5,
            f'cliproxy_ttft_seconds_sum{{{base}}}': 0.4,
            f'cliproxy_ttft_seconds_count{{{base}}}': 2,
            f'cliproxy_zero_token_success_total{{{base}}}': 1,
            f'cliproxy_api_equivalent_cost_usd_total{{{base}}}': 1.25,
            'cliproxy_last_request_timestamp_seconds': 1700000000.123,
            'cliproxy_models_priced': 34,
        }
        for kind, value in (("input", 123), ("output", 45), ("cached", 6), ("reasoning", 7)):
            expected[f'cliproxy_tokens_total{{type="{kind}",{base}}}'] = value
        for kind, value in (("read", 80), ("write", 9)):
            expected[f'cliproxy_cache_tokens_total{{type="{kind}",{base}}}'] = value
        self.assertEqual({k: v for k, v in out.items() if not k.startswith("cliproxy_usage_source_")}, expected)
        self.assertEqual(out['cliproxy_usage_source_subscribed{instance="claude-primary"}'], 1)
        self.assertEqual(out['cliproxy_usage_source_subscribed{instance="codex-primary"}'], 0)
        self.assertEqual(out['cliproxy_usage_source_last_event_age_seconds{instance="claude-primary"}'], 1.5)
        self.assertTrue(math.isnan(out['cliproxy_usage_source_last_event_age_seconds{instance="codex-primary"}']))
        self.assertIn('cliproxy_usage_source_last_event_age_seconds{instance="codex-primary"} NaN', m.text())
        self.assertIn('# TYPE cliproxy_requests_total counter', m.text())
        self.assertIn('# TYPE cliproxy_models_priced gauge', m.text())

    def test_failed_groups_add_to_shared_counters_without_adding_a_status_label(self):
        data = usage_payload()
        failed = copy.deepcopy(data["groups"][0]); failed["status"] = "failed"
        data["groups"].append(failed)
        _, out = self.scrape(data)
        requests = {k: v for k, v in out.items() if k.startswith("cliproxy_requests_total")}
        self.assertEqual(len(requests), 2)
        for name in ("cliproxy_tokens_total", "cliproxy_cache_tokens_total", "cliproxy_latency_seconds_sum"):
            self.assertTrue(all('status=' not in k for k in out if k.startswith(name)))
        self.assertEqual(sum(v for k, v in out.items() if k.startswith("cliproxy_api_equivalent_cost_usd_total")), 2.5)

    def test_unpriced_cost_is_absent_and_explicit_zero_cost_is_preserved(self):
        data = usage_payload(); data["groups"][0]["cost_usd"] = None
        _, out = self.scrape(data)
        self.assertFalse(any(k.startswith("cliproxy_api_equivalent_cost_usd_total") for k in out))
        data["groups"][0]["cost_usd"] = 0
        _, out = self.scrape(data)
        self.assertEqual(sum(v for k, v in out.items() if k.startswith("cliproxy_api_equivalent_cost_usd_total")), 0)
        self.assertTrue(any(k.startswith("cliproxy_api_equivalent_cost_usd_total") for k in out))

    def test_counters_are_transported_without_local_repricing_or_a_private_cache(self):
        first = usage_payload(); self.scrape(first)
        changed = usage_payload(); changed["groups"][0]["cost_usd"] = 3.75
        changed["groups"][0]["tokens"]["input"] = 2**40 + 1
        _, out = self.scrape(changed)
        self.assertEqual(sum(v for k, v in out.items() if k.startswith("cliproxy_api_equivalent_cost_usd_total")), 3.75)
        self.assertEqual(sum(v for k, v in out.items() if k.startswith("cliproxy_tokens_total{type=\"input\"")), 2**40 + 1)
        source = SCRIPT.read_text()
        for removed in ("sqlite3", "usage_events", "load_prices", "api_key_aliases", "USAGE_GROUPS_SQL"):
            self.assertNotIn(removed, source)

    def test_malformed_or_unready_responses_publish_no_partial_usage(self):
        for mutate in (lambda d: d.update(schema_version=2),
                       lambda d: d.update(missing_reported_counters=1),
                       lambda d: d["groups"].append({}),
                       lambda d: d["sources"][0].update(subscribed="yes"),
                       lambda d: d["groups"][0].update(requests=float("nan"))):
            data = usage_payload(); mutate(data)
            m = self.mod.Metrics()
            with patch.object(self.mod, "http_ok", return_value=(True, json.dumps(data).encode())):
                with self.assertRaises((ValueError, KeyError, TypeError)):
                    self.mod.collect_usage(m)
            self.assertEqual(m.lines, [])
        with patch.object(self.mod, "http_ok", return_value=(False, b"secret response must stay hidden")):
            with self.assertRaisesRegex(RuntimeError, "^prox-usage exporter unavailable$"):
                self.mod.collect_usage(self.mod.Metrics())

    def test_health_uses_usage_healthz_and_tolerates_a_retired_dashboard(self):
        calls = []
        def get(url, headers=None, timeout=5):
            calls.append(url)
            if url.endswith('/v1/exporter'):
                return True, json.dumps(usage_payload()).encode()
            return url == self.mod.USAGE + '/healthz', b'{}'
        self.mod.PROXIES = {}
        with patch.object(self.mod, 'http_ok', side_effect=get), \
             patch.object(self.mod, 'collect_quota'), patch.object(self.mod, 'collect_credentials'), \
             patch.object(self.mod, 'collect_plans'):
            text = self.mod.render()
        self.assertIn('cliproxy_up{service="usage"} 1', text)
        self.assertIn('cliproxy_up{service="dashboard"} 0', text)
        self.assertIn('cliproxy_exporter_collector_success{collector="usage"} 1', text)
        self.assertEqual(calls.count(self.mod.USAGE + '/v1/exporter'), 1)
        self.assertIn(self.mod.USAGE + '/healthz', calls)
        self.assertIn(self.mod.DASHBOARD + '/health', calls)
        self.assertFalse(any('/management.html' in url for url in calls))
        with patch('sys.stderr', new=io.StringIO()), \
             patch.object(self.mod, 'http_ok', return_value=(False, b'')), \
             patch.object(self.mod, 'collect_quota'), patch.object(self.mod, 'collect_credentials'), \
             patch.object(self.mod, 'collect_plans'):
            text = self.mod.render()
        self.assertIn('cliproxy_up{service="usage"} 0', text)
        self.assertIn('cliproxy_exporter_collector_success{collector="usage"} 0', text)
        self.assertNotIn('cliproxy_requests_total', text)

    def test_real_loopback_read_sends_the_bearer_once(self):
        requests = []
        class UsageStub(BaseHTTPRequestHandler):
            def do_GET(handler):
                requests.append((handler.path, handler.headers.get('Authorization')))
                body = json.dumps(usage_payload()).encode()
                handler.send_response(200); handler.end_headers(); handler.wfile.write(body)
            def log_message(self, *args):
                pass
        server = HTTPServer(('127.0.0.1', 0), UsageStub)
        thread = threading.Thread(target=server.serve_forever, daemon=True); thread.start()
        self.mod.USAGE = f'http://127.0.0.1:{server.server_port}'
        try:
            m = self.mod.Metrics(); self.mod.collect_usage(m)
            self.assertTrue(m.series)
        finally:
            server.shutdown(); server.server_close(); thread.join()
        self.assertEqual(requests, [('/v1/exporter', 'Bearer test-key')])


if __name__ == "__main__":
    unittest.main()
