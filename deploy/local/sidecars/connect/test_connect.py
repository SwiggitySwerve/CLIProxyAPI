"""Tests for cli-proxy-connect against a fake management API and exporter.

Run: python3 -I deploy/local/sidecars/connect/test_connect.py
"""
import contextlib
import importlib.machinery
import importlib.util
import io
import json
import os
import tempfile
import threading
import unittest
import unittest.mock
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import parse_qs, urlparse

HERE = Path(__file__).resolve().parent
loader = importlib.machinery.SourceFileLoader("connect", str(HERE / "cli-proxy-connect"))
spec = importlib.util.spec_from_loader("connect", loader)
cc = importlib.util.module_from_spec(spec)
loader.exec_module(cc)
cc.POLL_SECONDS = 0.01

CRED = {
    "id": "claude-someone.json", "auth_index": "0123456789abcdef", "name": "claude-someone.json",
    "provider": "claude", "kind": "auth_file", "instance_id": "claude-primary", "email": "someone@example.invalid",
    "priority": 10, "status": "active", "unavailable": False, "quota_blocked": False, "recovery_stage": "inference_verified",
    "quota": {"observed_at": "2026-11-12T10:00:00Z", "signals": {
        "Anthropic-Ratelimit-Unified-7d-Utilization": "0.42", "Anthropic-Ratelimit-Unified-7d-Status": "allowed",
        "Anthropic-Ratelimit-Unified-7d-Reset": "1794000000"}},
    "cooldowns": [{"scope": "model", "model_key": "model-a", "reason": "model_quota",
                   "retry_at": "2026-11-12T14:00:00Z", "remaining_seconds": 90}],
}


class FakeProxy(BaseHTTPRequestHandler):
    state = {}  # per test: status_replies, verify_status, log

    def reply(self, code, body):
        raw = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def handle_any(self, method):
        url = urlparse(self.path)
        q = {k: v[0] for k, v in parse_qs(url.query).items()}
        length = int(self.headers.get("Content-Length") or 0)
        body = json.loads(self.rfile.read(length)) if length else None
        s = self.state
        s["log"].append((method, url.path, q, body))
        if url.path == "/metrics":  # the exporter needs no key
            return self.reply_text(s["metrics"])
        if self.headers.get("Authorization") != "Bearer test-key":
            return self.reply(401, {"error": "unauthorized"})
        p = url.path.removeprefix("/v8/management")
        if p == "/oauth/auth-url":
            return self.reply(200, {"status": "ok", "url": "https://login.example.invalid/auth", "state": "st-1"})
        if p == "/oauth/status":
            return self.reply(200, s["status_replies"].pop(0) if len(s["status_replies"]) > 1 else s["status_replies"][0])
        if p == "/oauth/callback":
            return self.reply(200, {"status": "ok"})
        if p == "/oauth/session":
            return self.reply(200, {"status": "ok", "cancelled": True})
        if p == "/credentials/verify":
            return self.reply(*s["verify"])
        if p == "/credentials":
            return self.reply(200, {"files": [CRED]})
        if p == "/credentials/models":
            return self.reply(200, {"models": [{"id": "model-a"}, {"id": "model-b"}]})
        self.reply(404, {"error": "not found"})

    def reply_text(self, text):
        raw = text.encode()
        self.send_response(200)
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    do_GET = lambda self: self.handle_any("GET")
    do_POST = lambda self: self.handle_any("POST")
    do_DELETE = lambda self: self.handle_any("DELETE")

    def log_message(self, *args):
        pass


METRICS_LISTED = ('# TYPE cliproxy_credential_state gauge\n'
                  'cliproxy_credential_state{instance="claude-primary",provider="claude",'
                  'auth_index="0123456789abcdef",state="available",status="active"} 1\n')


