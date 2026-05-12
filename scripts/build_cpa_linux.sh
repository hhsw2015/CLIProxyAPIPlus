#!/usr/bin/env bash
# Build cpa-new-server (Linux x86_64 via Docker + Mac arm64 native).
# headroom-FFI enabled, commercial+embed, no VPS dependency.
#
# Output:
#   /tmp/cpa-release/cpa-new-server                  (Linux ELF, ~80MB)
#   /tmp/cpa-release/libheadroom_ffi.so              (40MB, ship next to Linux binary)
#   /tmp/cpa-release/cpa-new-server-mac-arm64        (Mach-O arm64, ~80MB)
#   /tmp/cpa-release/libheadroom_ffi.dylib           (38MB, ship next to Mac binary)
#
# Targets selected via TARGETS env (default "linux mac"). E.g.:
#   TARGETS=linux bash scripts/build_cpa_linux.sh
#   TARGETS=mac   bash scripts/build_cpa_linux.sh
#
# Mounts:
#   /Users/wowdd1/Dev/CLIProxyAPIPlus  → /work/cpa
#   /Users/wowdd1/Dev/sub2api/backend  → /work/sub2api/backend (replace target)

set -euo pipefail

CPA_DIR="${CPA_DIR:-/Users/wowdd1/Dev/CLIProxyAPIPlus}"
SUB2API_DIR="${SUB2API_DIR:-/Users/wowdd1/Dev/sub2api/backend}"
OUT_DIR="${OUT_DIR:-/tmp/cpa-release}"
HEADROOM_RELEASE="${HEADROOM_RELEASE:-headroom-ffi-v0.1.0}"
HEADROOM_REPO="${HEADROOM_REPO:-hhsw2015/headroom}"
GO_IMAGE="${GO_IMAGE:-golang:1.26-bookworm}"
TARGETS="${TARGETS:-linux mac}"

if [[ ! -d "${CPA_DIR}" ]]; then echo "missing ${CPA_DIR}" >&2; exit 1; fi
if [[ ! -d "${SUB2API_DIR}" ]]; then echo "missing ${SUB2API_DIR}" >&2; exit 1; fi
if [[ ! -f "${SUB2API_DIR}/internal/web/dist/index.html" ]]; then
  echo "missing ${SUB2API_DIR}/internal/web/dist/ — frontend not built" >&2
  exit 1
fi

mkdir -p "${OUT_DIR}"

VERSION="$(cd "${CPA_DIR}" && git rev-parse --short HEAD 2>/dev/null || echo headroom-ffi)"

build_linux() {
  echo "=== Linux x86_64 (docker ${GO_IMAGE}) ==="
  docker run --rm --platform linux/amd64 \
    -v "${CPA_DIR}:/work/cpa" \
    -v "${SUB2API_DIR}:/work/sub2api/backend" \
    -v "${OUT_DIR}:/out" \
    -e VERSION="${VERSION}" \
    -e HEADROOM_RELEASE="${HEADROOM_RELEASE}" \
    -e HEADROOM_REPO="${HEADROOM_REPO}" \
    -w /work/cpa \
    "${GO_IMAGE}" \
    bash -c '
set -euo pipefail
apt-get update -qq && apt-get install -qq -y curl ca-certificates >/dev/null

LIBDIR=/work/lib
mkdir -p "$LIBDIR"
echo "fetching libheadroom_ffi-linux-x86_64.so"
curl -fL --retry 3 -o "$LIBDIR/libheadroom_ffi.so" \
  "https://github.com/${HEADROOM_REPO}/releases/download/${HEADROOM_RELEASE}/libheadroom_ffi-linux-x86_64.so"
echo "fetching libglibc_shim-linux-x86_64.a"
curl -fL --retry 3 -o "$LIBDIR/libglibc_shim.a" \
  "https://github.com/${HEADROOM_REPO}/releases/download/${HEADROOM_RELEASE}/libglibc_shim-linux-x86_64.a"

cp go.mod /tmp/go.mod.bak
trap "cp /tmp/go.mod.bak go.mod" EXIT
sed -i "s|/Users/wowdd1/Dev/sub2api/backend|/work/sub2api/backend|g" go.mod

CGO_ENABLED=1 \
CGO_LDFLAGS="-L${LIBDIR} -lheadroom_ffi -Wl,-rpath,\$ORIGIN -Wl,-z,origin ${LIBDIR}/libglibc_shim.a" \
CGO_LDFLAGS_ALLOW="-Wl,-rpath,.*|-Wl,-z,origin|.*libglibc_shim\\.a" \
  go build \
    -trimpath \
    -tags commercial,embed \
    -ldflags="-X github.com/router-for-me/CLIProxyAPI/v7/internal/buildinfo.Version=${VERSION}" \
    -o /out/cpa-new-server ./cmd/server/

cp "$LIBDIR/libheadroom_ffi.so" /out/
ls -la /out/cpa-new-server /out/libheadroom_ffi.so
file /out/cpa-new-server
'
}

build_mac() {
  echo "=== Mac arm64 (native) ==="
  command -v go >/dev/null || { echo "go not in PATH" >&2; exit 1; }
  CACHE_DIR="${HOME}/.cache/headroom-ffi/${HEADROOM_RELEASE}"
  mkdir -p "${CACHE_DIR}"
  if [[ ! -f "${CACHE_DIR}/libheadroom_ffi.dylib" ]]; then
    curl -fL --retry 3 -o "${CACHE_DIR}/libheadroom_ffi.dylib" \
      "https://github.com/${HEADROOM_REPO}/releases/download/${HEADROOM_RELEASE}/libheadroom_ffi-darwin-arm64.dylib"
    chmod +x "${CACHE_DIR}/libheadroom_ffi.dylib"
  fi
  cd "${CPA_DIR}"
  CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 \
  CGO_LDFLAGS="-L${CACHE_DIR} -lheadroom_ffi -Wl,-rpath,@executable_path" \
  CGO_LDFLAGS_ALLOW='-Wl,-rpath,.*' \
    go build \
      -trimpath \
      -tags commercial,embed \
      -ldflags="-X github.com/router-for-me/CLIProxyAPI/v7/internal/buildinfo.Version=${VERSION}" \
      -o "${OUT_DIR}/cpa-new-server-mac-arm64" ./cmd/server/
  cp "${CACHE_DIR}/libheadroom_ffi.dylib" "${OUT_DIR}/"
  ls -la "${OUT_DIR}/cpa-new-server-mac-arm64" "${OUT_DIR}/libheadroom_ffi.dylib"
  file "${OUT_DIR}/cpa-new-server-mac-arm64"
}

for t in ${TARGETS}; do
  case "$t" in
    linux) build_linux ;;
    mac)   build_mac ;;
    *)     echo "unknown target: $t" >&2; exit 1 ;;
  esac
done

echo
echo "OK. Outputs in ${OUT_DIR}:"
ls -lh "${OUT_DIR}"
