#!/usr/bin/env bash
# Build cpa-new-server (Linux x86_64 + Mac arm64).
#
# Default (no FFI, pure Go, no Docker needed):
#   bash scripts/build_cpa_linux.sh
#   -> /tmp/cpa-release/cpa-new-server            (Linux ELF, ~160MB, statically linked)
#   -> /tmp/cpa-release/cpa-new-server-mac-arm64  (Mach-O arm64, ~160MB)
#
# Legacy FFI path (in-process compression, requires Docker for glibc linking):
#   HEADROOM_FFI=1 bash scripts/build_cpa_linux.sh
#   -> adds -tags headroom_ffi, links libheadroom_ffi.{so,dylib} alongside binary
#
# Target selection: TARGETS=linux | TARGETS=mac | TARGETS="linux mac" (default).

set -euo pipefail

CPA_DIR="${CPA_DIR:-/Users/wowdd1/Dev/CLIProxyAPIPlus}"
SUB2API_DIR="${SUB2API_DIR:-/Users/wowdd1/Dev/sub2api/backend}"
OUT_DIR="${OUT_DIR:-/tmp/cpa-release}"
HEADROOM_RELEASE="${HEADROOM_RELEASE:-headroom-ffi-v0.1.0}"
HEADROOM_REPO="${HEADROOM_REPO:-hhsw2015/headroom}"
GO_IMAGE="${GO_IMAGE:-golang:1.26-bookworm}"
TARGETS="${TARGETS:-linux mac}"
HEADROOM_FFI="${HEADROOM_FFI:-0}"

if [[ ! -d "${CPA_DIR}" ]]; then echo "missing ${CPA_DIR}" >&2; exit 1; fi

# sub2api is OPTIONAL. It is only imported under the `commercial` build tag
# (internal/commercial/*: billing, data-sync, middleware). If the local
# sub2api checkout is present (with its built frontend), we build WITH the
# commercial features; otherwise we drop the `commercial` tag and build a
# lean binary. Go ignores the unused go.mod replace when nothing under the
# active tags imports sub2api, so the missing directory is not an error.
COMMERCIAL_TAG=""
if [[ -d "${SUB2API_DIR}" && -f "${SUB2API_DIR}/internal/web/dist/index.html" ]]; then
  COMMERCIAL_TAG="commercial,"
  echo "sub2api found at ${SUB2API_DIR} — building WITH commercial features"
else
  echo "WARNING: sub2api not found at ${SUB2API_DIR}" >&2
  echo "         -> building WITHOUT commercial features (billing/data-sync/middleware disabled)." >&2
  echo "         Set SUB2API_DIR to a checkout with internal/web/dist/ to re-enable." >&2
fi

mkdir -p "${OUT_DIR}"
VERSION="$(cd "${CPA_DIR}" && git rev-parse --short HEAD 2>/dev/null || echo unknown)"

if [[ "${HEADROOM_FFI}" == "1" ]]; then
  if [[ -z "${COMMERCIAL_TAG}" ]]; then
    echo "HEADROOM_FFI=1 needs the sub2api checkout (Docker mount + go.mod rewrite)." >&2
    echo "Provide SUB2API_DIR or unset HEADROOM_FFI." >&2
    exit 1
  fi
  BUILD_TAGS="${COMMERCIAL_TAG}embed,headroom_ffi"
  echo "=== Building WITH headroom FFI (tags: ${BUILD_TAGS}, Docker required for Linux) ==="
else
  BUILD_TAGS="${COMMERCIAL_TAG}embed"
  echo "=== Building WITHOUT headroom FFI (tags: ${BUILD_TAGS}, pure Go) ==="
  echo "    Python headroom-proxy is expected to sit in front of CPA."
  echo "    Set HEADROOM_FFI=1 to re-enable the legacy in-process FFI path."
fi

build_linux_no_ffi() {
  echo "=== Linux x86_64 (native cross-compile, CGO_ENABLED=0) ==="
  cd "${CPA_DIR}"
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build \
      -trimpath \
      -tags "${BUILD_TAGS}" \
      -ldflags="-X github.com/router-for-me/CLIProxyAPI/v7/internal/buildinfo.Version=${VERSION}" \
      -o "${OUT_DIR}/cpa-new-server" ./cmd/server/
  rm -f "${OUT_DIR}/libheadroom_ffi.so"
  file "${OUT_DIR}/cpa-new-server"
  ls -lh "${OUT_DIR}/cpa-new-server"
}

