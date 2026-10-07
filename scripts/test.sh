#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

echo "[test] go build"
go build ./...

echo "[test] go vet"
go vet ./...

echo "[test] go test"
go test ./...

# The GUI frontend builds with npm, not go build: tsc + vite run inside
# `npm run build`, and none of the gates above touch the frontend. A green
# suite with a red window is how an undefined initMap survived to a boot
# attempt, so the console frontend builds here too. Skipped loudly --
# never silently -- where node is absent: a machine that only builds the OBC
# must not fail for lacking node, and must not pass believing it checked the
# window.
if command -v npm >/dev/null 2>&1; then
    echo "[test] npm run build (frontend tsc + vite)"
    (cd cmd/gs/frontend && npm run build)
    # The instruments carry the arithmetic that absence-forbids-a-zero and
    # held-is-not-a-measurement rules turn into, so it is tested rather than
    # eyeballed. Node strips the types itself; no test framework.
    echo "[test] npm test (frontend widgets)"
    (cd cmd/gs/frontend && npm test)
else
    echo "[test] SKIP npm run build: no npm on PATH (enter the nix shell for the full gate)"
fi

echo "[test] firmware tests"
# python3, not python: the nix shell provides python3 and nothing called `python`,
# so this step failed on a clean shell with "command not found" -- which, under
# `set -e`, aborted the gate before it could report anything useful.
#
# pytest is the other half of the same trap. shell.nix did not list it, so a fresh
# `nix-shell` has an interpreter with no pytest and this step failed again with
# "No module named pytest". pytest is now in shell.nix; this check is the loud
# skip so a machine that cannot install it fails visibly rather than quietly
# skipping the only tests that cover the flight controller's safety policy.
if python3 -c 'import pytest' 2>/dev/null; then
    python3 -m pytest firmware/tests/ -v
elif command -v pytest >/dev/null 2>&1; then
    pytest firmware/tests/ -v
else
    echo "[test] FAIL: firmware tests did NOT run -- no pytest for python3." >&2
    echo "[test]      The COBS framing, the nanopb message layout and the" >&2
    echo "[test]      heading/zeroing policy shared with firmware/ went unchecked." >&2
    echo "[test]      Install it, or add pytest to shell.nix." >&2
    exit 1
fi

# The sketch itself.
#
# firmware/tests/ compiles gondola_model.h and calibration.h on the host, so the
# model is covered -- but nothing in that suite compiles firmware.ino. The
# source-shape assertions read it as text, and text reads fine through a missing
# brace: firmware.ino was once left with a stray `}` and a truncated function
# signature, and 176 tests passed while the sketch did not compile for the board
# it runs on. This step is the only thing that would have caught it.
#
# Same rule as npm above: skipped loudly, never silently, and a failure to compile
# is a failure of the gate rather than a warning.
if command -v arduino-cli >/dev/null 2>&1; then
    echo "[test] firmware sketch compiles for the Pico"
    OUT="$(mktemp -d)"
    trap 'rm -rf "$OUT"' EXIT
    # Matches scripts/flash-firmware.sh. Without this the sketch is never built
    # for the board it runs on, which is the only target that matters.
    if ! arduino-cli compile --fqbn rp2040:rp2040:rpipicow --output-dir "$OUT" firmware; then
        echo "[test] FAIL: firmware.ino does not compile for rp2040:rp2040:rpipicow" >&2
        exit 1
    fi
else
    echo "[test] SKIP firmware sketch compile: no arduino-cli on PATH."
    echo "[test]      firmware.ino is NOT checked by the host suite -- it is text-asserted"
    echo "[test]      only, and text reads fine through a syntax error. Enter the nix"
    echo "[test]      shell with arduino-cli to close this gap."
fi

echo "[test] ok"
