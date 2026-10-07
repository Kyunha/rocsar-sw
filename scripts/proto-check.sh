#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

echo "[proto-check] generating Go"
buf generate

echo "[proto-check] generating C (firmware)"
STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT

(
    cd api
    "$ROOT/third_party/nanopb/generator-bin/protoc" \
        -I . --nanopb_out="$STAGE" \
        rocsar/v1/common.proto rocsar/v1/pico.proto
)

mkdir -p firmware
find "$STAGE" -name '*.pb.h' -o -name '*.pb.c' | while read -r f; do
    sed -e 's|#include "rocsar/v1/|#include "|' \
        -e 's|#include <pb\.h>|#include "pb.h"|' \
        "$f" > "firmware/$(basename "$f")"
done

for f in pb.h pb_common.h pb_common.c pb_decode.h pb_decode.c pb_encode.h pb_encode.c; do
    [[ -f "third_party/nanopb/$f" ]] || { echo "[proto-check] missing nanopb runtime: $f" >&2; exit 1; }
done
cp third_party/nanopb/pb.h third_party/nanopb/pb_common.* \
   third_party/nanopb/pb_decode.* third_party/nanopb/pb_encode.* firmware/
cp third_party/nanopb-extra/cobs.h third_party/nanopb-extra/cobs.c firmware/

echo "[proto-check] verifying generated files match git"
git diff --exit-code -- \
    api/rocsar/v1/*.pb.go \
    firmware/*.pb.[ch] \
    firmware/pb.[ch] firmware/pb_common.[ch] firmware/pb_decode.[ch] firmware/pb_encode.[ch] \
    firmware/cobs.[ch]

echo "[proto-check] ok"
