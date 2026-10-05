#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

echo "[lint] go vet"
go vet ./...

echo "[lint] ruff (Python ground-station remnants; gone in step 8)"
if [ -e gs ] || [ -e tools/gs_probe.py ]; then
    ruff check gs/ tools/gs_probe.py
else
    echo "[lint] SKIP ruff: no Python ground station left to check"
fi

echo "[lint] buf lint"
buf lint

echo "[lint] ok"
