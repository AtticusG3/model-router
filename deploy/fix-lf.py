#!/usr/bin/env python3
import pathlib
import sys
for p in sys.argv[1:]:
    path = pathlib.Path(p)
    data = path.read_bytes().replace(b"\r\n", b"\n").replace(b"\r", b"\n")
    path.write_bytes(data)
    path.chmod(0o755)
