#!/usr/bin/env python3
"""Unit tests for deploy/sd-router-shim (no GPU, no real backend)."""

from __future__ import annotations

import base64
import contextlib
import importlib.machinery
import importlib.util
import json
import os
import socket
import struct
import threading
import time
import unittest
import zlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from unittest import mock

ROOT = Path(__file__).resolve().parents[1]
_loader = importlib.machinery.SourceFileLoader("sd_router_shim", str(ROOT / "sd-router-shim"))
_spec = importlib.util.spec_from_loader(_loader.name, _loader)
shim = importlib.util.module_from_spec(_spec)
_loader.exec_module(shim)

ENV_KEYS = (
    "SHIM_PROMPT_ENHANCE",
    "SHIM_ENHANCER_URL",
    "SHIM_ENHANCER_MODEL",
    "SHIM_ENHANCER_TIMEOUT",
    "SHIM_BLOCKED_RETRIES",
)

CAPTION = {
    "high_level_description": "a red sports car parked at a meet",
    "compositional_deconstruction": {
        "background": "a row of parked cars under a clear sky",
        "elements": [
            {"type": "obj", "bbox": [120, 240, 880, 820], "desc": "a red coupe"}
        ],
    },
}


def _png(width: int, height: int, pixel) -> bytes:
    rows = b"".join(
        b"\x00" + b"".join(bytes(pixel(x, y)) for x in range(width))
        for y in range(height)
    )

    def chunk(kind: bytes, data: bytes) -> bytes:
        return (
            struct.pack(">I", len(data))
            + kind
            + data
            + struct.pack(">I", zlib.crc32(kind + data) & 0xFFFFFFFF)
        )

    ihdr = struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0)
    return (
        b"\x89PNG\r\n\x1a\n"
        + chunk(b"IHDR", ihdr)
        + chunk(b"IDAT", zlib.compress(rows))
        + chunk(b"IEND", b"")
    )


def grey_fixture() -> bytes:
    return _png(32, 32, lambda _x, _y: (128, 128, 128))


def colour_fixture() -> bytes:
    return _png(32, 32, lambda x, y: (x * 8 % 256, y * 8 % 256, 40))


class FakeEnhancer:
    def __init__(self) -> None:
        self.content = json.dumps(CAPTION)
        self.delay = 0.0
        self.status = "running"
        self.catalog_status = 200
        self.chat_status = 200
        self.reply: bytes | None = None
        self.trickle = 0.0
        self.requests: list[dict] = []
        owner = self

        class Handler(BaseHTTPRequestHandler):
            def _reply(self, status: int, body: bytes, trickle: float = 0.0) -> None:
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                try:
                    if not trickle:
                        self.wfile.write(body)
                        return
                    for byte in body:
                        self.wfile.write(bytes([byte]))
                        time.sleep(trickle / len(body))
                except (BrokenPipeError, ConnectionResetError):
                    pass

            def do_GET(self) -> None:  # noqa: N802
                catalog = {
                    "data": [
                        {"id": "test-model", "status": {"value": owner.status}}
                    ]
                }
                self._reply(owner.catalog_status, json.dumps(catalog).encode())

            def do_POST(self) -> None:  # noqa: N802
                length = int(self.headers.get("Content-Length", 0) or 0)
                owner.requests.append(json.loads(self.rfile.read(length)))
                if owner.delay:
                    time.sleep(owner.delay)
                reply = {"choices": [{"message": {"content": owner.content}}]}
                body = owner.reply if owner.reply is not None else json.dumps(reply).encode()
                self._reply(owner.chat_status, body, owner.trickle)

            def log_message(self, *_args) -> None:
                return

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, args=(0.05,), daemon=True)

    def start(self) -> str:
        self.thread.start()
        return f"http://127.0.0.1:{self.server.server_address[1]}/v1"

    def stop(self) -> None:
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=2)


