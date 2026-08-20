#!/bin/bash
# Build TheTom llama-cpp-turboquant llama-server into $SRC/build-fresh.
# Usage: build-turboquant.sh SRC ARCH NVCC [JOBS]
# Example: build-turboquant.sh /home/kevyn/llama-cpp-turboquant-fresh '70;89' /usr/local/cuda/bin/nvcc 20
set -euo pipefail

SRC=${1:?src dir}
ARCH=${2:?cuda arch e.g. 70 or 70;89 or 86 or 120a}
NVCC=${3:?nvcc path}
JOBS=${4:-$(nproc)}
REPO=https://github.com/TheTom/llama-cpp-turboquant.git
BRANCH=feature/turboquant-kv-cache

echo "=== START $(date -Is) host=$(hostname) ==="
echo "SRC=$SRC ARCH=$ARCH NVCC=$NVCC JOBS=$JOBS"

if [ ! -x "$NVCC" ]; then
  echo "nvcc not executable: $NVCC" >&2
  exit 1
fi
export PATH="$(dirname "$NVCC"):${PATH:-/usr/bin:/bin}"
export CUDACXX="$NVCC"
"$NVCC" --version | tail -1

if [ ! -d "$SRC/.git" ]; then
  mkdir -p "$(dirname "$SRC")"
  git clone --branch "$BRANCH" --single-branch --depth 1 "$REPO" "$SRC"
else
  git -C "$SRC" fetch --depth 1 origin "$BRANCH"
  git -C "$SRC" checkout -B "$BRANCH" FETCH_HEAD
fi
echo "commit=$(git -C "$SRC" rev-parse --short HEAD) $(git -C "$SRC" log -1 --pretty=%s)"

cmake -S "$SRC" -B "$SRC/build-fresh" \
  -DCMAKE_BUILD_TYPE=Release \
  -DCMAKE_CUDA_COMPILER="$NVCC" \
  -DCMAKE_CUDA_ARCHITECTURES="$ARCH" \
  -DGGML_CUDA=ON \
  -DGGML_CUDA_F16=ON \
  -DGGML_CUDA_FA=ON \
  -DGGML_CUDA_GRAPHS=ON \
  -DGGML_NATIVE=ON \
  -DLLAMA_BUILD_TESTS=OFF \
  -DLLAMA_BUILD_EXAMPLES=OFF \
  -DLLAMA_BUILD_SERVER=ON

cmake --build "$SRC/build-fresh" --config Release -j"$JOBS" --target llama-server

BIN="$SRC/build-fresh/bin/llama-server"
export LD_LIBRARY_PATH="$SRC/build-fresh/bin${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
ls -l "$BIN"
if ! "$BIN" --help 2>/dev/null | grep -qi turbo4; then
  echo "ERROR: llama-server missing turbo4 cache types" >&2
  "$BIN" --help 2>/dev/null | grep -i cache-type | head -20 || true
  exit 1
fi
"$BIN" --help 2>/dev/null | grep -i turbo | head
echo "=== DONE $(date -Is) bin=$BIN ==="
