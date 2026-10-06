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
python -m pytest firmware/tests/ -v

echo "[test] ok"
