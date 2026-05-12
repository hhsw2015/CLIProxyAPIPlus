#!/usr/bin/env bash
# Build CPA with headroom FFI compression enabled.
#
# By default fetches a prebuilt libheadroom_ffi.{dylib,so} from the
# headroom GitHub release. Set HEADROOM_ROOT to use a local build
# instead (cargo build --release -p headroom-ffi in that directory).
#
# Output binary expects the lib next to it via @rpath/$ORIGIN, so
# deployment must ship the .so/.dylib alongside the binary.
#
# Env knobs:
#   HEADROOM_ROOT       — local checkout (skips download if set)
#   HEADROOM_RELEASE    — release tag (default: headroom-ffi-v0.1.0)
#   HEADROOM_REPO       — GH repo (default: hhsw2015/headroom)
#   HEADROOM_CACHE_DIR  — download cache (default: $HOME/.cache/headroom-ffi)
#   OUT                 — output binary path (default: ./bin/cli-proxy-api)
#   GLIBC_SHIM          — link shim for glibc <2.38 hosts (Linux only, 0/1)

set -euo pipefail

HEADROOM_RELEASE="${HEADROOM_RELEASE:-headroom-ffi-v0.1.0}"
HEADROOM_REPO="${HEADROOM_REPO:-hhsw2015/headroom}"
HEADROOM_CACHE_DIR="${HEADROOM_CACHE_DIR:-$HOME/.cache/headroom-ffi/${HEADROOM_RELEASE}}"
OUT="${OUT:-./bin/cli-proxy-api}"
GLIBC_SHIM="${GLIBC_SHIM:-0}"

case "$(uname -s)-$(uname -m)" in
  Darwin-arm64)
    RPATH_FLAGS="-Wl,-rpath,@executable_path"
    LIB_NAME="libheadroom_ffi.dylib"
    RELEASE_LIB="libheadroom_ffi-darwin-arm64.dylib"
    ;;
  Linux-x86_64)
    RPATH_FLAGS="-Wl,-rpath,\$ORIGIN -Wl,-z,origin"
    LIB_NAME="libheadroom_ffi.so"
    RELEASE_LIB="libheadroom_ffi-linux-x86_64.so"
    ;;
  *)
    echo "unsupported platform: $(uname -s)-$(uname -m)" >&2
    exit 1
    ;;
esac

if [[ -n "${HEADROOM_ROOT:-}" ]]; then
  HEADROOM_LIB_DIR="${HEADROOM_ROOT}/target/release"
  if [[ ! -f "${HEADROOM_LIB_DIR}/${LIB_NAME}" ]]; then
    echo "missing ${HEADROOM_LIB_DIR}/${LIB_NAME}" >&2
    echo "build it: (cd ${HEADROOM_ROOT} && cargo build --release -p headroom-ffi)" >&2
    exit 1
  fi
  echo "using local build at ${HEADROOM_LIB_DIR}"
else
  HEADROOM_LIB_DIR="${HEADROOM_CACHE_DIR}"
  mkdir -p "${HEADROOM_LIB_DIR}"
  if [[ ! -f "${HEADROOM_LIB_DIR}/${LIB_NAME}" ]]; then
    echo "fetching ${RELEASE_LIB} from ${HEADROOM_REPO}@${HEADROOM_RELEASE}"
    URL="https://github.com/${HEADROOM_REPO}/releases/download/${HEADROOM_RELEASE}/${RELEASE_LIB}"
    curl -fL --retry 3 -o "${HEADROOM_LIB_DIR}/${LIB_NAME}" "${URL}"
    chmod +x "${HEADROOM_LIB_DIR}/${LIB_NAME}"
  fi
  if [[ "${GLIBC_SHIM}" == "1" && "$(uname -s)" == "Linux" ]]; then
    if [[ ! -f "${HEADROOM_LIB_DIR}/libglibc_shim.a" ]]; then
      echo "fetching libglibc_shim-linux-x86_64.a"
      curl -fL --retry 3 \
        -o "${HEADROOM_LIB_DIR}/libglibc_shim.a" \
        "https://github.com/${HEADROOM_REPO}/releases/download/${HEADROOM_RELEASE}/libglibc_shim-linux-x86_64.a"
    fi
  fi
fi

OUT_DIR="$(dirname "${OUT}")"
mkdir -p "${OUT_DIR}"

CGO_LDFLAGS_VAL="-L${HEADROOM_LIB_DIR} ${RPATH_FLAGS}"
if [[ "${GLIBC_SHIM}" == "1" && "$(uname -s)" == "Linux" ]]; then
  CGO_LDFLAGS_VAL="${CGO_LDFLAGS_VAL} ${HEADROOM_LIB_DIR}/libglibc_shim.a"
fi

CGO_LDFLAGS="${CGO_LDFLAGS_VAL}" \
CGO_LDFLAGS_ALLOW='-Wl,-rpath,.*|-Wl,-z,origin|.*libglibc_shim\.a' \
  go build -o "${OUT}" ./cmd/server/

# Stage the lib next to the binary so @rpath/$ORIGIN resolves at runtime.
# Remove stale copies first; macOS kernel can pin the previous build's mmap.
rm -f "${OUT_DIR}/${LIB_NAME}"
cp "${HEADROOM_LIB_DIR}/${LIB_NAME}" "${OUT_DIR}/"

echo "built ${OUT}"
echo "shipped ${LIB_NAME} to ${OUT_DIR}/"
