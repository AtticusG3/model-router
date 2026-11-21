#!/usr/bin/env python3
"""Unit tests for deploy/sd-router-shim (no GPU, no real backend)."""

from __future__ import annotations

import base64
import importlib.machinery
import importlib.util
import json
import os
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
        self.requests: list[dict] = []
        owner = self

        class Handler(BaseHTTPRequestHandler):
            def _reply(self, status: int, body: bytes) -> None:
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                try:
                    self.wfile.write(body)
                except (BrokenPipeError, ConnectionResetError):
                    pass

            def do_GET(self) -> None:  # noqa: N802
                catalog = {
                    "data": [
                        {"id": "test-model", "status": {"value": owner.status}}
                    ]
                }
                self._reply(200, json.dumps(catalog).encode())

            def do_POST(self) -> None:  # noqa: N802
                length = int(self.headers.get("Content-Length", 0) or 0)
                owner.requests.append(json.loads(self.rfile.read(length)))
                if owner.delay:
                    time.sleep(owner.delay)
                reply = {"choices": [{"message": {"content": owner.content}}]}
                self._reply(200, json.dumps(reply).encode())

            def log_message(self, *_args) -> None:
                return

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)

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
                "SHIM_ENHANCER_TIMEOUT": "5",
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

    def test_plain_prompt_is_rewritten_to_valid_caption(self) -> None:
        data, original, enhanced = shim._enhance_txt2img(
            "/sdapi/v1/txt2img",
            self.body(prompt="a red car", width=768, height=512, seed=7),
            True,
        )
        self.assertEqual(original, "a red car")
        self.assertIsNotNone(enhanced)
        out = json.loads(data)
        self.assertEqual(out["prompt"], enhanced)
        caption = json.loads(enhanced)
        self.assertTrue(shim._valid_caption(caption))
        self.assertEqual(caption["compositional_deconstruction"]["elements"][0]["bbox"], [120, 240, 880, 820])
        self.assertIn("768x512", self.enhancer.requests[0]["messages"][1]["content"])

    def test_existing_json_prompt_is_untouched(self) -> None:
        prompt = json.dumps(CAPTION)
        data = self.body(prompt=prompt)
        out, original, enhanced = shim._enhance_txt2img("/sdapi/v1/txt2img", data, True)
        self.assertIs(out, data)
        self.assertIsNone(original)
        self.assertIsNone(enhanced)
        self.assertEqual(self.enhancer.requests, [])

    def test_enhance_prompt_false_is_untouched(self) -> None:
        data = self.body(prompt="a red car", enhance_prompt=False)
        out, original, enhanced = shim._enhance_txt2img("/sdapi/v1/txt2img", data, True)
        self.assertIs(out, data)
        self.assertIsNone(original)
        self.assertIsNone(enhanced)
        self.assertEqual(self.enhancer.requests, [])

    def test_malformed_output_uses_template(self) -> None:
        self.enhancer.content = "this is not json"
        data, _, enhanced = shim._enhance_txt2img("/sdapi/v1/txt2img", self.body(prompt="a red car"), True)
        expected = json.dumps(shim._caption_template("a red car"), separators=(",", ":"))
        self.assertEqual(enhanced, expected)
        self.assertTrue(shim._valid_caption(json.loads(json.loads(data)["prompt"])))

    def test_timeout_uses_template(self) -> None:
        self.enhancer.delay = 0.5
        os.environ["SHIM_ENHANCER_TIMEOUT"] = "0.1"
        data, _, enhanced = shim._enhance_txt2img("/sdapi/v1/txt2img", self.body(prompt="a red car"), True)
        expected = json.dumps(shim._caption_template("a red car"), separators=(",", ":"))
        self.assertEqual(enhanced, expected)

    def test_flag_off_or_img2img_is_untouched(self) -> None:
        data = self.body(prompt="a red car")
        for path, enabled in (("/sdapi/v1/txt2img", False), ("/sdapi/v1/img2img", True)):
            out, original, enhanced = shim._enhance_txt2img(path, data, enabled)
            self.assertIs(out, data)
            self.assertIsNone(original)
            self.assertIsNone(enhanced)
        self.assertEqual(self.enhancer.requests, [])

    def test_stopped_enhancer_model_is_not_cold_loaded(self) -> None:
        self.enhancer.status = "stopped"
        data, _, enhanced = shim._enhance_txt2img("/sdapi/v1/txt2img", self.body(prompt="a red car"), True)
        expected = json.dumps(shim._caption_template("a red car"), separators=(",", ":"))
        self.assertEqual(enhanced, expected)
        self.assertEqual(self.enhancer.requests, [])

    def test_style_description_only_when_requested(self) -> None:
        styled = dict(CAPTION)
        styled["style_description"] = {"medium": "photo"}
        self.enhancer.content = json.dumps(styled)
        _, _, plain = shim._enhance_txt2img("/sdapi/v1/txt2img", self.body(prompt="a red car"), True)
        self.assertNotIn("style_description", json.loads(plain))
        _, _, styled_out = shim._enhance_txt2img(
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
        self.assertEqual(parsed, (5900, True, True, ["sd-server"]))
        self.assertIsNone(shim._parse_args(["--", "sd-server"]))
        os.environ["SHIM_PROMPT_ENHANCE"] = "1"
        parsed = shim._parse_args(["5900", "--", "sd-server"])
        self.assertEqual(parsed, (5900, True, False, ["sd-server"]))


if __name__ == "__main__":
    unittest.main()
