# Headroom is MB the node will not book

Admission was willing to book the last free megabyte, and peers selected candidates from that same raw free figure. `headroom_mb` is one integer on the node, subtracted once from each GPU before a `vram_mb` check, default 0. The same subtraction comes off every free figure the node publishes, including free-if-evicted. It is not a percent of the card and not a haircut on `vram_mb`. A percent would mean a different spare on a 4060 and a V100. Publishing raw `nvidia-smi` free would send this node loads it is required to refuse.