build_linux_with_ffi() {
  echo "=== Linux x86_64 (docker ${GO_IMAGE} for glibc linking) ==="
  local local_so_mount=()
  if [[ -n "${HEADROOM_LOCAL_SO:-}" ]]; then
    if [[ ! -f "${HEADROOM_LOCAL_SO}" ]]; then
      echo "HEADROOM_LOCAL_SO=${HEADROOM_LOCAL_SO} not found" >&2; exit 1
    fi
    local_so_mount=(-v "${HEADROOM_LOCAL_SO}:/work/local-libheadroom_ffi.so:ro")
  fi
  docker run --rm --platform linux/amd64 \
    -v "${CPA_DIR}:/work/cpa" \
    -v "${SUB2API_DIR}:/work/sub2api/backend" \
    -v "${OUT_DIR}:/out" \
    "${local_so_mount[@]}" \
    -e VERSION="${VERSION}" \
    -e BUILD_TAGS="${BUILD_TAGS}" \
    -e HEADROOM_RELEASE="${HEADROOM_RELEASE}" \
    -e HEADROOM_REPO="${HEADROOM_REPO}" \
    -e HEADROOM_LOCAL_SO="${HEADROOM_LOCAL_SO:-}" \
    -w /work/cpa \
    "${GO_IMAGE}" \
    bash -c '
set -euo pipefail
apt-get update -qq && apt-get install -qq -y curl ca-certificates >/dev/null

cp go.mod /tmp/go.mod.bak
trap "cp /tmp/go.mod.bak go.mod" EXIT
sed -i "s|/Users/wowdd1/Dev/sub2api/backend|/work/sub2api/backend|g" go.mod

LIBDIR=/work/lib
mkdir -p "$LIBDIR"
if [[ -n "${HEADROOM_LOCAL_SO:-}" && -f /work/local-libheadroom_ffi.so ]]; then
  echo "using local libheadroom_ffi.so override"
  cp /work/local-libheadroom_ffi.so "$LIBDIR/libheadroom_ffi.so"
else
  echo "fetching libheadroom_ffi-linux-x86_64.so"
  curl -fL --retry 3 -o "$LIBDIR/libheadroom_ffi.so" \
    "https://github.com/${HEADROOM_REPO}/releases/download/${HEADROOM_RELEASE}/libheadroom_ffi-linux-x86_64.so"
fi
echo "fetching libglibc_shim-linux-x86_64.a"
curl -fL --retry 3 -o "$LIBDIR/libglibc_shim.a" \
  "https://github.com/${HEADROOM_REPO}/releases/download/${HEADROOM_RELEASE}/libglibc_shim-linux-x86_64.a"

CGO_ENABLED=1 \
CGO_LDFLAGS="-L${LIBDIR} -lheadroom_ffi -Wl,-rpath,\$ORIGIN -Wl,-z,origin ${LIBDIR}/libglibc_shim.a" \
CGO_LDFLAGS_ALLOW="-Wl,-rpath,.*|-Wl,-z,origin|.*libglibc_shim\\.a" \
  go build \
    -trimpath \
    -tags "${BUILD_TAGS}" \
    -ldflags="-X github.com/router-for-me/CLIProxyAPI/v7/internal/buildinfo.Version=${VERSION}" \
    -o /out/cpa-new-server ./cmd/server/
cp "$LIBDIR/libheadroom_ffi.so" /out/
file /out/cpa-new-server
ls -la /out/cpa-new-server /out/libheadroom_ffi.so
'
}

build_mac_no_ffi() {
  echo "=== Mac arm64 (native, CGO_ENABLED=0) ==="
  cd "${CPA_DIR}"
  CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 \
    go build \
      -trimpath \
      -tags "${BUILD_TAGS}" \
      -ldflags="-X github.com/router-for-me/CLIProxyAPI/v7/internal/buildinfo.Version=${VERSION}" \
      -o "${OUT_DIR}/cpa-new-server-mac-arm64" ./cmd/server/
  rm -f "${OUT_DIR}/libheadroom_ffi.dylib"
  file "${OUT_DIR}/cpa-new-server-mac-arm64"
  ls -lh "${OUT_DIR}/cpa-new-server-mac-arm64"
}

build_mac_with_ffi() {
  echo "=== Mac arm64 (native, cgo linking libheadroom_ffi.dylib) ==="
  cd "${CPA_DIR}"
  CACHE_DIR="${HOME}/.cache/headroom-ffi/${HEADROOM_RELEASE}"
  mkdir -p "${CACHE_DIR}"
  if [[ ! -f "${CACHE_DIR}/libheadroom_ffi.dylib" ]]; then
    curl -fL --retry 3 -o "${CACHE_DIR}/libheadroom_ffi.dylib" \
      "https://github.com/${HEADROOM_REPO}/releases/download/${HEADROOM_RELEASE}/libheadroom_ffi-darwin-arm64.dylib"
    chmod +x "${CACHE_DIR}/libheadroom_ffi.dylib"
  fi
  CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 \
  CGO_LDFLAGS="-L${CACHE_DIR} -lheadroom_ffi -Wl,-rpath,@executable_path" \
  CGO_LDFLAGS_ALLOW='-Wl,-rpath,.*' \
    go build \
      -trimpath \
      -tags "${BUILD_TAGS}" \
      -ldflags="-X github.com/router-for-me/CLIProxyAPI/v7/internal/buildinfo.Version=${VERSION}" \
      -o "${OUT_DIR}/cpa-new-server-mac-arm64" ./cmd/server/
  cp "${CACHE_DIR}/libheadroom_ffi.dylib" "${OUT_DIR}/"
  file "${OUT_DIR}/cpa-new-server-mac-arm64"
  ls -lh "${OUT_DIR}/cpa-new-server-mac-arm64" "${OUT_DIR}/libheadroom_ffi.dylib"
}

for t in ${TARGETS}; do
  case "$t" in
    linux)
      if [[ "${HEADROOM_FFI}" == "1" ]]; then build_linux_with_ffi; else build_linux_no_ffi; fi
      ;;
    mac)
      if [[ "${HEADROOM_FFI}" == "1" ]]; then build_mac_with_ffi; else build_mac_no_ffi; fi
      ;;
    *) echo "unknown target: $t" >&2; exit 1 ;;
  esac
done

echo
echo "OK. Outputs in ${OUT_DIR}:"
ls -lh "${OUT_DIR}"
