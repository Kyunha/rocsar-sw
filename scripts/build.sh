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

# ============================================================================
# Generate the Go protobuf bindings if they are stale
# ============================================================================
# api/rocsar/v1/*.pb.go is gitignored and generated at build time, so this script
# used to depend on someone having run scripts/generate.sh first. If it had not,
# the failure was a wall of
#
#     convert.go:35: m.GetGondolaRollDeg undefined (type *rocsarv1.PicoTelemetry
#
# naming hand-written files that are entirely innocent. The source of truth is
# api/rocsar/v1/*.proto and nothing in that message says so.
#
# So: regenerate when a .proto is newer than its .pb.go, and refuse clearly when
# that cannot be done. Only the Go target is touched -- the nanopb C under
# firmware/ and the Python under gs/ are committed, which is why generate.sh's
# header explains that asymmetry.
proto_dir="api/rocsar/v1"

needs_generate() {
    local pb proto base
    for pb in "$proto_dir"/*.pb.go; do
        [[ -e "$pb" ]] || return 0
        base="$(basename "$pb" .pb.go)"
        for proto in "$proto_dir"/*.proto; do
            [[ "$(basename "$proto" .proto)" == "$base" ]] || continue
            if [[ "$proto" -nt "$pb" ]]; then
                return 0
            fi
        done
    done
    return 1
}

ensure_generated() {
    if ! needs_generate; then
        return 0
    fi

    if ! command -v buf >/dev/null 2>&1; then
        echo "[build] FAIL: the Go protobuf bindings are stale and buf is not installed." >&2
        echo "[build]      a file under $proto_dir/*.proto is newer than its .pb.go," >&2
        echo "[build]      and $proto_dir/*.pb.go is gitignored (generated at build" >&2
        echo "[build]      time). Install buf and re-run this script, or run:" >&2
        echo "[build]          ./scripts/generate.sh" >&2
        exit 1
    fi

    echo "[build] generating Go protobuf bindings (a .proto is newer than its .pb.go)"
    buf lint
    buf generate
}

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
        return 1
    fi

    echo "[build] building the Ground Station frontend"
    ( cd "$frontend" && npm install --silent && npm run build )
}

ensure_generated

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