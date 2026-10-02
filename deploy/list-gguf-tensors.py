#!/usr/bin/env python3
import os, sys
_extra = os.environ.get("GGUF_PY", "")
if _extra:
    sys.path.insert(0, _extra)
from gguf import GGUFReader
r = GGUFReader(sys.argv[1])
print("tensors", len(r.tensors))
for t in r.tensors:
    print(t.name, t.tensor_type.name, t.n_bytes)
