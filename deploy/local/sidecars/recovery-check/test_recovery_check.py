"""Tests for cli-proxy-recovery-check: what a record keeps and what it must never keep.

Run: python3 -I deploy/local/sidecars/recovery-check/test_recovery_check.py
"""
import importlib.machinery
import importlib.util
import json
import os
import tempfile
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
loader = importlib.machinery.SourceFileLoader("recovery_check", str(HERE / "cli-proxy-recovery-check"))
spec = importlib.util.spec_from_loader("recovery_check", loader)
rc = importlib.util.module_from_spec(spec)
loader.exec_module(rc)

INSTANCE = {"id": "claude-primary", "provider": "claude"}
CRED = {
    "auth_index": "0123456789abcdef", "provider": "claude", "status": "error", "unavailable": True,
    "disabled": False, "priority": 80, "quota_blocked": True, "recovery_stage": "quota_blocked",
    "success": 0, "failed": 1,
    # Fields that identify the account or hold secrets: none may reach a record.
    "email": "someone@example.invalid", "name": "claude-someone@example.invalid.json",
    "label": "someone@example.invalid", "account": "acct-secret", "path": "/home/u/auth/x.json",
    "id_token": {"sub": "secret"}, "access_token": "eyJsecret", "status_message": "someone@example.invalid",
    "cooldowns": [{"scope": "credential", "reason": "credential_quota",
                   "retry_at": "2026-11-12T14:00:29Z", "remaining_seconds": 100}],
    "quota": {"observed_at": "2026-11-11T21:00:00Z", "signals": {
        "Anthropic-Ratelimit-Unified-7d-Status": "rejected",
        "Anthropic-Ratelimit-Unified-7d-Utilization": "0.99",
        "Anthropic-Ratelimit-Unified-Upgrade-Paths": "https://example.invalid/upgrade",
        "Retry-After": "300",
        "Authorization": "Bearer secret",
        "Set-Cookie": "session=secret"}},
    "model_quotas": {
        "model-a": {"observed_at": "t", "signals": {"Anthropic-Ratelimit-Unified-7d-Status": "rejected"}},
        "model-b": {"observed_at": "t", "signals": {"Anthropic-Ratelimit-Unified-7d-Status": "rejected",
                                                    "Anthropic-Ratelimit-Unified-7d_oi-Status": "allowed"}},
    },
}


class RecordTest(unittest.TestCase):
    def setUp(self):
        self.rec = rc.credential_record(INSTANCE, CRED)

    def test_labelled_by_instance_and_auth_index_only(self):
        self.assertEqual(self.rec["instance"], "claude-primary")
        self.assertEqual(self.rec["auth_index"], "0123456789abcdef")

    def test_no_identity_or_secret_reaches_the_record(self):
        raw = json.dumps(self.rec)
        for forbidden in ("someone", "example.invalid/upgrade", "acct-secret", "/home/", "eyJ", "Bearer", "session="):
            self.assertNotIn(forbidden, raw)
        for field in ("email", "name", "label", "account", "path", "id_token", "access_token", "status_message"):
            self.assertNotIn(field, self.rec)

    def test_signals_are_an_allow_list(self):
        self.assertEqual(set(self.rec["quota"]["signals"]), {
            "Anthropic-Ratelimit-Unified-7d-Status", "Anthropic-Ratelimit-Unified-7d-Utilization", "Retry-After"})

    def test_cooldown_deadline_is_kept(self):
        self.assertEqual(self.rec["cooldowns"], [{"scope": "credential", "reason": "credential_quota",
                                                  "retry_at": "2026-11-12T14:00:29Z", "remaining_seconds": 100}])

    def test_model_signals_keep_only_the_differences(self):
        self.assertNotIn("model-a", self.rec["model_quotas"])
        self.assertEqual(self.rec["model_quotas"]["model-b"]["signals"],
                         {"Anthropic-Ratelimit-Unified-7d_oi-Status": "allowed"})


class AppendTest(unittest.TestCase):
    def test_mode_600_and_rotation(self):
        with tempfile.TemporaryDirectory() as d:
            out = Path(d) / "sub" / "recovery.jsonl"
            rc.append_line(out, '{"a":1}')
            self.assertEqual(oct(out.stat().st_mode & 0o777), "0o600")
            old = rc.MAX_BYTES
            rc.MAX_BYTES = 1
            try:
                rc.append_line(out, '{"a":2}')
            finally:
                rc.MAX_BYTES = old
            self.assertEqual(out.with_name("recovery.jsonl.1").read_text(), '{"a":1}\n')
            self.assertEqual(out.read_text(), '{"a":2}\n')


if __name__ == "__main__":
    unittest.main()
