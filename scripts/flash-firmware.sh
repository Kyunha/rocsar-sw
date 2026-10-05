#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

PORT="${ARDUINO_PORT:-}"

if [[ -z "$PORT" ]]; then
    PORT="$(arduino-cli board list 2>/dev/null | awk '/tty|usbmodem|usbdev/{print $1; exit}')"
fi

if [[ -z "$PORT" ]]; then
    echo "[flash] no RP2040 serial port found" >&2
    echo "[flash] set ARDUINO_PORT or connect the board" >&2
    exit 1
fi

echo "[flash] compiling firmware for rp2040"
arduino-cli compile --fqbn arduino:mbed_rp2040:pico firmware/firmware.ino

echo "[flash] uploading to ${PORT}"
arduino-cli upload --fqbn arduino:mbed_rp2040:pico --port "$PORT" firmware/firmware.ino

echo "[flash] ok"
