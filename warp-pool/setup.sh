#!/bin/bash
set -e

# WARP Pool Setup Script
# Downloads and installs Cloudflare WARP CLI

WARP_VERSION="2024.12.554.0"
WARP_DIR="$(dirname "$0")"
DATA_DIR="${WARP_DIR}/data"

echo "=== WARP Pool Setup ==="

# Detect OS and architecture
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)

case "$ARCH" in
    x86_64)
        ARCH="amd64"
        ;;
    aarch64|arm64)
        ARCH="arm64"
        ;;
    *)
        echo "Unsupported architecture: $ARCH"
        exit 1
        ;;
esac

echo "Detected: $OS/$ARCH"

# Download WARP CLI
download_warp() {
    local url=""
    local output="${WARP_DIR}/warp"

    case "$OS" in
        darwin)
            echo "Note: On macOS, install WARP from the App Store or use:"
            echo "  brew install cloudflare-warp"
            echo ""
            echo "Then copy the warp-cli binary to this directory:"
            echo "  cp /usr/local/bin/warp-cli ${output}"
            return 1
            ;;
        linux)
            # Download from Cloudflare's package
            url="https://pkg.cloudflareclient.com/pool/noble/main/c/cloudflare-warp/cloudflare-warp_${WARP_VERSION}-1_${ARCH}.deb"
            ;;
        *)
            echo "Unsupported OS: $OS"
            exit 1
            ;;
    esac

    echo "Downloading WARP..."
    local tmp_dir=$(mktemp -d)
    local deb_file="${tmp_dir}/cloudflare-warp.deb"

    if command -v curl &> /dev/null; then
        curl -L -o "$deb_file" "$url"
    elif command -v wget &> /dev/null; then
        wget -O "$deb_file" "$url"
    else
        echo "Error: curl or wget required"
        exit 1
    fi

    # Extract warp-cli from deb
    echo "Extracting..."
    cd "$tmp_dir"
    ar x "$deb_file"
    tar xf data.tar.* 2>/dev/null || tar xf data.tar.zst --use-compress-program=unzstd 2>/dev/null || {
        echo "Error: Failed to extract. Try installing zstd."
        exit 1
    }

    cp usr/bin/warp-cli "$output"
    chmod +x "$output"

    # Cleanup
    cd -
    rm -rf "$tmp_dir"

    echo "WARP CLI installed to: $output"
}

# Create data directories
setup_dirs() {
    echo "Creating data directories..."
    mkdir -p "$DATA_DIR"

    # Read pool size from config
    local pool_size=$(grep "pool_size:" "${WARP_DIR}/config.yaml" 2>/dev/null | awk '{print $2}')
    pool_size=${pool_size:-5}

    for i in $(seq 0 $((pool_size - 1))); do
        mkdir -p "${DATA_DIR}/warp-${i}"
    done

    echo "Created ${pool_size} instance directories"
}

# Build the binary
build() {
    echo "Building warp-pool..."
    cd "$WARP_DIR"
    go build -o warp-pool .
    echo "Built: ${WARP_DIR}/warp-pool"
}

# Main
main() {
    case "${1:-all}" in
        warp)
            download_warp
            ;;
        dirs)
            setup_dirs
            ;;
        build)
            build
            ;;
        all)
            download_warp || true
            setup_dirs
            build
            ;;
        *)
            echo "Usage: $0 [warp|dirs|build|all]"
            exit 1
            ;;
    esac

    echo ""
    echo "=== Setup Complete ==="
    echo "Run: ./warp-pool -config config.yaml"
}

main "$@"
