"""Black-box test of cli-proxy-exporter's proxy-state metrics against a stub management API.

Run: python3 -m unittest deploy/local/sidecars/exporter/test_exporter.py
"""
import copy
import io
import json
import math
import time
from unittest.mock import patch
import os
import subprocess
import sys
import socket
import tempfile
import threading
import unittest
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, HTTPServer, ThreadingHTTPServer
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


_network_violations = []
_network_guard = None
_original_connect = socket.socket.connect


def setUpModule():
    global _network_guard
    def connect(sock, address):
        if isinstance(address, tuple) and address[1] in {18317, 18320, 8317, 8319}:
            _network_violations.append(address)
            raise AssertionError(f"test attempted a live service port: {address}")
        return _original_connect(sock, address)
    _network_guard = patch.object(socket.socket, 'connect', connect)
    _network_guard.start()


def tearDownModule():
    _network_guard.stop()
    # http_ok catches exceptions; retain attempted ports so a swallowed assertion still fails.
    if _network_violations:
        raise AssertionError(f"tests attempted live service ports: {_network_violations}")


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
        with tempfile.TemporaryDirectory(dir="/dev/shm") as tmp:
            tmp = Path(tmp)
            (tmp / ".config/cli-proxy-rerank").mkdir(parents=True)
            (tmp / ".config/cli-proxy-rerank/management-key").write_text("test-key\n")
            instances = tmp / "instances.json"
            instances.write_text(json.dumps({"instances": [{
                "id": "stub", "provider": "claude", "base_url": f"http://127.0.0.1:{srv.server_port}",
                "management_path": "/m"}]}))
            mod = load_exporter(tmp)
            mod.DASHBOARD = f"http://127.0.0.1:{srv.server_port}"
            mod.PROXIES = {"claude": (f"http://127.0.0.1:{srv.server_port}",
                                       f"http://127.0.0.1:{srv.server_port}/m", "stub")}
            with patch.object(mod, 'fetch_usage', return_value=(True, json.dumps(health_payload()).encode())), \
                 patch.object(mod, 'collect_usage'), patch.object(mod, 'collect_quota'), patch.object(mod, 'collect_plans'):
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
    return dict(schema_version=1, rate_book_loaded=True, client_aliases_loaded=True, groups=[usage_group()],
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
                                      {"Authorization": "Bearer test-key"}, timeout=4)
        return m, {s: v for s, v in (l for l in m.lines if not isinstance(l, str))}

    def test_every_legacy_usage_series_keeps_its_name_labels_units_and_value(self):
        m, out = self.scrape()
        base = 'provider="claude",model="m1",account="acct",client="Work laptop",source="prox-usage"'
        expected = {
            f'cliproxy_requests_total{{status="success",{base}}}': 5,
            f'cliproxy_latency_seconds_sum{{{base}}}': 2.5,
            f'cliproxy_latency_seconds_count{{{base}}}': 5,
            f'cliproxy_ttft_seconds_sum{{{base}}}': 0.4,
            f'cliproxy_ttft_seconds_count{{{base}}}': 2,
            f'cliproxy_zero_token_success_total{{{base}}}': 1,
            f'cliproxy_api_equivalent_cost_usd_total{{{base}}}': 1.25,
            'cliproxy_last_request_timestamp_seconds{source="prox-usage"}': 1700000000.123,
            'cliproxy_models_priced{source="prox-usage"}': 34,
            'cliproxy_usage_unpriced_models{source="prox-usage"}': 0,
        }
        for kind, value in (("input", 123), ("output", 45), ("cached", 6), ("reasoning", 7)):
            expected[f'cliproxy_tokens_total{{type="{kind}",{base}}}'] = value
        for kind, value in (("read", 80), ("write", 9)):
            expected[f'cliproxy_cache_tokens_total{{type="{kind}",{base}}}'] = value
        self.assertEqual({k: v for k, v in out.items() if not k.startswith("cliproxy_usage_source_")}, expected)
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

    def test_unpriced_models_with_traffic_are_counted_once_and_named(self):
        data = usage_payload()  # m1 is priced
        for changes in (
            dict(model="m2", cost_usd=None),
            dict(model="m2", cost_usd=None, account="other", status="failed"),  # same model again
            dict(model="m3", cost_usd=None, requests=0),  # no traffic: not reported
        ):
            row = usage_group(); row.update(changes); data["groups"].append(row)
        m, out = self.scrape(data)
        self.assertEqual(out['cliproxy_usage_unpriced_models{source="prox-usage"}'], 1)
        info = {k: v for k, v in out.items() if k.startswith("cliproxy_usage_unpriced_model_info")}
        self.assertEqual(info, {'cliproxy_usage_unpriced_model_info{provider="claude",model="m2",source="prox-usage"}': 1})
        self.assertIn('# TYPE cliproxy_usage_unpriced_models gauge', m.text())
        self.assertIn('# TYPE cliproxy_usage_unpriced_model_info gauge', m.text())

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
                       lambda d: d.update(rate_book_loaded=False),
                       lambda d: d["groups"].append({}),
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