class ConnectTest(unittest.TestCase):
    def setUp(self):
        FakeProxy.state = {"log": [], "metrics": METRICS_LISTED, "verify": (200, {"status": "ok", "verified": True}),
                           "status_replies": [{"status": "wait"}, {"status": "ok", "auth_id": CRED["id"]}]}
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), FakeProxy)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.addCleanup(self.server.server_close)
        self.addCleanup(self.server.shutdown)
        self.port = self.server.server_address[1]
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        d = Path(tmp.name)
        (d / "key").write_text("test-key\n")
        (d / "instances.json").write_text(json.dumps({"instances": [
            {"id": "claude-primary", "provider": "claude", "base_url": f"http://127.0.0.1:{self.port}"},
            {"id": "codex-primary", "provider": "codex", "base_url": "http://127.0.0.1:1"}]}))
        (d / "exporter.json").write_text(json.dumps({"listen": [["10.0.0.5", 9787], ["127.0.0.1", self.port]]}))
        env = {"CLI_PROXY_INSTANCES": str(d / "instances.json"), "CLI_PROXY_MANAGEMENT_KEY_FILE": str(d / "key"),
               "CLI_PROXY_EXPORTER_CONFIG": str(d / "exporter.json")}
        patch = unittest.mock.patch.dict(os.environ, env)
        patch.start()
        self.addCleanup(patch.stop)

    def run_main(self, *argv):
        out, err = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            code = cc.main(list(argv))
        return code, out.getvalue(), err.getvalue()

    def calls(self, path):
        return [c for c in FakeProxy.state["log"] if c[1].endswith(path)]

    def test_full_flow_prints_everything_and_changes_nothing_else(self):
        code, out, _ = self.run_main("claude")
        self.assertEqual(code, 0, out)
        start = self.calls("/oauth/auth-url")[0]
        self.assertEqual((start[0], start[2]), ("GET", {"provider": "claude", "verify": "true"}))
        self.assertIn("https://login.example.invalid/auth", out)
        self.assertEqual(self.calls("/credentials/verify")[0][3], {"name": CRED["id"]})
        for expected in ("0123456789abcdef", "model-a, model-b", "observed_at 2026-11-12T10:00:00Z",
                         "7d: used 42%, status allowed, reset 2026-11-", "scope model, model model-a",
                         "remaining 90s", "[  ok] exporter"):
            self.assertIn(expected, out)
        writes = {(m, p.removeprefix("/v8/management")) for m, p, _, _ in FakeProxy.state["log"] if m != "GET"}
        self.assertEqual(writes, {("POST", "/credentials/verify")})

    def test_exporter_url_prefers_the_loopback_listener(self):
        self.assertEqual(cc.exporter_url(None), f"http://127.0.0.1:{self.port}/metrics")

    def test_exporter_must_match_instance_and_auth_index(self):
        FakeProxy.state["metrics"] = METRICS_LISTED.replace('instance="claude-primary"', 'instance="other"')
        code, out, _ = self.run_main("claude", "--credential", CRED["id"])
        self.assertEqual(code, 1)
        self.assertIn("[FAIL] exporter", out)
        FakeProxy.state["metrics"] = METRICS_LISTED
        code, out, _ = self.run_main("claude", "--credential", CRED["id"])
        self.assertEqual(code, 0, out)

    def test_no_exporter_skips_the_scrape(self):
        FakeProxy.state["metrics"] = "# nothing\n"
        code, out, _ = self.run_main("claude", "--credential", CRED["id"])
        self.assertEqual(code, 1)
        code, out, _ = self.run_main("claude", "--credential", CRED["id"], "--no-exporter")
        self.assertEqual(code, 0, out)
        self.assertIn("[skip] exporter", out)
        self.assertEqual(len(self.calls("/metrics")), 1)  # only the first run scraped

    def test_credential_flag_skips_login(self):
        code, out, _ = self.run_main("claude", "--credential", "0123456789abcdef", "--no-exporter", "--model", "model-b")
        self.assertEqual(code, 0, out)
        self.assertEqual(self.calls("/oauth/auth-url"), [])
        self.assertEqual(self.calls("/credentials/verify")[0][3], {"name": CRED["id"], "model": "model-b"})

    def test_failed_verify_fails_the_run_but_still_lists(self):
        FakeProxy.state["verify"] = (429, {"status": "error", "verified": False, "error": "rate limited"})
        code, out, _ = self.run_main("claude", "--credential", CRED["id"], "--no-exporter")
        self.assertEqual(code, 1)
        self.assertIn("[FAIL] verify: HTTP 429: rate limited", out)
        self.assertIn("[  ok] listing", out)

    def test_login_error_cancels_the_session_and_stops(self):
        FakeProxy.state["status_replies"] = [{"status": "error", "error": "Authentication failed"}]
        code, out, _ = self.run_main("claude")
        self.assertEqual(code, 1)
        self.assertIn("[FAIL] oauth: login failed: Authentication failed", out)
        self.assertEqual(len(self.calls("/oauth/session")), 1)
        self.assertEqual(self.calls("/credentials/verify"), [])

    def test_login_timeout_cancels_the_session(self):
        FakeProxy.state["status_replies"] = [{"status": "wait"}]
        code, out, _ = self.run_main("claude", "--timeout", "0")
        self.assertEqual(code, 1)
        self.assertIn("no login completed", out)
        self.assertEqual(len(self.calls("/oauth/session")), 1)

    def test_instance_selection(self):
        code, _, err = self.run_main("claude", "nope", "--no-exporter")
        self.assertEqual(code, 1)
        self.assertIn("unknown instance 'nope'", err)
        code, _, err = self.run_main("kimi", "--no-exporter")
        self.assertEqual(code, 1)
        self.assertIn("0 instances serve provider 'kimi'", err)


class WindowsTest(unittest.TestCase):
    def test_claude_and_codex_headers_become_named_windows(self):
        w = cc.windows({
            "Anthropic-Ratelimit-Unified-5h-Utilization": "0.5", "Anthropic-Ratelimit-Unified-7d_oi-Status": "rejected",
            "Anthropic-Ratelimit-Unified-Status": "allowed", "X-Codex-Primary-Used-Percent": "12",
            "X-Codex-Primary-Reset-At": "1794000000", "Retry-After": "5"})
        self.assertEqual(w["5h"], {"used": "50%"})
        self.assertEqual(w["7d_oi"], {"status": "rejected"})
        self.assertEqual(w["unified"], {"status": "allowed"})
        self.assertEqual(w["primary"]["used"], "12%")
        self.assertNotIn("retry-after", w)


if __name__ == "__main__":
    unittest.main()
