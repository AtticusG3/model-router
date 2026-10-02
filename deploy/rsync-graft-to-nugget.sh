#!/bin/bash
set -eu
SRC=/home/kevyn/models/Ornith-1.5-35B-A3B-BigBang-MTP-Q5_K_M-grafted.gguf
DST=nugget:/opt/ai/models/llm/Ornith-1.5-35B-A3B-BigBang-MTP-Q5_K_M-grafted.gguf
ls -l "$SRC"
rsync -avP --inplace "$SRC" "$DST"
