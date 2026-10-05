#!/usr/bin/env bash
#
# Build every binary into bin/.
#
#   ./scripts/build.sh              development build
#   ./scripts/build.sh --release    stripped, for deployment
#
# `gs` is the odd one out. cmd/gs/main.go carries
# `//go:embed all:frontend/dist`, and an embed pattern that matches nothing is a
# compile error -- so a fresh checkout cannot build the Ground Station until the
# frontend has been built once. The script does that for you when npm is
# available and skips `gs` with a message when it is not, because the OBC, the
# console and every bench tool build without a Node toolchain and a broken
# `gs` should not block work on them.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

RELEASE=false
case "${1:-}" in
    --release) RELEASE=true ;;
    "") ;;
    *) echo "usage: $0 [--release]" >&2; exit 1 ;;
esac

LDFLAGS="-s -w"

build() {
    local out="$1" pkg="$2"
    if $RELEASE; then
        go build -trimpath -ldflags "$LDFLAGS" -o "bin/$out" "$pkg"
    else
        go build -o "bin/$out" "$pkg"
    fi
    printf '[build] %-14s %s\n' "$out" "$pkg"
}

build_frontend() {
    local frontend="$ROOT/cmd/gs/frontend"

    if [[ -d "$frontend/dist" ]] && [[ -n "$(ls -A "$frontend/dist" 2>/dev/null)" ]]; then
        return 0
    fi

    if ! command -v npm >/dev/null 2>&1; then
        echo "[build] skipping gs: cmd/gs/frontend/dist does not exist and npm is not installed."
        echo "[build]          install Node, then: (cd cmd/gs/frontend && npm install && npm run build)"
        echo "[build]          or build it with: wails build -s ./cmd/gs"
        return 1
    fi

    echo "[build] building the Ground Station frontend"
    ( cd "$frontend" && npm install --silent && npm run build )
}

mkdir -p bin

build obc         ./cmd/obc
build gs_cli      ./tools/gs_cli
build gs_probe    ./tools/gs_probe
build camera_bench ./tools/camera_bench
build gnss_bench  ./tools/gnss_bench
build pico_bench  ./tools/pico_bench
build sdr_bench   ./tools/sdr_bench

# Last, and non-fatal: it is the only target with an external build step.
if build_frontend; then
    build gs ./cmd/gs
fi

echo "[build] ok -> $ROOT/bin"