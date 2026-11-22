#!/usr/bin/python3
"""Split a live node config into a tracked file of placeholders + an overlay.

The tracked `configs/<node>.yaml` carries placeholder addresses only (#184/#195):
the LAN /24 as 10.99.0.x and tailnet hosts as 203.0.113.x, keeping the last
octet. The real values live in `configs/local/<node>.yaml`, which is gitignored
and merged over the tracked file at load time (`--overlay`, default
`<config dir>/local/<node>.yaml`).

Usage:
  deploy/overlay-split.py --node buster /opt/ai/config/model-router.yaml
      writes configs/local/buster.yaml and configs/buster.yaml (scrubbed)
  deploy/overlay-split.py --node buster --overlay-only /opt/.../model-router.yaml > /tmp/ov.yaml
  deploy/overlay-split.py --node buster --check configs/buster.yaml
      exit 1 if the file still contains a non-placeholder private address

Exit codes: 0 ok, 1 real address found by --check, 2 usage/parse error.
"""

from __future__ import annotations

import argparse
import ipaddress
import re
import sys
from pathlib import Path

try:
    import yaml
except ImportError:  # pragma: no cover - environment guard
    sys.exit("PyYAML is required: apt install python3-yaml")

LAN_PLACEHOLDER = "10.99.0."
TAIL_PLACEHOLDER = "203.0.113."

ADDR_RE = re.compile(
    r"\b(?:10\.\d{1,3}\.\d{1,3}\.\d{1,3}"
    r"|192\.168\.\d{1,3}\.\d{1,3}"
    r"|172\.(?:1[6-9]|2\d|3[01])\.\d{1,3}\.\d{1,3}"
    r"|100\.(?:6[4-9]|[7-9]\d|1[01]\d|12[0-7])\.\d{1,3}\.\d{1,3})\b"
)

# Documented range constants in code/docs, not node addresses.
ALLOWED = {"10.0.0.0", "172.16.0.0", "192.168.0.0", "100.64.0.0"}


def is_placeholder(addr: str) -> bool:
    return addr.startswith(LAN_PLACEHOLDER) or addr.startswith(TAIL_PLACEHOLDER)


def placeholder_for(addr: str) -> str:
    """Real address -> placeholder, keeping the last octet."""
    last = addr.rsplit(".", 1)[1]
    ip = ipaddress.ip_address(addr)
    if ip in ipaddress.ip_network("100.64.0.0/10"):
        return TAIL_PLACEHOLDER + last
    return LAN_PLACEHOLDER + last


def real_addresses(text: str) -> list[str]:
    out: list[str] = []
    for m in ADDR_RE.finditer(text):
        addr = m.group(0)
        if addr in ALLOWED or is_placeholder(addr):
            continue
        out.append(addr)
    return out


def scrub(text: str) -> tuple[str, dict[str, str]]:
    mapping: dict[str, str] = {}

    def sub(m: re.Match[str]) -> str:
        addr = m.group(0)
        if addr in ALLOWED or is_placeholder(addr):
            return addr
        rep = mapping.setdefault(addr, placeholder_for(addr))
        return rep

    return ADDR_RE.sub(sub, text), mapping


def overlay_from(cfg: dict) -> dict:
    """Keep only the per-node address-bearing keys."""
    ov: dict = {}
    if isinstance(cfg.get("peers"), list):
        ov["peers"] = cfg["peers"]
    discovery = cfg.get("discovery") or {}
    if isinstance(discovery, dict):
        keep = {k: discovery[k] for k in ("subnets", "exclude") if discovery.get(k)}
        if keep:
            ov["discovery"] = keep
    security = cfg.get("security") or {}
    if isinstance(security, dict):
        keep = {k: security[k] for k in ("trusted_proxies", "api_keys") if security.get(k)}
        if keep:
            ov["security"] = keep
    return ov


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("config", type=Path, help="the config to read (usually the live one)")
    ap.add_argument("--node", required=True, help="node name (overlay file name)")
    ap.add_argument("--repo", type=Path, default=Path(__file__).resolve().parent.parent,
                    help="repo root (default: the repo this script lives in)")
    ap.add_argument("--overlay-only", action="store_true", help="print the overlay, change nothing")
    ap.add_argument("--check", action="store_true", help="only check for real addresses; change nothing")
    ap.add_argument("--dry-run", action="store_true", help="show what would be written")
    args = ap.parse_args()

    text = args.config.read_text()
    try:
        cfg = yaml.safe_load(text) or {}
    except yaml.YAMLError as exc:
        print(f"parse {args.config}: {exc}", file=sys.stderr)
        return 2

    if args.check:
        found = real_addresses(text)
        if found:
            print("real addresses present: " + ", ".join(sorted(set(found))), file=sys.stderr)
            return 1
        print("ok: placeholders only")
        return 0

    overlay = overlay_from(cfg)
    ov_text = yaml.safe_dump(overlay, sort_keys=False, width=120)
    if args.overlay_only:
        sys.stdout.write(ov_text)
        return 0

    scrubbed, mapping = scrub(text)
    tracked = args.repo / "configs" / f"{args.node}.yaml"
    overlay_path = args.repo / "configs" / "local" / f"{args.node}.yaml"

    if mapping:
        print(f"scrubbed {len(mapping)} address(es):")
        for real, ph in sorted(mapping.items()):
            print(f"  {real} -> {ph}")
    else:
        print("nothing to scrub: config already carries placeholders")

    if args.dry_run:
        print(f"would write {overlay_path} ({len(ov_text.splitlines())} lines)")
        print(f"would write {tracked}")
        return 0

    overlay_path.parent.mkdir(parents=True, exist_ok=True)
    overlay_path.write_text(
        f"# {args.node}: per-node real values, untracked (configs/local/ is gitignored).\n"
        f"# Merged over configs/{args.node}.yaml at load time (#195).\n" + ov_text
    )
    tracked.write_text(scrubbed)
    left = real_addresses(scrubbed)
    if left:
        print("ERROR: scrubbed file still contains " + ", ".join(sorted(set(left))), file=sys.stderr)
        return 1
    print(f"wrote {tracked}\nwrote {overlay_path}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
