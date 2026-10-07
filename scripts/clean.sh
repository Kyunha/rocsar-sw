#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

echo "[clean] removing build artifacts"
rm -rf bin/
rm -f obc gs gs_cli gs_probe *.test

echo "[clean] removing generated protobuf"
rm -f api/rocsar/v1/*.pb.go
rm -f firmware/*.pb.[ch]

echo "[clean] removing caches"
find . -type d -name '__pycache__' -exec rm -rf {} + 2>/dev/null || true
rm -rf .pytest_cache/ .ruff_cache/ .mypy_cache/

echo "[clean] ok"
