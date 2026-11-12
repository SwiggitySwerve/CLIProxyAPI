"""Black-box test of cli-proxy-exporter's proxy-state metrics against a stub management API.

Run: python3 -m unittest deploy/local/sidecars/exporter/test_exporter.py
"""
import json
import os
import subprocess
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
