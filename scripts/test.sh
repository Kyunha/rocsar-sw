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

# The GUI builds through wails, not go build: tsc + vite + cgo all run inside
# `wails build`, and none of the gates above touch the frontend. A green suite
# with a red window is how an undefined initMap survived to a boot attempt, so
# the console builds here too. Skipped loudly -- never silently -- where the
# GUI toolchain is absent: a machine that only builds the OBC must not fail
# for lacking node, and must not pass believing it checked the window.
if command -v wails >/dev/null 2>&1; then
    echo "[test] wails build (frontend tsc + vite + cgo)"
    (cd cmd/gs && wails build)
else
    echo "[test] SKIP wails build: no wails on PATH (enter the nix shell for the full gate)"
fi

echo "[test] firmware tests"
python -m pytest firmware/tests/ -v

echo "[test] ok"
