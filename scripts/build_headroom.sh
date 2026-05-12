#!/usr/bin/env bash
# Build CPA with headroom FFI compression enabled.
#
# Requires the headroom-ffi crate to be built first:
#   cd ${HEADROOM_ROOT:-$HOME/Dev/headroom} && cargo build --release -p headroom-ffi
#
# Output binary expects libheadroom_ffi.{dylib,so} to be located via @rpath
# (next to the binary, or in ./lib/), so deployment must ship the .so/.dylib
# alongside the binary.

set -euo pipefail

HEADROOM_ROOT="${HEADROOM_ROOT:-$HOME/Dev/headroom}"
HEADROOM_LIB_DIR="${HEADROOM_ROOT}/target/release"
OUT="${OUT:-./bin/cli-proxy-api}"

case "$(uname -s)" in
  Darwin)
    RPATH_FLAGS="-Wl,-rpath,@executable_path"
    LIB_NAME="libheadroom_ffi.dylib"
    ;;
  Linux)
    RPATH_FLAGS="-Wl,-rpath,\$ORIGIN -Wl,-z,origin"
    LIB_NAME="libheadroom_ffi.so"
    ;;
  *)
    echo "unsupported OS: $(uname -s)" >&2
    exit 1
    ;;
esac

if [[ ! -f "${HEADROOM_LIB_DIR}/${LIB_NAME}" ]]; then
  echo "missing ${HEADROOM_LIB_DIR}/${LIB_NAME}" >&2
  echo "build it: (cd ${HEADROOM_ROOT} && cargo build --release -p headroom-ffi)" >&2
  exit 1
fi

OUT_DIR="$(dirname "${OUT}")"
mkdir -p "${OUT_DIR}"

CGO_LDFLAGS="-L${HEADROOM_LIB_DIR} ${RPATH_FLAGS}" \
CGO_LDFLAGS_ALLOW='-Wl,-rpath,.*|-Wl,-z,origin' \
  go build -o "${OUT}" ./cmd/server/

# Stage the dylib next to the binary so @rpath/$ORIGIN resolves at runtime.
# Remove stale copies first to avoid "ld: file already exists" / kernel cache
# pinning the previous build's mmap on macOS.
rm -f "${OUT_DIR}/${LIB_NAME}"
cp "${HEADROOM_LIB_DIR}/${LIB_NAME}" "${OUT_DIR}/"

echo "built ${OUT}"
echo "shipped ${LIB_NAME} to $(dirname "${OUT}")/"
