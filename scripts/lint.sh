#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

echo "[lint] go vet"
go vet ./...

# ruff and the Python Ground Station are gone: gs/ and tools/gs_probe.py were
# deleted with the Python console, so there is no Python in this repository to
# lint. The firmware tests are still Python (firmware/tests) and are run by
# scripts/test.sh; they are not linted here.

echo "[lint] buf lint"
buf lint

echo "[lint] ok"
