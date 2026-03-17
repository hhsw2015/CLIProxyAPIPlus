FROM golang:1.23-alpine AS builder

WORKDIR /app

# Install dependencies
RUN apk add --no-cache git

# Copy source
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o warp-pool .

# Runtime image
FROM alpine:3.20

WORKDIR /app

# Install dependencies
RUN apk add --no-cache ca-certificates curl

# Copy binary and config
COPY --from=builder /app/warp-pool .
COPY config.yaml .

# Download WARP CLI (for Linux/amd64)
ARG WARP_VERSION=2024.12.554.0
RUN curl -L "https://pkg.cloudflareclient.com/pool/noble/main/c/cloudflare-warp/cloudflare-warp_${WARP_VERSION}-1_amd64.deb" -o /tmp/warp.deb && \
    cd /tmp && \
    ar x warp.deb && \
    tar xf data.tar.* 2>/dev/null || tar xf data.tar.zst --use-compress-program=unzstd && \
    mv usr/bin/warp-cli /app/warp && \
    chmod +x /app/warp && \
    rm -rf /tmp/*

# Create data directory
RUN mkdir -p /app/data

# Expose ports
EXPOSE 1080 8118 9090

# Run
CMD ["./warp-pool", "-config", "config.yaml"]
