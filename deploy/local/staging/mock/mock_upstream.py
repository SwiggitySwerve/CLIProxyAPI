#!/usr/bin/env python3
"""Mock Anthropic Messages + Codex Responses upstream for the staging stack.

Python standard library only. One listener serves both the upstream API and a
small admin API, so tests can script quota behaviour per credential.

Upstream endpoints (credential = x-api-key, or the Authorization bearer token):
  POST /v1/messages                Anthropic Messages, JSON or SSE (stream=true)
  POST /v1/messages/count_tokens   Anthropic token count
  POST /responses                  Codex/OpenAI Responses (SSE unless stream=false)
  POST /responses/compact          Codex compact (JSON)
  GET  /wham/usage                 Codex usage probe (also under /backend-api/)

Admin endpoints:
  POST   /admin/scenario  {"credential", "mode", "percent", "reset_in", "reset_at", "retry_after"}
         mode: allowed | warning | rejected-7d | rejected-5h | rejected-7d_oi | lift
  GET    /admin/state     current scenario per credential
  GET    /admin/log       request log; filter with ?credential=
  DELETE /admin/log       clear the request log
  POST   /admin/reset     clear scenarios and log
  GET    /healthz
"""
import argparse
import json
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse

LOCK = threading.Lock()
SCENARIOS = {}  # credential -> {"mode", "percent", "reset_at", "retry_after"}
LOG = []
SEQ = [0]

REJECTED_MODES = ("rejected-7d", "rejected-5h", "rejected-7d_oi")
MODES = ("allowed", "warning") + REJECTED_MODES + ("lift",)
DEFAULT_RESET_IN = 7200


def scenario_for(cred):
    with LOCK:
        return dict(SCENARIOS.get(cred) or {"mode": "allowed", "percent": 10})


def set_scenario(body):
    cred = body.get("credential")
    mode = body.get("mode")
    if not cred or mode not in MODES:
        return None, "credential and mode (%s) are required" % ", ".join(MODES)
    with LOCK:
        cur = SCENARIOS.get(cred) or {"mode": "allowed", "percent": 10}
        if mode == "lift":
            if cur["mode"] in REJECTED_MODES:
                cur = {"mode": "allowed", "percent": 10}
        else:
            reset_at = body.get("reset_at")
            if reset_at is None:
                reset_at = int(time.time()) + int(body.get("reset_in", DEFAULT_RESET_IN))
            cur = {
                "mode": mode,
                "percent": int(body.get("percent", 85 if mode == "warning" else 100)),
                "reset_at": int(reset_at),
                "retry_after": body.get("retry_after"),
            }
        SCENARIOS[cred] = cur
        return dict(cur), None


def claude_headers(sc):
    """Anthropic unified rate-limit headers for a scenario."""
    now = int(time.time())
    mode = sc["mode"]
    h5_reset, d7_reset = now + 3 * 3600, now + 5 * 86400
    s5 = s7 = soi = "allowed"
    u5, u7, uoi = 0.1, 0.2, 0.0
    unified = "allowed"
    unified_reset = d7_reset
    claim = "five_hour"
    if mode == "warning":
        s7 = unified = "allowed_warning"
        u7 = sc["percent"] / 100.0
        claim = "seven_day"
    elif mode == "rejected-7d":
        s7 = unified = "rejected"
        u7 = 1.0
        d7_reset = unified_reset = sc["reset_at"]
        claim = "seven_day"
    elif mode == "rejected-5h":
        s5 = unified = "rejected"
        u5 = 1.0
        h5_reset = unified_reset = sc["reset_at"]
        claim = "five_hour"
    elif mode == "rejected-7d_oi":
        soi = unified = "rejected"
        uoi = 1.0
        unified_reset = sc["reset_at"]
        claim = "seven_day_oi"
    headers = [
        ("anthropic-ratelimit-unified-status", unified),
        ("anthropic-ratelimit-unified-reset", str(unified_reset)),
        ("anthropic-ratelimit-unified-representative-claim", claim),
        ("anthropic-ratelimit-unified-5h-status", s5),
        ("anthropic-ratelimit-unified-5h-utilization", str(u5)),
        ("anthropic-ratelimit-unified-5h-reset", str(h5_reset)),
        ("anthropic-ratelimit-unified-7d-status", s7),
        ("anthropic-ratelimit-unified-7d-utilization", str(u7)),
        ("anthropic-ratelimit-unified-7d-reset", str(d7_reset)),
        ("anthropic-ratelimit-unified-7d_oi-status", soi),
        ("anthropic-ratelimit-unified-7d_oi-utilization", str(uoi)),
        ("anthropic-ratelimit-unified-7d_oi-reset", str(sc.get("reset_at") or d7_reset)),
    ]
    if mode in REJECTED_MODES and sc.get("retry_after") is not None:
        headers.append(("retry-after", str(int(sc["retry_after"]))))
    return headers


