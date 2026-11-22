#!/usr/bin/env bash
# Build a patched sd-server that includes Z-Image reference tokens in PE.
# Writes into a separate build directory. Never overwrites a live binary.
set -euo pipefail

usage() {
  cat <<'USAGE'
Usage: build-sd-server-zimage-ref-patch.sh --src DIR [options]

Build leejet/stable-diffusion.cpp with z-image-ref-pe.patch applied, in a
directory that is not the live sd-server build. The script does not install
the binary, does not edit model-router config, and does not stop any process.

Required:
  --src DIR          Existing stable-diffusion.cpp checkout. Prefer a worktree
                     or copy, not the live tree that already has a dirty build/.

Options:
  --build-dir DIR    CMake build directory (default: <src>/build-zimage-ref-patch)
  --patch FILE       Patch to apply (default: repo third_party/.../z-image-ref-pe.patch)
  --arch LIST        CMAKE/GGML CUDA architectures (default: 70;86)
  --nvcc PATH        nvcc (default: /usr/local/cuda-12.9/bin/nvcc if present)
  --jobs N           Parallel compile jobs (default: 12)
  --skip-apply       Source already has the patch; do not run git apply
  -h, --help         Show this help

Example:
  deploy/build-sd-server-zimage-ref-patch.sh \
    --src "$HOME/infra/sd-cpp-zimage-ref-patch"

Then run the new binary from <build-dir>/bin/sd-server on a spare loopback
port to test. Point only the z-anime-turbo stanza at it after approval.
USAGE
}

SRC=""
BUILD_DIR=""
PATCH=""
ARCH="70;86"
NVCC=""
JOBS=12
SKIP_APPLY=0

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "$SCRIPT_DIR/.." && pwd)
DEFAULT_PATCH="$REPO_ROOT/third_party/stable-diffusion.cpp/z-image-ref-pe.patch"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --src) SRC=$2; shift 2 ;;
    --build-dir) BUILD_DIR=$2; shift 2 ;;
    --patch) PATCH=$2; shift 2 ;;
    --arch) ARCH=$2; shift 2 ;;
    --nvcc) NVCC=$2; shift 2 ;;
    --jobs) JOBS=$2; shift 2 ;;
    --skip-apply) SKIP_APPLY=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

if [[ -z "$SRC" ]]; then
  echo "--src is required" >&2
  usage >&2
  exit 2
fi
if [[ ! -d "$SRC" ]]; then
  echo "source directory not found: $SRC" >&2
  exit 1
fi
PATCH=${PATCH:-$DEFAULT_PATCH}
BUILD_DIR=${BUILD_DIR:-"$SRC/build-zimage-ref-patch"}

# Refuse to write into a directory named exactly "build" under a tree that
# already looks like the live checkout, unless the caller set --build-dir to
# a different path. The default already avoids "build/".
if [[ "$(basename "$BUILD_DIR")" == "build" ]]; then
  echo "refusing to use a build directory named 'build' (that is the live default)." >&2
  echo "pass --build-dir with a distinct name, for example build-zimage-ref-patch." >&2
  exit 1
fi

if [[ -z "$NVCC" && -x /usr/local/cuda-12.9/bin/nvcc ]]; then
  NVCC=/usr/local/cuda-12.9/bin/nvcc
fi

if [[ "$SKIP_APPLY" -eq 0 ]]; then
  if [[ ! -f "$PATCH" ]]; then
    echo "patch not found: $PATCH" >&2
    exit 1
  fi
  if grep -q 'ignore ref_latents for now' "$SRC/src/model/common/rope.hpp" 2>/dev/null; then
    git -C "$SRC" apply --check "$PATCH"
    git -C "$SRC" apply "$PATCH"
    echo "applied $PATCH"
  elif grep -q 'Z-Image edit: reference and target share spatial RoPE' "$SRC/src/model/common/rope.hpp" 2>/dev/null; then
    echo "source already has the patch, continuing"
  else
    echo "cannot tell if the patch is already applied; inspect $SRC/src/model/common/rope.hpp" >&2
    exit 1
  fi
fi

CMAKE_ARGS=(
  -S "$SRC"
  -B "$BUILD_DIR"
  -G Ninja
  -DSD_CUDA=ON
  -DCMAKE_BUILD_TYPE=Release
  -DGGML_CUDA_ARCHITECTURES="$ARCH"
  -DCMAKE_CUDA_ARCHITECTURES="$ARCH"
  -DGGML_CUDA_COMPRESSION_MODE=none
)
if [[ -n "$NVCC" ]]; then
  CMAKE_ARGS+=(-DCMAKE_CUDA_COMPILER="$NVCC")
fi

cmake "${CMAKE_ARGS[@]}"
cmake --build "$BUILD_DIR" --config Release --target sd-server sd-cli -j "$JOBS"

echo
echo "built:"
echo "  $BUILD_DIR/bin/sd-server"
echo "  $BUILD_DIR/bin/sd-cli"
if [[ -x "$BUILD_DIR/bin/sd-server" ]]; then
  "$BUILD_DIR/bin/sd-server" --version || true
fi
echo "This script did not install the binary or change any router config."
