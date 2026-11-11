"""Tests for cli-proxy-catalog. Run: python3 -m unittest deploy/local/sidecars/catalog/test_catalog.py

The sidecar must reach the ultrafast account only through the proxy's
POST /v8/management/requests/api-call (token substituted by the proxy) and must
never open an auth file. A fake management server stands in for the proxy.
"""
import builtins
import importlib.machinery
import importlib.util
import json
import os
import pathlib
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer

SCRIPT = pathlib.Path(__file__).with_name("cli-proxy-catalog")
KEY = "test-management-key"
CREDENTIAL = "codex-placeholder-target.json"
ACCOUNT_ID = "acct-placeholder-0001"
AUTH_INDEX = "idx-target-0001"
MODELS = [{"slug": "gpt-9", "visibility": "list", "context_window": 123456}]


def load_module():
    loader = importlib.machinery.SourceFileLoader("cli_proxy_catalog_under_test", str(SCRIPT))
    spec = importlib.util.spec_from_loader(loader.name, loader)
    module = importlib.util.module_from_spec(spec)
    loader.exec_module(module)
    return module


class FakeProxy:
    """Records management calls and answers GET /credentials and POST /requests/api-call."""

    def __init__(self, credentials, api_call_reply=None):
        self.credentials = credentials
        self.api_call_reply = api_call_reply or {"status_code": 200, "header": {}, "body": json.dumps({"models": MODELS})}
        self.calls = []
        outer = self

        class Handler(BaseHTTPRequestHandler):
            def _record(self, body=None):
                outer.calls.append({"method": self.command, "path": self.path, "auth": self.headers.get("Authorization"), "body": body})

            def _reply(self, payload):
                data = json.dumps(payload).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

            def do_GET(self):
                self._record()
                if self.path == "/v8/management/credentials":
                    self._reply({"files": outer.credentials})
                else:
                    self.send_error(404)

            def do_POST(self):
                body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))) or b"{}")
                self._record(body)
                if self.path == "/v8/management/requests/api-call":
                    self._reply(outer.api_call_reply)
                else:
                    self.send_error(404)

            def log_message(self, *args):
                pass

        self.server = HTTPServer(("127.0.0.1", 0), Handler)
        self.base_url = "http://127.0.0.1:%d" % self.server.server_port
        threading.Thread(target=self.server.serve_forever, daemon=True).start()

    def close(self):
        self.server.shutdown()
        self.server.server_close()

    def api_calls(self):
        return [c for c in self.calls if c["method"] == "POST"]


def credential(name=CREDENTIAL, provider="codex", disabled=False, account_id=ACCOUNT_ID):
    return {"name": name, "provider": provider, "disabled": disabled, "auth_index": AUTH_INDEX,
            "id_token": {"chatgpt_account_id": account_id, "plan_type": "pro"}}


class CatalogTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.home = pathlib.Path(self.tmp.name)  # empty HOME: any auth-file read finds nothing
        self.key_file = self.home / "management-key"
        self.key_file.write_text(KEY + "\n")
        self.proxies = []
        self.opened = []
        real_open, real_read_text = builtins.open, pathlib.Path.read_text

        def spy_open(file, *args, **kwargs):
            self.opened.append(str(file))
            return real_open(file, *args, **kwargs)

        def spy_read_text(path, *args, **kwargs):
            self.opened.append(str(path))
            return real_read_text(path, *args, **kwargs)

        builtins.open, pathlib.Path.read_text = spy_open, spy_read_text
        self.addCleanup(setattr, builtins, "open", real_open)
        self.addCleanup(setattr, pathlib.Path, "read_text", real_read_text)

    def start(self, credentials, api_call_reply=None, config=None):
        proxy = FakeProxy(credentials, api_call_reply)
        self.proxies.append(proxy)
        self.addCleanup(proxy.close)
        instances = self.home / "instances.json"
        instances.write_text(json.dumps({"instances": [
            {"id": "claude-primary", "provider": "claude", "base_url": "http://127.0.0.1:1", "management_path": "/v8/management"},
            {"id": "codex-primary", "provider": "codex", "base_url": proxy.base_url, "management_path": "/v8/management"},
        ]}))
        cfg = {"credential": CREDENTIAL, "management_key_file": str(self.key_file)}
        cfg.update(config or {})
        config_file = self.home / "catalog.json"
        config_file.write_text(json.dumps(cfg))
        os.environ["CLI_PROXY_INSTANCES"] = str(instances)
        os.environ["CLI_PROXY_CATALOG_CONFIG"] = str(config_file)
        self.addCleanup(os.environ.pop, "CLI_PROXY_INSTANCES", None)
        self.addCleanup(os.environ.pop, "CLI_PROXY_CATALOG_CONFIG", None)
        return proxy, load_module()

    def test_fetches_models_through_api_call_with_token_placeholder(self):
        proxy, catalog = self.start([credential(), credential(name="codex-placeholder-other.json")])
        self.assertEqual(catalog.load_account_models(), MODELS)
        (call,) = proxy.api_calls()
        self.assertEqual(call["path"], "/v8/management/requests/api-call")
        self.assertEqual(call["auth"], "Bearer " + KEY)
        body = call["body"]
        self.assertEqual(body["auth_index"], AUTH_INDEX)
        self.assertEqual(body["method"], "GET")
        self.assertTrue(body["url"].startswith("https://chatgpt.com/backend-api/codex/models"))
        self.assertEqual(body["header"]["Authorization"], "Bearer $TOKEN$")
        self.assertEqual(body["header"]["Chatgpt-Account-Id"], ACCOUNT_ID)

    def test_never_opens_an_auth_file(self):
        _, catalog = self.start([credential()])
        self.opened.clear()
        self.assertEqual(catalog.load_account_models(), MODELS)
        self.assertEqual([p for p in self.opened if "auth" in p.lower().replace("authorization", "")], [])
        source = SCRIPT.read_text()
        for needle in ("access_token", "cli-proxy-api/auth", "ULTRAFAST_AUTH", "@"):
            self.assertNotIn(needle, source)

    def test_no_account_models_when_credential_is_unavailable(self):
        for name, creds, config in [
            ("disabled", [credential(disabled=True)], None),
            ("wrong provider", [credential(provider="claude")], None),
            ("not listed", [credential(name="codex-placeholder-other.json")], None),
            ("not configured", [credential()], {"credential": ""}),
            ("no account id", [credential(account_id="")], None),
        ]:
            with self.subTest(name):
                proxy, catalog = self.start(creds, config=config)
                self.assertEqual(catalog.load_account_models(), [])
                self.assertEqual(proxy.api_calls(), [])

    def test_failed_upstream_keeps_catalog_unchanged(self):
        for name, reply in [
            ("non-200", {"status_code": 429, "header": {}, "body": "{}"}),
            ("not json", {"status_code": 200, "header": {}, "body": "<html>"}),
        ]:
            with self.subTest(name):
                _, catalog = self.start([credential()], api_call_reply=reply)
                self.assertEqual(catalog.load_account_models(), [])

    def test_unreachable_proxy_returns_empty(self):
        proxy, catalog = self.start([credential()])
        proxy.close()
        self.assertEqual(catalog.load_account_models(), [])

    def test_proxy_base_comes_from_the_instance_map(self):
        proxy, catalog = self.start([credential()])
        self.assertEqual(catalog.proxy_base(), proxy.base_url)


if __name__ == "__main__":
    unittest.main()