class ShimTest(unittest.TestCase):
    def setUp(self) -> None:
        self.saved = {key: os.environ.pop(key, None) for key in ENV_KEYS}
        self.enhancer = FakeEnhancer()
        url = self.enhancer.start()
        os.environ.update(
            {
                "SHIM_ENHANCER_URL": url,
                "SHIM_ENHANCER_MODEL": "test-model",
            }
        )

    def tearDown(self) -> None:
        self.enhancer.stop()
        for key, value in self.saved.items():
            if value is None:
                os.environ.pop(key, None)
            else:
                os.environ[key] = value

    def body(self, **fields) -> bytes:
        return json.dumps(fields).encode("utf-8")

    def enhance(self, path: str, data: bytes, enabled: bool, timeout: float = 5.0):
        return shim._enhance_txt2img(path, data, enabled, timeout)

    def test_plain_prompt_is_rewritten_to_valid_caption(self) -> None:
        data, original, enhanced, source = self.enhance(
            "/sdapi/v1/txt2img",
            self.body(prompt="a red car", width=768, height=512, seed=7),
            True,
        )
        self.assertEqual(original, "a red car")
        self.assertIsNotNone(enhanced)
        self.assertEqual(source, "enhancer")
        out = json.loads(data)
        self.assertEqual(out["prompt"], enhanced)
        caption = json.loads(enhanced)
        self.assertTrue(shim._valid_caption(caption))
        self.assertEqual(caption["compositional_deconstruction"]["elements"][0]["bbox"], [120, 240, 880, 820])
        self.assertIn("768x512", self.enhancer.requests[0]["messages"][1]["content"])

    def test_existing_json_prompt_is_untouched(self) -> None:
        prompt = json.dumps(CAPTION)
        data = self.body(prompt=prompt)
        out, original, enhanced, source = self.enhance("/sdapi/v1/txt2img", data, True)
        self.assertIs(out, data)
        self.assertIsNone(original)
        self.assertIsNone(enhanced)
        self.assertIsNone(source)
        self.assertEqual(self.enhancer.requests, [])

    def test_enhance_prompt_false_is_untouched(self) -> None:
        data = self.body(prompt="a red car", enhance_prompt=False)
        out, original, enhanced, source = self.enhance("/sdapi/v1/txt2img", data, True)
        self.assertIs(out, data)
        self.assertIsNone(original)
        self.assertIsNone(enhanced)
        self.assertIsNone(source)
        self.assertEqual(self.enhancer.requests, [])

    def test_malformed_output_uses_template(self) -> None:
        self.enhancer.content = "this is not json"
        data, _, enhanced, source = self.enhance("/sdapi/v1/txt2img", self.body(prompt="a red car"), True)
        expected = json.dumps(shim._caption_template("a red car"), separators=(",", ":"))
        self.assertEqual(enhanced, expected)
        self.assertTrue(shim._valid_caption(json.loads(json.loads(data)["prompt"])))

    def test_timeout_uses_template(self) -> None:
        self.enhancer.delay = 0.5
        data, _, enhanced, source = self.enhance(
            "/sdapi/v1/txt2img", self.body(prompt="a red car"), True, timeout=0.1
        )
        expected = json.dumps(shim._caption_template("a red car"), separators=(",", ":"))
        self.assertEqual(enhanced, expected)
        self.assertEqual(source, "template")

    def test_every_enhancer_failure_sends_prompt_in_template(self) -> None:
        timeout = 0.5
        closed = socket.socket()
        closed.bind(("127.0.0.1", 0))
        refused_url = f"http://127.0.0.1:{closed.getsockname()[1]}/v1"
        closed.close()
        cases = {
            "model not running": {"status": "stopped"},
            "catalogue unreachable": {"catalog_status": 500},
            "catalogue unreachable, no model set": {"catalog_status": 500, "env": {"SHIM_ENHANCER_MODEL": ""}},
            "model missing from catalogue": {"env": {"SHIM_ENHANCER_MODEL": "other-model"}},
            "connection refused": {"env": {"SHIM_ENHANCER_URL": refused_url}},
            "timeout before headers": {"delay": timeout * 3},
            "trickled body": {"trickle": timeout * 3},
            "http 500": {"chat_status": 500},
            "non-json content": {"content": "this is not json"},
            "json without choices": {"reply": b'{"id":"x"}'},
            "invalid caption": {"content": json.dumps({"high_level_description": "x"})},
            "malformed url": {"env": {"SHIM_ENHANCER_URL": "127.0.0.1:18080/v1"}},
            "unexpected exception": {"raises": True},
        }
        no_chat = {"model not running", "catalogue unreachable", "catalogue unreachable, no model set",
                   "model missing from catalogue"}
        prompts = ("a red car", 'a "quoted" sign\\path\nsecond line 🚗 赤い車')
        for name, case in cases.items():
            for prompt in prompts:
                with self.subTest(name, prompt=prompt):
                    fake = FakeEnhancer()
                    for key in ("status", "catalog_status", "delay", "trickle", "chat_status", "content", "reply"):
                        if key in case:
                            setattr(fake, key, case[key])
                    env = {"SHIM_ENHANCER_URL": fake.start(), **case.get("env", {})}
                    boom = mock.patch.object(shim, "_enhancer_model", side_effect=RuntimeError("boom"))
                    try:
                        with mock.patch.dict(os.environ, env), boom if case.get("raises") else contextlib.nullcontext():
                            started = time.monotonic()
                            data, original, _, source = self.enhance(
                                "/sdapi/v1/txt2img", self.body(prompt=prompt), True, timeout=timeout
                            )
                            elapsed = time.monotonic() - started
                    finally:
                        fake.stop()
                    self.assertLess(elapsed, timeout + 1)
                    self.assertEqual(original, prompt)
                    self.assertEqual(source, "template")
                    caption = json.loads(json.loads(data)["prompt"])
                    self.assertIsInstance(caption, dict)
                    self.assertTrue(shim._valid_caption(caption))
                    self.assertEqual(caption["high_level_description"], prompt)
                    self.assertEqual(caption["compositional_deconstruction"]["elements"][0]["desc"], prompt)
                    if name in no_chat:
                        self.assertEqual(fake.requests, [])

    def test_flag_off_or_img2img_is_untouched(self) -> None:
        data = self.body(prompt="a red car")
        for path, enabled in (("/sdapi/v1/txt2img", False), ("/sdapi/v1/img2img", True)):
            out, original, enhanced, source = self.enhance(path, data, enabled)
            self.assertIs(out, data)
            self.assertIsNone(original)
            self.assertIsNone(enhanced)
        self.assertEqual(self.enhancer.requests, [])

    def test_stopped_enhancer_model_is_not_cold_loaded(self) -> None:
        self.enhancer.status = "stopped"
        data, _, enhanced, source = self.enhance("/sdapi/v1/txt2img", self.body(prompt="a red car"), True)
        expected = json.dumps(shim._caption_template("a red car"), separators=(",", ":"))
        self.assertEqual(enhanced, expected)
        self.assertEqual(self.enhancer.requests, [])

    def test_style_description_only_when_requested(self) -> None:
        styled = dict(CAPTION)
        styled["style_description"] = {"medium": "photo"}
        self.enhancer.content = json.dumps(styled)
        _, _, plain, _ = self.enhance("/sdapi/v1/txt2img", self.body(prompt="a red car"), True)
        self.assertNotIn("style_description", json.loads(plain))
        _, _, styled_out, _ = self.enhance(
            "/sdapi/v1/txt2img", self.body(prompt="a cinematic photo of a red car"), True
        )
        self.assertIn("style_description", json.loads(styled_out))

    def test_blocked_card_detection_fixture(self) -> None:
        self.assertTrue(shim._png_is_flat_grey(grey_fixture()))
        self.assertFalse(shim._png_is_flat_grey(colour_fixture()))
        reply = json.dumps({"images": [base64.b64encode(grey_fixture()).decode()]}).encode()
        self.assertTrue(shim._response_is_blocked_card(reply))
        self.assertFalse(shim._response_is_blocked_card(b"not json"))

    def test_cli_flags(self) -> None:
        parsed = shim._parse_args(["--prompt-enhance", "--retry-blocked", "5900", "--", "sd-server"])
        self.assertEqual(parsed, (5900, True, True, 20.0, ["sd-server"]))
        self.assertIsNone(shim._parse_args(["--", "sd-server"]))
        os.environ["SHIM_PROMPT_ENHANCE"] = "1"
        os.environ["SHIM_ENHANCER_TIMEOUT"] = "30"
        parsed = shim._parse_args(["5900", "--", "sd-server"])
        self.assertEqual(parsed, (5900, True, False, 30.0, ["sd-server"]))
        parsed = shim._parse_args(["--enhancer-timeout", "45", "5900", "--", "sd-server"])
        self.assertEqual(parsed, (5900, True, False, 45.0, ["sd-server"]))
        for bad in (["--enhancer-timeout"], ["--enhancer-timeout", "0"], ["--enhancer-timeout", "soon"]):
            self.assertIsNone(shim._parse_args(bad + ["5900", "--", "sd-server"]))


if __name__ == "__main__":
    unittest.main()
