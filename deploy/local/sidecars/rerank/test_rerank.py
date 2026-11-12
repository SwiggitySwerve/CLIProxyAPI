"""Black-box test of cli-proxy-rerank against a stub management API.

Run: python3 -m unittest deploy/local/sidecars/rerank/test_rerank.py
"""
import json
import os
import subprocess
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path

SCRIPT = Path(__file__).with_name("cli-proxy-rerank")
CREDS = [
    {"name": "a.json", "email": "a@example.test", "provider": "claude", "auth_index": "ia", "priority": 100},
    {"name": "b.json", "email": "b@example.test", "provider": "claude", "auth_index": "ib", "priority": 100},
    {"name": "c.json", "email": "c@example.test", "provider": "claude", "auth_index": "ic", "priority": 100},
]
USAGE_OK = {"five_hour": {"utilization": 10, "resets_at": "2026-01-01T00:00:00+00:00"},
            "seven_day": {"utilization": 100, "resets_at": "2026-01-03T00:00:00+00:00"}}


class Stub(BaseHTTPRequestHandler):
    seen = []  # (method, path, user-agent sent to upstream)

    def _send(self, obj):
        body = json.dumps(obj).encode()
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        Stub.seen.append(("GET", self.path, None))
        self._send({"files": CREDS})

    def do_POST(self):
        req = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        Stub.seen.append(("POST", self.path, req["header"].get("User-Agent")))
        if req["auth_index"] == "ib":  # b is the credential whose usage read succeeds
            self._send({"status_code": 200, "body": json.dumps(USAGE_OK)})
        else:  # a: usage endpoint 429; c: relay itself fails
            self._send({"status_code": 429, "body": ""})

    def do_PATCH(self):
        Stub.seen.append(("PATCH", self.path, None))
        self._send({})

    def log_message(self, *args):
        pass


class RerankReadOnly(unittest.TestCase):
    def test_failed_probe_is_unknown_and_nothing_is_patched(self):
        Stub.seen.clear()
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
            config = tmp / "config.yaml"
            config.write_text('upstream:\n    claude:\n        header-defaults:\n            user-agent: "claude-cli/9.9.9 (test)"\n')
            state = tmp / "state.json"
            state.write_text(json.dumps({"updated_at": 1000.0, "accounts": [
                {"provider": "claude", "account": "a@example.test", "rank": 1, "priority": 100, "limited": False,
                 "used_percent": 40, "reset_at": 5000.0}]}))
            env = dict(os.environ, HOME=str(tmp), CLI_PROXY_INSTANCES=str(instances),
                       CLI_PROXY_RERANK_STATE=str(state), CLI_PROXY_CLAUDE_CONFIG=str(config))
            subprocess.run([str(SCRIPT)], env=env, check=True, capture_output=True)
            accounts = {a["account"]: a for a in json.loads(state.read_text())["accounts"]}
        srv.shutdown()

        self.assertEqual({m for m, _, _ in Stub.seen}, {"GET", "POST"})  # no PATCH, ever
        self.assertEqual({ua for m, _, ua in Stub.seen if m == "POST"}, {"claude-cli/9.9.9 (test)"})
        a, b, c = (accounts[f"{x}@example.test"] for x in "abc")
        # a: probe failed, keeps its previous reading and age; not limited, not re-timed
        self.assertEqual((a["probe_status"], a["limited"], a["used_percent"], a["observed_at"]),
                         ("unknown", False, 40, 1000.0))
        self.assertEqual(a["reset_at"], 5000.0)
        # c: probe failed and there was no history: unknown, still not limited
        self.assertEqual((c["probe_status"], c["limited"], c["reset_at"], c["observed_at"]),
                         ("unknown", False, None, None))
        # b: probe succeeded at 100% used: genuinely limited, sinks below the unknowns
        self.assertEqual((b["probe_status"], b["limited"]), ("ok", True))
        self.assertEqual(b["rank"], 3)
        # priorities in state are the proxy's own values, untouched
        self.assertEqual({x["priority"] for x in accounts.values()}, {100})


if __name__ == "__main__":
    unittest.main()
