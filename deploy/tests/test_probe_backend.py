#!/usr/bin/env python3
"""Tests for deploy/probe-backend.sh (no GPU, no real backend)."""

from __future__ import annotations

import subprocess
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "probe-backend.sh"
TYPES = [
    "chat",
    "embedding",
    "rerank",
    "image",
    "speech",
    "transcription",
    "realtime",
    "video",
    "audio_gen",
]


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.0"

    def do_GET(self):
        if self.path.startswith("/v1/realtime"):
            self.send_response(101)
            self.send_header("Connection", "Upgrade")
            self.send_header("Upgrade", "websocket")
            self.send_header("Sec-WebSocket-Accept", "probe")
            self.end_headers()
            return
        self.reply()

    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        self.rfile.read(length)
        self.reply()

    def reply(self, code=200):
        body = b'{"ok":true}'
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


class BadHealth(Handler):
    def do_GET(self):
        self.reply(500)


class Server:
    def __init__(self, handler=Handler):
        self.handler = handler

    def __enter__(self):
        self.httpd = ThreadingHTTPServer(("127.0.0.1", 0), self.handler)
        threading.Thread(target=self.httpd.serve_forever, daemon=True).start()
        self.url = "http://127.0.0.1:%d" % self.httpd.server_address[1]
        return self

    def __exit__(self, *exc):
        self.httpd.shutdown()
        self.httpd.server_close()


class ProbeBackendTests(unittest.TestCase):
    def probe(self, url, *args):
        return subprocess.run(
            ["bash", str(SCRIPT), url, *args],
            capture_output=True,
            text=True,
            timeout=60,
        )

    def test_every_type_passes(self):
        with Server() as srv:
            for kind in TYPES:
                with self.subTest(kind=kind):
                    result = self.probe(srv.url, kind)
                    self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                    self.assertTrue(result.stdout.startswith("PASS "), result.stdout)

    def test_health_failure_is_fail(self):
        with Server(BadHealth) as srv:
            result = self.probe(srv.url, "chat")
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("FAIL chat health", result.stdout)

    def test_unknown_type_is_fail(self):
        with Server() as srv:
            result = self.probe(srv.url, "nonsense")
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("FAIL unknown type nonsense", result.stdout)


if __name__ == "__main__":
    unittest.main()
