#!/usr/bin/env python3
"""Graft qwen35moe MTP blk.40.* tensors onto a 40-block GGUF.

Copies every KV from the base except GGUF.* and general.architecture (the
writer emits architecture from the constructor). Patches block_count 40->41
and sets nextn_predict_layers=1. Appends only donor tensors named blk.40.*
(embed/output in the donor file are ignored -- that file is an MTP head, not
a draft model).

Usage:
  PYTHONPATH=/path/to/gguf-py python3 gguf_mtp_graft.py BASE.gguf DONOR.gguf OUT.gguf
"""
from __future__ import annotations

import os
import sys
import time

_extra = os.environ.get("GGUF_PY", "")
if _extra:
    sys.path.insert(0, _extra)

import gguf
from gguf import GGUFReader, GGUFWriter, GGUFValueType


SKIP_KEYS = frozenset({gguf.Keys.General.ARCHITECTURE})
BLOCK_KEYS = ("qwen35moe.block_count", "qwen3moe.block_count")
NEXTN_KEY = "qwen35moe.nextn_predict_layers"


def _is_gguf_internal(name: str) -> bool:
    return name.startswith("GGUF.")


def graft(base_path: str, donor_path: str, out_path: str) -> None:
    t0 = time.time()
    print("base  ", base_path, flush=True)
    print("donor ", donor_path, flush=True)
    print("out   ", out_path, flush=True)
    base = GGUFReader(base_path)
    donor = GGUFReader(donor_path)

    arch = base.fields[gguf.Keys.General.ARCHITECTURE].contents()
    print("arch  ", arch, "endian", base.endianess, flush=True)

    extra = [t for t in donor.tensors if t.name.startswith("blk.40.")]
    if not extra:
        raise SystemExit("donor has no blk.40.* tensors")
    overlap = {t.name for t in base.tensors} & {t.name for t in extra}
    if overlap:
        raise SystemExit("base already has donor tensors: %s" % sorted(overlap)[:8])

    print("base tensors", len(base.tensors), "grafting", len(extra), "blk.40.*", flush=True)
    for t in extra:
        print("  +", t.name, "type", t.tensor_type.name, "nbytes", t.n_bytes, flush=True)

    writer = GGUFWriter(out_path, arch, use_temp_file=False, endianess=base.endianess)
    have_nextn = False
    for field in base.fields.values():
        if _is_gguf_internal(field.name) or field.name in SKIP_KEYS:
            continue
        val_type = field.types[0]
        sub_type = field.types[-1] if val_type == GGUFValueType.ARRAY else None
        val = field.contents()
        if field.name in BLOCK_KEYS:
            print("patch", field.name, val, "->", 41, flush=True)
            val = 41
        if field.name == NEXTN_KEY:
            have_nextn = True
            val = 1
        writer.add_key_value(field.name, val, val_type, sub_type=sub_type)

    if not have_nextn:
        print("add  ", NEXTN_KEY, "= 1", flush=True)
        writer.add_uint32(NEXTN_KEY, 1)

    all_tensors = list(base.tensors) + extra
    for t in all_tensors:
        writer.add_tensor_info(
            t.name, t.data.shape, t.data.dtype, t.data.nbytes, t.tensor_type
        )

    writer.write_header_to_file()
    writer.write_kv_data_to_file()
    writer.write_ti_data_to_file()
    n = len(all_tensors)
    for i, t in enumerate(all_tensors, 1):
        if i == 1 or i == n or i % 50 == 0:
            print("write tensor %d/%d %s" % (i, n, t.name), flush=True)
        writer.write_tensor_data(t.data, tensor_endianess=base.endianess)
    writer.close()
    print("done seconds=%.1f tensors=%d" % (time.time() - t0, n), flush=True)


def verify(path: str) -> None:
    r = GGUFReader(path)
    bc = r.fields.get("qwen35moe.block_count")
    nx = r.fields.get(NEXTN_KEY)
    blk40 = [t.name for t in r.tensors if t.name.startswith("blk.40.")]
    print("verify", path)
    print("  tensors", len(r.tensors))
    print("  qwen35moe.block_count", None if bc is None else bc.contents())
    print("  nextn_predict_layers", None if nx is None else nx.contents())
    print("  blk.40 count", len(blk40))
    for name in blk40:
        print("   ", name)
    if bc is None or bc.contents() != 41:
        raise SystemExit("block_count is not 41")
    if nx is None or nx.contents() != 1:
        raise SystemExit("nextn_predict_layers is not 1")
    if len(blk40) < 8:
        raise SystemExit("too few blk.40 tensors")
    print("verify OK")


if __name__ == "__main__":
    if len(sys.argv) == 3 and sys.argv[1] == "--verify":
        verify(sys.argv[2])
        raise SystemExit(0)
    if len(sys.argv) != 4:
        raise SystemExit("usage: gguf_mtp_graft.py BASE DONOR OUT | --verify FILE")
    graft(sys.argv[1], sys.argv[2], sys.argv[3])
    verify(sys.argv[3])