def codex_windows(sc):
    """(primary, secondary) = (used_percent, window_minutes, reset_at) per window."""
    now = int(time.time())
    mode = sc["mode"]
    primary = [10, 300, now + 3 * 3600]
    secondary = [20, 10080, now + 5 * 86400]
    if mode == "warning":
        secondary[0] = sc["percent"]
    elif mode == "rejected-5h":
        primary[0], primary[2] = 100, sc["reset_at"]
    elif mode in ("rejected-7d", "rejected-7d_oi"):
        secondary[0], secondary[2] = 100, sc["reset_at"]
    return primary, secondary


def codex_headers(sc):
    now = int(time.time())
    out = []
    for name, (pct, minutes, reset_at) in zip(("primary", "secondary"), codex_windows(sc)):
        out += [
            ("x-codex-%s-used-percent" % name, str(pct)),
            ("x-codex-%s-window-minutes" % name, str(minutes)),
            ("x-codex-%s-reset-after-seconds" % name, str(max(0, reset_at - now))),
            ("x-codex-%s-reset-at" % name, str(reset_at)),
        ]
    out.append(("x-codex-plan-type", "plus"))
    return out


def wham_usage(sc):
    now = int(time.time())
    rejected = sc["mode"] in REJECTED_MODES
    primary, secondary = codex_windows(sc)

    def window(w):
        pct, minutes, reset_at = w
        return {
            "used_percent": pct,
            "limit_window_seconds": minutes * 60,
            "reset_after_seconds": max(0, reset_at - now),
            "reset_at": reset_at,
        }

    return {
        "plan_type": "plus",
        "rate_limit": {
            "allowed": not rejected,
            "limit_reached": rejected,
            "primary_window": window(primary),
            "secondary_window": window(secondary),
        },
    }


def credential_of(handler):
    key = handler.headers.get("x-api-key")
    if key:
        return key.strip()
    auth = handler.headers.get("Authorization", "")
    if auth.lower().startswith("bearer "):
        return auth[7:].strip()
    return "anonymous"


