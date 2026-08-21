#!/usr/bin/env python3
"""Dump selected GGUF metadata keys and tensor name prefixes. Stdlib only."""
import struct
import sys

GGUF_MAGIC = b"GGUF"
# value types: 0 u8, 1 i8, 2 u16, 3 i16, 4 u32, 5 i32, 6 f32, 7 bool, 8 string, 9 array, 10 u64, 11 i64, 12 f64
STRING = 8
ARRAY = 9

def rstr(f):
    n = struct.unpack("<Q", f.read(8))[0]
    return f.read(n).decode("utf-8", "replace")

def skip_val(f, t):
    if t == 0 or t == 1 or t == 7:
        f.read(1)
    elif t == 2 or t == 3:
        f.read(2)
    elif t in (4, 5, 6):
        f.read(4)
    elif t in (10, 11, 12):
        f.read(8)
    elif t == STRING:
        rstr(f)
    elif t == ARRAY:
        at = struct.unpack("<I", f.read(4))[0]
        n = struct.unpack("<Q", f.read(8))[0]
        for _ in range(n):
            skip_val(f, at)
    else:
        raise SystemExit("unknown type %s" % t)

def read_val(f, t):
    if t == 4:
        return struct.unpack("<I", f.read(4))[0]
    if t == 5:
        return struct.unpack("<i", f.read(4))[0]
    if t == 10:
        return struct.unpack("<Q", f.read(8))[0]
    if t == 11:
        return struct.unpack("<q", f.read(8))[0]
    if t == 7:
        return bool(f.read(1)[0])
    if t == STRING:
        return rstr(f)
    if t == 6:
        return struct.unpack("<f", f.read(4))[0]
    skip_val(f, t)
    return "<skipped>"

WANT = (
    "general.architecture",
    "general.name",
    "general.basename",
    "general.size_label",
    "general.type",
    "general.file_type",
    "qwen3moe.block_count",
    "qwen3.block_count",
    "qwen35moe.block_count",
    "qwen3_5.block_count",
    "llama.block_count",
    "qwen3moe.expert_count",
    "qwen35moe.expert_count",
    "qwen3moe.embedding_length",
    "qwen35moe.embedding_length",
    "tokenizer.ggml.model",
    "tokenizer.ggml.tokens",
)

def dump(path):
    print("===", path)
    with open(path, "rb") as f:
        mag = f.read(4)
        if mag != GGUF_MAGIC:
            print("not gguf", mag)
            return
        ver = struct.unpack("<I", f.read(4))[0]
        n_tensors, n_kv = struct.unpack("<QQ", f.read(16))
        print("gguf_version", ver, "tensors", n_tensors, "kv", n_kv)
        for _ in range(n_kv):
            key = rstr(f)
            t = struct.unpack("<I", f.read(4))[0]
            if key in WANT or "block_count" in key or "nextn" in key or "architecture" in key or key.endswith(".vocab_size") or "expert_count" in key or "embedding_length" in key:
                if t == ARRAY and key == "tokenizer.ggml.tokens":
                    at = struct.unpack("<I", f.read(4))[0]
                    n = struct.unpack("<Q", f.read(8))[0]
                    print(key, "array_len", n)
                    for _i in range(n):
                        skip_val(f, at)
                else:
                    print(key, "=", read_val(f, t))
            else:
                skip_val(f, t)
        names = []
        for i in range(n_tensors):
            name = rstr(f)
            n_dims = struct.unpack("<I", f.read(4))[0]
            f.read(8 * n_dims)
            f.read(4)  # type
            f.read(8)  # offset
            if i < 8 or i >= n_tensors - 4:
                names.append(name)
        print("first_tensors", names[:8])
        print("last_tensors", names[-4:])

for p in sys.argv[1:]:
    dump(p)
    print()
