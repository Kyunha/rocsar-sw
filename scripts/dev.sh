#!/usr/bin/env bash
#
# Run the OBC with simulated hardware, then follow it with the console.
#
# Two things this has to do that a bare `go run ./cmd/obc` does not:
#
#   1. Point the data root somewhere writable on this machine. rocsar.toml
#      ships root = "/mnt/rocsar_ssd", which exists on the Pi and nowhere else,
#      and storage.Check treats a missing root as fatal at startup rather than
#      creating one. The environment beats the file, so this is a one-line
#      override and needs no rocsar.local.toml. http.device is cleared at the
#      same time, since ./data is not on the aircraft's SSD.
#
#   2. Point the console at 127.0.0.1. Both defaults are the aircraft's
#      address, so `gs_cli watch` on a laptop otherwise reports a dead OBC.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

DATA_DIR="${ROCSAR_DEV_DATA:-$ROOT/data}"
mkdir -p "$DATA_DIR"

OBC_PID=""
CLI_PID=""

cleanup() {
    trap - EXIT INT TERM
    [[ -n "$CLI_PID" ]] && kill "$CLI_PID" 2>/dev/null || true
    [[ -n "$OBC_PID" ]] && kill "$OBC_PID" 2>/dev/null || true
    wait 2>/dev/null || true
}
trap cleanup EXIT INT TERM

echo "[dev] data root $DATA_DIR"
echo "[dev] starting obc (pico, camera and sdr simulated)"

# ROCSAR_HTTP_DEVICE is emptied because http.device names the aircraft's SSD and
# asserts the data root is mounted on it. ./data is a directory in the working
# tree, so there is no second filesystem to be on and the assertion would fail
# every local run. The aircraft asserts it; the bench does not.
#
# shellcheck disable=SC2046
ROCSAR_HTTP_ROOT="$DATA_DIR" ROCSAR_HTTP_DEVICE="" go run ./cmd/obc \
    --mock-pico --mock-camera --mock-sdr &
OBC_PID=$!

# The OBC binds its sockets before the first telemetry frame, but `go run`
# spends a moment compiling. Wait for the control port rather than sleeping a
# guessed interval: a fixed sleep is either too short on a cold cache or
# wasted on a warm one.
for _ in $(seq 1 100); do
    if (exec 3<>/dev/tcp/127.0.0.1/5555) 2>/dev/null; then
        break
    fi
    kill -0 "$OBC_PID" 2>/dev/null || { echo "[dev] obc exited during startup" >&2; exit 1; }
    sleep 0.1
done

echo "[dev] starting gs_cli watch (Ctrl-C stops both)"
go run ./tools/gs_cli \
    -control tcp://127.0.0.1:5555 \
    -telemetry tcp://127.0.0.1:5556 \
    -http http://127.0.0.1:5557 &
CLI_PID=$!

wait -n "$OBC_PID" "$CLI_PID"