def sse(events):
    return "".join("event: %s\ndata: %s\n\n" % (name, json.dumps(data)) for name, data in events).encode()


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.0"  # close-delimited bodies keep SSE trivial

    def log_message(self, fmt, *args):  # request log is the /admin/log endpoint
        pass

    # --- helpers -----------------------------------------------------------
    def read_json(self):
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length) if length else b""
        try:
            return json.loads(raw) if raw else {}
        except ValueError:
            return {}

    def send_bytes(self, status, body, ctype, headers=()):
        self.send_response(status)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        for k, v in headers:
            self.send_header(k, v)
        self.end_headers()
        self.wfile.write(body)

    def send_json(self, status, obj, headers=()):
        self.send_bytes(status, json.dumps(obj).encode(), "application/json", headers)

    def record(self, cred, status, model="", stream=False, mode=""):
        with LOCK:
            SEQ[0] += 1
            LOG.append({
                "seq": SEQ[0], "ts": round(time.time(), 3), "method": self.command,
                "path": self.path, "credential": cred, "model": model,
                "stream": stream, "status": status, "scenario": mode,
            })

    # --- routing -----------------------------------------------------------
    def do_GET(self):
        path = urlparse(self.path).path
        if path == "/healthz":
            return self.send_json(200, {"status": "ok"})
        if path == "/admin/state":
            with LOCK:
                return self.send_json(200, {"scenarios": SCENARIOS})
        if path == "/admin/log":
            want = parse_qs(urlparse(self.path).query).get("credential", [None])[0]
            with LOCK:
                entries = [e for e in LOG if want is None or e["credential"] == want]
            return self.send_json(200, {"entries": entries})
        if path.endswith("/wham/usage"):
            cred = credential_of(self)
            sc = scenario_for(cred)
            self.record(cred, 200, mode=sc["mode"])
            return self.send_json(200, wham_usage(sc))
        self.send_json(404, {"error": "not found", "path": path})

    def do_DELETE(self):
        if urlparse(self.path).path == "/admin/log":
            with LOCK:
                LOG.clear()
            return self.send_json(200, {"cleared": True})
        self.send_json(404, {"error": "not found"})

    def do_POST(self):
        path = urlparse(self.path).path
        body = self.read_json()
        if path == "/admin/scenario":
            state, err = set_scenario(body)
            if err:
                return self.send_json(400, {"error": err})
            return self.send_json(200, state)
        if path == "/admin/reset":
            with LOCK:
                SCENARIOS.clear()
                LOG.clear()
            return self.send_json(200, {"reset": True})
        if path.endswith("/v1/messages/count_tokens"):
            return self.send_json(200, {"input_tokens": 10})
        if path.endswith("/v1/messages"):
            return self.claude_messages(body)
        if path.endswith("/responses/compact"):
            return self.codex_responses(body, compact=True)
        if path.endswith("/responses"):
            return self.codex_responses(body)
        self.send_json(404, {"error": "not found", "path": path})

    # --- Anthropic Messages --------------------------------------------------
    def claude_messages(self, body):
        cred = credential_of(self)
        sc = scenario_for(cred)
        model = body.get("model", "claude-mock")
        stream = bool(body.get("stream"))
        headers = claude_headers(sc)
        if sc["mode"] in REJECTED_MODES:
            self.record(cred, 429, model, stream, sc["mode"])
            return self.send_json(429, {
                "type": "error",
                "error": {"type": "rate_limit_error", "message": "Mock quota window rejected (%s)" % sc["mode"]},
            }, headers)
        self.record(cred, 200, model, stream, sc["mode"])
        text = "mock response from %s" % cred
        usage = {"input_tokens": 10, "output_tokens": 5}
        if not stream:
            return self.send_json(200, {
                "id": "msg_mock", "type": "message", "role": "assistant", "model": model,
                "content": [{"type": "text", "text": text}],
                "stop_reason": "end_turn", "stop_sequence": None, "usage": usage,
            }, headers)
        events = [
            ("message_start", {"type": "message_start", "message": {
                "id": "msg_mock", "type": "message", "role": "assistant", "model": model,
                "content": [], "stop_reason": None, "stop_sequence": None,
                "usage": {"input_tokens": 10, "output_tokens": 1}}}),
            ("content_block_start", {"type": "content_block_start", "index": 0,
                                     "content_block": {"type": "text", "text": ""}}),
            ("content_block_delta", {"type": "content_block_delta", "index": 0,
                                     "delta": {"type": "text_delta", "text": text}}),
            ("content_block_stop", {"type": "content_block_stop", "index": 0}),
            ("message_delta", {"type": "message_delta",
                               "delta": {"stop_reason": "end_turn", "stop_sequence": None},
                               "usage": {"output_tokens": 5}}),
            ("message_stop", {"type": "message_stop"}),
        ]
        self.send_bytes(200, sse(events), "text/event-stream", headers)

    # --- Codex Responses -----------------------------------------------------
    def codex_responses(self, body, compact=False):
        cred = credential_of(self)
        sc = scenario_for(cred)
        model = body.get("model", "gpt-mock")
        stream = body.get("stream", True) and not compact
        headers = codex_headers(sc)
        if sc["mode"] in REJECTED_MODES:
            reset_at = sc["reset_at"]
            self.record(cred, 429, model, stream, sc["mode"])
            return self.send_json(429, {"error": {
                "type": "usage_limit_reached",
                "message": "The usage limit has been reached",
                "plan_type": "plus",
                "resets_at": reset_at,
                "resets_in_seconds": max(1, reset_at - int(time.time())),
            }}, headers)
        self.record(cred, 200, model, stream, sc["mode"])
        text = "mock response from %s" % cred
        usage = {"input_tokens": 10, "output_tokens": 5, "total_tokens": 15}
        message = {"id": "msg_mock", "type": "message", "role": "assistant", "status": "completed",
                   "content": [{"type": "output_text", "text": text, "annotations": []}]}
        final = {"id": "resp_mock", "object": "response", "status": "completed", "model": model,
                 "output": [message], "usage": usage}
        if not stream:
            return self.send_json(200, final, headers)
        events = [
            ("response.created", {"type": "response.created", "response": {
                "id": "resp_mock", "object": "response", "status": "in_progress", "model": model}}),
            ("response.output_item.added", {"type": "response.output_item.added", "output_index": 0,
                                            "item": {"id": "msg_mock", "type": "message", "role": "assistant",
                                                     "status": "in_progress", "content": []}}),
            ("response.output_text.delta", {"type": "response.output_text.delta", "item_id": "msg_mock",
                                            "output_index": 0, "content_index": 0, "delta": text}),
            ("response.output_item.done", {"type": "response.output_item.done", "output_index": 0,
                                           "item": message}),
            ("response.completed", {"type": "response.completed", "response": final}),
        ]
        self.send_bytes(200, sse(events), "text/event-stream", headers)


def main():
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--host", default="0.0.0.0")
    parser.add_argument("--port", type=int, default=18330)
    args = parser.parse_args()
    server = ThreadingHTTPServer((args.host, args.port), Handler)
    print("mock upstream listening on %s:%d" % (args.host, args.port), flush=True)
    server.serve_forever()


if __name__ == "__main__":
    main()
