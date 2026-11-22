#!/usr/bin/python3
"""Stamp img2img / inpaint capabilities on tracked image stanzas (#200).

Text-level, not YAML-round-trip: the configs are heavily commented and must keep
their formatting. Idempotent — a second run changes nothing.

img2img goes on image stanzas that match /sdapi/v1 (the sd-server path). A
stanza that selects by body field or a non-sdapi prefix is left alone, because
its server may not implement the sdapi img2img contract. inpaint goes only on
the stanzas the ticket names as inpaint-capable.
"""
import re
import sys

# Stanzas known to accept a mask (sdapi `mask` on img2img), per #200.
INPAINT = {
    "sd3.5-inpaint", "sdxl-juggernaut-xi", "qwen-image-2.1", "qwen-image-edit-2511",
    "boogu-image-edit", "fibo-2.0-edit-turbo", "flux3-klein",
}

START = re.compile(r"^(\s*)-\s*model_id:\s*(\S+)\s*$")


def blocks(lines):
    """Split top-level stanza list entries into (start, end) index ranges."""
    starts = [i for i, l in enumerate(lines) if START.match(l)]
    out = []
    for n, i in enumerate(starts):
        end = starts[n + 1] if n + 1 < len(starts) else len(lines)
        out.append((i, end))
    return out


def stamp(path):
    lines = open(path).read().splitlines()
    changed = 0
    # Reverse order: an insertion shifts every later block, so walk from the end
    # and the indices of the blocks still to visit stay valid.
    for start, end in reversed(blocks(lines)):
        m = START.match(lines[start])
        if not m:
            continue
        indent, model_id = m.group(1), m.group(2)
        body = "\n".join(lines[start:end])
        if not re.search(r"^\s*api_type:\s*image\s*$", body, re.M):
            continue
        # Only sdapi-matching stanzas: an explicit sdapi prefix, or no match block
        # at all with api_type image and a port (the plain sd-server case).
        if re.search(r"^\s*body_field:", body, re.M):
            continue
        prefix = re.search(r"^\s*path_prefix:\s*(\S+)", body, re.M)
        if prefix and not prefix.group(1).startswith("/sdapi/v1"):
            continue
        want = ["img2img"] + (["inpaint"] if model_id in INPAINT else [])
        cap_line = re.search(r"^(\s*)capabilities:\s*\[(.*?)\]\s*$", body, re.M)
        if cap_line:
            have = [c.strip() for c in cap_line.group(2).split(",") if c.strip()]
            missing = [c for c in want if c not in have]
            if not missing:
                continue
            new = "capabilities: [" + ", ".join(have + missing) + "]"
            abs_start = start + body[: cap_line.start()].count("\n")
            lines[abs_start] = cap_line.group(1) + new
        else:
            # Insert right after model_id, before any comment-free key.
            lines.insert(start + 1, f"{indent}  capabilities: [{', '.join(want)}]")
        changed += 1
    if changed:
        open(path, "w").write("\n".join(lines) + "\n")
    return changed


if __name__ == "__main__":
    total = 0
    for p in sys.argv[1:]:
        n = stamp(p)
        total += n
        print(f"{p}: {n} stanza(s) stamped")
    # The tool is idempotent: a second run stamps nothing, which is success, not
    # failure. There is no error a non-zero exit could signal here.
    sys.exit(0)