def health_payload():
    return {"schema_version": 1, "rate_book_loaded": False, "client_aliases_loaded": False,
            "sources": [
                {"instance": "claude-primary", "subscribed": True, "last_event_age_seconds": 1.5},
                {"instance": "codex-primary", "subscribed": False, "last_event_age_seconds": None}]}


class ExporterUsageHealth(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(dir="/dev/shm")
        self.mod = load_exporter(Path(self.tmp.name))

    def tearDown(self):
        self.tmp.cleanup()

    def test_adds_only_health_and_ignores_counter_configuration_readiness(self):
        m = self.mod.Metrics()
        with patch.object(self.mod, 'http_ok', return_value=(True, json.dumps(health_payload()).encode())) as get:
            self.mod.collect_usage_health(m)
            self.mod.fetch_usage(m)
        get.assert_called_once_with('http://127.0.0.1:18320/v1/exporter', {'Authorization': 'Bearer test-key'}, timeout=4)
        values = {line[0]: line[1] for line in m.lines if isinstance(line, list)}
        self.assertEqual(set(values), {
            'cliproxy_up{service="usage"}',
            'cliproxy_usage_source_subscribed{instance="claude-primary"}',
            'cliproxy_usage_source_subscribed{instance="codex-primary"}',
            'cliproxy_usage_source_last_event_age_seconds{instance="claude-primary"}',
            'cliproxy_usage_source_last_event_age_seconds{instance="codex-primary"}'})
        self.assertEqual(values['cliproxy_up{service="usage"}'], 1)
        self.assertEqual(values['cliproxy_usage_source_subscribed{instance="claude-primary"}'], 1)
        self.assertEqual(values['cliproxy_usage_source_subscribed{instance="codex-primary"}'], 0)
        self.assertEqual(values['cliproxy_usage_source_last_event_age_seconds{instance="claude-primary"}'], 1.5)
        self.assertTrue(math.isnan(values['cliproxy_usage_source_last_event_age_seconds{instance="codex-primary"}']))
        self.assertIn(' NaN', m.text())

    def health_values(self, data, raises=None):
        m = self.mod.Metrics()
        with patch.object(self.mod, 'http_ok', return_value=(True, json.dumps(data).encode())):
            if raises:
                with self.assertRaises(raises):
                    self.mod.collect_usage_health(m)
            else:
                self.mod.collect_usage_health(m)
        return {line[0]: line[1] for line in m.lines if isinstance(line, list)}

    def test_job_outcomes_emit_only_recorded_timestamps(self):
        data = health_payload()
        data["jobs"] = {"import": {"last_success_seconds": 1700000100.5, "last_failure_seconds": None},
                        "backup": {"last_success_seconds": None, "last_failure_seconds": 1700000200},
                        "prune": {"last_success_seconds": None, "last_failure_seconds": None}}  # never ran
        data["jobs_outcome_error"] = True
        values = self.health_values(data)
        self.assertEqual({k: v for k, v in values.items() if k.startswith("cliproxy_usage_job_")}, {
            'cliproxy_usage_job_last_success_timestamp_seconds{kind="import"}': 1700000100.5,
            'cliproxy_usage_job_last_failure_timestamp_seconds{kind="backup"}': 1700000200,
            'cliproxy_usage_job_outcome_error': 1})
        self.assertIn('cliproxy_usage_source_subscribed{instance="claude-primary"}', values)
        data["jobs_outcome_error"] = False
        self.assertEqual(self.health_values(data)['cliproxy_usage_job_outcome_error'], 0)

    def test_usage_service_without_job_fields_adds_no_job_series(self):
        values = self.health_values(health_payload())  # an older prox-usage has neither key
        self.assertFalse(any(k.startswith("cliproxy_usage_job_") for k in values))
        self.assertIn('cliproxy_usage_source_subscribed{instance="claude-primary"}', values)

    def test_malformed_job_fields_keep_source_health_and_raise_the_flag(self):
        good = {"last_success_seconds": 1700000100, "last_failure_seconds": None}
        cases = [("jobs", []), ("jobs", None)] + [("jobs", {"import": bad}) for bad in (
            None, {"last_success_seconds": 1700000100},  # outcome not an object; a missing key
            dict(good, last_success_seconds=True), dict(good, last_success_seconds="1700000100"),
            dict(good, last_failure_seconds=-1), dict(good, last_failure_seconds=math.nan),
            dict(good, last_failure_seconds=math.inf))] + [("jobs_outcome_error", bad) for bad in (1, "false", None)]
        for key, bad in cases:
            with self.subTest(key=key, bad=bad):
                data = health_payload()
                data["jobs"] = {"import": dict(good), "backup": dict(good)}
                data["jobs_outcome_error"] = False
                data[key] = bad
                values = self.health_values(data)  # does not raise: source health is independent
                self.assertEqual(values['cliproxy_usage_source_subscribed{instance="claude-primary"}'], 1)
                self.assertEqual(values['cliproxy_usage_source_subscribed{instance="codex-primary"}'], 0)
                self.assertEqual(values['cliproxy_usage_source_last_event_age_seconds{instance="claude-primary"}'], 1.5)
                # The job block is all-or-nothing: no timestamps, and the failure is visible.
                self.assertEqual({k: v for k, v in values.items() if k.startswith("cliproxy_usage_job_")},
                                 {'cliproxy_usage_job_outcome_error': 1})

    def test_malformed_source_entry_still_rejects_the_health_block(self):
        data = health_payload()
        data["jobs"] = {"import": {"last_success_seconds": 1700000100, "last_failure_seconds": None}}
        data["jobs_outcome_error"] = False
        data["sources"][0]["subscribed"] = "yes"
        self.assertEqual(set(self.health_values(data, raises=ValueError)), {'cliproxy_up{service="usage"}'})

    def test_slow_usage_read_has_one_deadline_and_other_collectors_still_render(self):
        release = threading.Event()
        seen = []
        class Slow(BaseHTTPRequestHandler):
            def do_GET(handler):
                seen.append((handler.path, handler.headers.get('Authorization')))
                release.wait(10)
                try:
                    handler.send_response(200); handler.end_headers(); handler.wfile.write(b'{}')
                except (BrokenPipeError, ConnectionResetError):
                    pass
            def log_message(self, *args):
                pass
        server = ThreadingHTTPServer(('127.0.0.1', 0), Slow)
        thread = threading.Thread(target=server.serve_forever, daemon=True); thread.start()
        self.mod.USAGE = f'http://127.0.0.1:{server.server_port}'
        self.mod.PROXIES = {}
        calls = []
        try:
            with patch.object(self.mod, 'collect_usage', side_effect=lambda m: calls.append('usage')), \
                 patch.object(self.mod, 'collect_quota', side_effect=lambda m: calls.append('quota')), \
                 patch.object(self.mod, 'collect_credentials', side_effect=lambda m: calls.append('credentials')), \
                 patch.object(self.mod, 'collect_plans', side_effect=lambda m: calls.append('plans')):
                # Dashboard stays on its original probe; intercept only that request.
                original = self.mod.http_ok
                def get(url, *args, **kwargs):
                    if url.startswith(self.mod.DASHBOARD):
                        return False, b''
                    return original(url, *args, **kwargs)
                with patch.object(self.mod, 'http_ok', side_effect=get), patch('sys.stderr'):
                    start = time.monotonic(); text = self.mod.render(); elapsed = time.monotonic()-start
            self.assertLess(elapsed, 4.8)
            self.assertIn('cliproxy_up{service="usage"} 0', text)
            self.assertEqual(calls, ['usage', 'quota', 'credentials', 'plans'])
            self.assertEqual(seen, [('/v1/exporter', 'Bearer test-key')])
            self.assertNotIn('test-key', seen[0][0])
        finally:
            release.set(); server.shutdown(); server.server_close(); thread.join()




class ExporterCutoverReadiness(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory(dir="/dev/shm")
        self.mod=load_exporter(Path(self.tmp.name))

    def tearDown(self):
        self.tmp.cleanup()

    def test_first_load_failures_skip_counters_but_last_good_errors_keep_counters(self):
        for field in ['rate_book_loaded','client_aliases_loaded']:
            payload=usage_payload();payload[field]=False
            payload['rate_book_error']=payload['client_alias_error']=True
            m=self.mod.Metrics()
            with patch.object(self.mod,'http_ok',return_value=(True,json.dumps(payload).encode())):
                with self.assertRaises(ValueError): self.mod.collect_usage(m)
            self.assertEqual(m.lines,[])
        payload=usage_payload(); payload['rate_book_error']=payload['client_alias_error']=True
        with patch.object(self.mod,'http_ok',return_value=(True,json.dumps(payload).encode())):
            m=self.mod.Metrics();self.mod.collect_usage(m)
        self.assertTrue(any(line.startswith('cliproxy_requests_total') for line in m.text().splitlines()))

    def test_startup_failure_marks_collector_failed_while_source_health_stays_available(self):
        self.mod.PROXIES={}
        for field in ['rate_book_loaded','client_aliases_loaded']:
            payload=usage_payload();payload[field]=False
            with patch.object(self.mod,'fetch_usage',return_value=(True,json.dumps(payload).encode())), \
                 patch.object(self.mod,'http_ok',return_value=(False,b'')), \
                 patch.object(self.mod,'collect_quota'),patch.object(self.mod,'collect_credentials'), \
                 patch.object(self.mod,'collect_plans'),patch('sys.stderr',new=io.StringIO()):
                text=self.mod.render()
            self.assertIn('cliproxy_exporter_collector_success{collector="usage"} 0',text)
            self.assertIn('cliproxy_up{service="usage"} 1',text)
            self.assertNotIn('cliproxy_requests_total',text)
            self.assertNotIn('cliproxy_api_equivalent_cost_usd_total',text)

    def test_health_and_usage_share_one_request_and_only_counters_gain_source(self):
        with patch.object(self.mod,'http_ok',return_value=(True,json.dumps(usage_payload()).encode())) as get:
            m=self.mod.Metrics();self.mod.collect_usage_health(m);self.mod.collect_usage(m)
        self.assertEqual(get.call_count,1)
        for line in m.lines:
            if isinstance(line,list):
                if line[0].startswith(('cliproxy_up{','cliproxy_usage_source_')):
                    self.assertNotIn('source=',line[0])
                else:
                    self.assertIn('source="prox-usage"',line[0])

    def test_shared_fixture_matches_old_sql_labels_and_endpoint_serialization(self):
        import sqlite3
        fixture=json.loads(SCRIPT.with_name('usageExporterLabels.json').read_text())
        db=sqlite3.connect(':memory:')
        db.row_factory=sqlite3.Row
        db.executescript(SCRIPT.with_name('legacy-usage.fixture.sql').read_text())
        db.execute('CREATE TABLE api_key_aliases(api_key_hash TEXT, alias TEXT)')
        for alias in fixture['client_alias']:
            db.execute('INSERT INTO api_key_aliases VALUES (?,?)',(alias['api_key_hash'],alias['alias']))
        for event in fixture['events']:
            keys=list(event)
            db.execute(f"INSERT INTO usage_events ({','.join(keys)}) VALUES ({','.join('?' for _ in keys)})",[event[k] for k in keys])
        old=[dict(row) for row in db.execute("""SELECT e.provider,
          COALESCE(NULLIF(e.requested_model,''),e.model) AS model,
          COALESCE(NULLIF(e.account_snapshot,''),e.auth_label_snapshot,e.auth_index) AS account,
          COALESCE(NULLIF(a.alias,''),CASE WHEN e.api_key_hash IS NULL OR e.api_key_hash='' THEN 'unknown'
            ELSE 'key ' || substr(e.api_key_hash,1,8) END) AS client
          FROM usage_events e LEFT JOIN api_key_aliases a ON a.api_key_hash=e.api_key_hash""")]
        db.close()
        self.assertEqual(old,fixture['labels'])
        # Prox's test loads this exact fixture through runCpampJob and /v1/exporter.
        payload=usage_payload();payload['groups']=[]
        for labels in fixture['labels']:
            row=usage_group();row.update(labels);payload['groups'].append(row)
        with patch.object(self.mod,'http_ok',return_value=(True,json.dumps(payload).encode())):
            m=self.mod.Metrics();self.mod.collect_usage(m)
        import re
        def label_values(series):
            return {key:value for key,value in re.findall(r'(provider|model|account|client)="([^"]*)"',series)}
        actual=[label_values(line[0]) for line in m.lines if isinstance(line,list) and line[0].startswith('cliproxy_requests_total{')]
        self.assertEqual(actual,old)


class ExporterLabelGate(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory(dir="/dev/shm")
        self.root=Path(self.tmp.name)
        self.mod=load_exporter(self.root)

    def tearDown(self):
        self.tmp.cleanup()

    def check(self, old, prox):
        m=self.mod.Metrics()
        for row in old:
            m.add('cliproxy_requests_total','counter','Requests',1,status='success',**row)
            m.add('cliproxy_tokens_total','counter','Tokens',1,type='input',**row)
        metrics=self.root/'old.prom';metrics.write_text(m.text())
        payload=usage_payload();payload['groups']=[]
        for labels in prox:
            row=usage_group();row.update(labels);payload['groups'].append(row)
        data=self.root/'prox.json';data.write_text(json.dumps(payload))
        return subprocess.run([sys.executable,str(SCRIPT.with_name('check-usage-labels.py')),
          '--old-metrics',str(metrics),'--exporter-json',str(data)],capture_output=True,text=True)

    def test_shared_fixture_passes_and_every_distinct_label_difference_blocks(self):
        labels=json.loads(SCRIPT.with_name('usageExporterLabels.json').read_text())['labels']
        result=self.check(labels,labels)
        self.assertEqual(result.returncode,0,result.stderr)
        self.assertFalse(json.loads(result.stdout)['swap_blocked'])
        for name in ['account','client','provider','model']:
            changed=copy.deepcopy(labels);changed[0][name]='different'
            result=self.check(labels,changed)
            self.assertEqual(result.returncode,1,result.stderr)
            diff=json.loads(result.stdout)['differences'][name]
            self.assertIn(labels[0][name],diff['only_old'])
            self.assertIn('different',diff['only_prox'])

    def test_lossy_historical_account_cases_block_without_changing_normalization(self):
        base={'provider':'claude','model':'m1','account':'unused','client':'test'}
        for before,after in [('  team@example.test  ','team@example.test'),('','3')]:
            result=self.check([{**base,'account':before}],[{**base,'account':after}])
            self.assertEqual(result.returncode,1,result.stderr)
            diff=json.loads(result.stdout)['differences']['account']
            self.assertEqual(diff['only_old'],[before or 'unknown'])
            self.assertEqual(diff['only_prox'],[after])

    def test_prometheus_escapes_preserve_visible_label_values(self):
        labels=[{'provider':'claude','model':'m1','account':'Account, {one}',
          'client':'Laptop "work", C:\\folder'}]
        result=self.check(labels,labels)
        self.assertEqual(result.returncode,0,result.stderr)


if __name__ == "__main__":
    unittest.main()
