#!/usr/bin/env bash
set -euo pipefail

# Build pulseha and pulsectl for Linux/amd64 (Intel) and generate PHP protobuf stubs.

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
VERSION="$(git -C "${REPO_ROOT}" describe --tags 2>/dev/null || echo 'dev')"
BUILD="$(git -C "${REPO_ROOT}" rev-parse HEAD 2>/dev/null || echo 'unknown')"
LDFLAGS="-ldflags \"-X main.Version=${VERSION} -X main.Build=${BUILD}\""

PHP_OUT="${REPO_ROOT}/rpc/php"

# ── helpers ──────────────────────────────────────────────────────────────────

info()  { echo "==> $*"; }
error() { echo "ERROR: $*" >&2; exit 1; }

require() {
    command -v "$1" >/dev/null 2>&1 || error "'$1' not found. $2"
}

# ── 1. Fetch Go dependencies ──────────────────────────────────────────────────

info "Fetching Go dependencies..."
cd "${REPO_ROOT}"
go mod download

# ── 2. Build pulseha daemon (Linux/amd64) ────────────────────────────────────

info "Building pulseha daemon → cmd/pulseha/bin/pulseha (linux/amd64)..."
mkdir -p "${REPO_ROOT}/cmd/pulseha/bin"
eval env GOOS=linux GOARCH=amd64 go build ${LDFLAGS} -v \
    -o "${REPO_ROOT}/cmd/pulseha/bin/pulseha" \
    "${REPO_ROOT}/cmd/pulseha"

# ── 3. Build pulsectl CLI (Linux/amd64) ──────────────────────────────────────

info "Building pulsectl CLI → cmd/pulsectl/bin/pulsectl (linux/amd64)..."
mkdir -p "${REPO_ROOT}/cmd/pulsectl/bin"
eval env GOOS=linux GOARCH=amd64 go build ${LDFLAGS} -v \
    -o "${REPO_ROOT}/cmd/pulsectl/bin/pulsectl" \
    "${REPO_ROOT}/cmd/pulsectl"

# ── 4. Generate PHP protobuf stubs ───────────────────────────────────────────

info "Generating PHP protobuf stubs → rpc/php/..."
require protoc "Install with: brew install protobuf"

mkdir -p "${PHP_OUT}"

# Locate the grpc_php_plugin. Checked in order:
#   1. PATH
#   2. GRPC_PHP_PLUGIN env var
#   3. Common install locations (pecl, composer vendor)
GRPC_PHP_PLUGIN="${GRPC_PHP_PLUGIN:-}"
if [ -z "${GRPC_PHP_PLUGIN}" ]; then
    for candidate in \
        "$(which grpc_php_plugin 2>/dev/null)" \
        "/usr/local/bin/grpc_php_plugin" \
        "/opt/homebrew/bin/grpc_php_plugin" \
        "${REPO_ROOT}/vendor/bin/grpc_php_plugin"
    do
        if [ -x "${candidate}" ]; then
            GRPC_PHP_PLUGIN="${candidate}"
            break
        fi
    done
fi

# Generate base PHP message classes (always available with protoc)
protoc \
    --proto_path="${REPO_ROOT}" \
    --php_out="${PHP_OUT}" \
    "${REPO_ROOT}/rpc/server.proto"

# Generate PHP gRPC service stubs if the plugin is available
if [ -n "${GRPC_PHP_PLUGIN}" ]; then
    info "grpc_php_plugin found at ${GRPC_PHP_PLUGIN} — generating service stubs..."
    protoc \
        --proto_path="${REPO_ROOT}" \
        --php_out="${PHP_OUT}" \
        --grpc_out="${PHP_OUT}" \
        --plugin=protoc-gen-grpc="${GRPC_PHP_PLUGIN}" \
        "${REPO_ROOT}/rpc/server.proto"
else
    echo ""
    echo "  WARNING: grpc_php_plugin not found — message classes generated but gRPC"
    echo "  service stubs were skipped. To generate service stubs, install the plugin:"
    echo ""
    echo "    # Build from source:"
    echo "    git clone -b v1.66.0 --depth 1 https://github.com/grpc/grpc"
    echo "    cd grpc && cmake . -DBUILD_SHARED_LIBS=ON && make grpc_php_plugin"
    echo "    sudo cp bins/opt/grpc_php_plugin /usr/local/bin/"
    echo ""
    echo "    # Or set GRPC_PHP_PLUGIN=/path/to/grpc_php_plugin and re-run this script."
    echo ""
fi

# ── Done ─────────────────────────────────────────────────────────────────────

info "Build complete."
echo ""
echo "  Daemon:  cmd/pulseha/bin/pulseha  (linux/amd64)"
echo "  CLI:     cmd/pulsectl/bin/pulsectl (linux/amd64)"
echo "  PHP:     rpc/php/"
