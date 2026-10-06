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

echo "[test] ok"
