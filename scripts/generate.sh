#!/usr/bin/env bash
#
# One schema, three generators.
#
#   Go     -> api/rocsar/v1/*.pb.go     OBC            (build time, gitignored)
#   Python -> gs/rocsar/v1/*_pb2.py     Ground Station (committed: the GS must
#                                                 install on a laptop with no
#                                                 toolchain)
#   C      -> firmware/*.pb.{c,h}       RP2040         (committed: the Arduino
#                                                 build compiles by proximity)
#
# Why the C target does not go through buf
# ----------------------------------------
# nanopb resolves an options file relative to the *current working directory*,
# not relative to protoc's include path. `pico.options` has to be found at
# `rocsar/v1/pico.options` under the CWD, so the generator must run from `api/`.
# buf always runs from the workspace root and offers no way to change that.
#
# This is not a preference. When the options file is not found, nanopb does not
# warn: it silently emits `pb_callback_t antennas;` instead of a static array,
# and the RP2040's RAM stops being budgetable at build time. Verified -- a bogus
# directive added to pico.options produced no diagnostic either. So the C target
# runs the vendored protoc from `api/` where the options file resolves, and the
# antenna array is asserted afterwards.
#
# Why firmware generation is restricted to two files
# --------------------------------------------------
# Arduino compiles every .c next to the .ino by proximity. A ground-station
# message compiled for the RP2040 would end up in the flight controller's flash.
# The file list below is explicit for that reason, and
# test/layering_test.go -> TestFirmwareExcludesGroundStationTypes greps the
# generated C for the excluded type names.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

PROTOC="${PROTOC:-$ROOT/third_party/nanopb/generator-bin/protoc}"

# The two files the flight controller is allowed to compile.
FIRMWARE_PROTOS=(rocsar/v1/common.proto rocsar/v1/pico.proto)

# Ground-station types that must never appear in the generated C.
FORBIDDEN_IN_FIRMWARE=(CommandRequest CommandResponse TelemetryFrame SdrStatus CameraStatus LinkStatus)

command -v buf >/dev/null || { echo "[gen] buf not found in PATH" >&2; exit 1; }
[[ -x "$PROTOC" ]] || { echo "[gen] vendored protoc not found at $PROTOC" >&2; exit 1; }

echo "[gen] lint"
buf lint

echo "[gen] build (the whole module must import cleanly)"
buf build -o /dev/null

# ---------------------------------------------------------------------------
# Go + Python: the whole module. buf drives both.
# ---------------------------------------------------------------------------
echo "[gen] go + python"
buf generate

# ---------------------------------------------------------------------------
# C: the flight-controller link only, from api/ so pico.options resolves.
# ---------------------------------------------------------------------------
echo "[gen] nanopb C (firmware only)"
STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT

(
    cd api
    "$PROTOC" -I . --nanopb_out="$STAGE" "${FIRMWARE_PROTOS[@]}"
)

mkdir -p firmware
# nanopb mirrors the proto directory layout under --nanopb_out. Arduino's
# proximity build needs one flat folder, so flatten and rewrite the cross-file
# include to match. A nested include would compile under protoc and fail under
# the Arduino IDE, which is the worst way to find out.
find "$STAGE" -name '*.pb.h' -o -name '*.pb.c' | while read -r f; do
    sed -e 's|#include "rocsar/v1/|#include "|' "$f" > "firmware/$(basename "$f")"
done

# The COBS framing layer is ours, not nanopb's, and is vendored in its own
# directory so its provenance is obvious. cobs_encode() does not bounds-check its
# output buffer, so its callers size the buffer from the documented worst case
# (length + length/254 + 1) -- see firmware/pico_wire.h.
for f in pb.h pb_common.h pb_common.c pb_decode.h pb_decode.c pb_encode.h pb_encode.c; do
    [[ -f "third_party/nanopb/$f" ]] || {
        echo "[gen] missing nanopb runtime file: $f" >&2; exit 1; }
done
cp third_party/nanopb/pb.h third_party/nanopb/pb_common.* \
   third_party/nanopb/pb_decode.* third_party/nanopb/pb_encode.* firmware/
cp third_party/nanopb-extra/cobs.h third_party/nanopb-extra/cobs.c firmware/

# ---------------------------------------------------------------------------
# Verify, because every one of these fails silently otherwise.
# ---------------------------------------------------------------------------
echo "[gen] verifying"

# 1. max_count applied. Without the options file this is a pb_callback_t and
#    the struct size stops being a compile-time constant.
if ! grep -q 'antennas\[2\]' firmware/pico.pb.h; then
    echo "[gen] FAIL: PicoTelemetry.antennas is not a static [2] array." >&2
    echo "[gen]      pico.options was not applied by nanopb." >&2
    grep -n 'antennas' firmware/pico.pb.h | head >&2
    exit 1
fi

# 2. No ground-station message reached the firmware.
#
#    Match the generated C symbol `rocsar_v1_<Type>`, NOT the bare proto name.
#    nanopb copies proto comments verbatim into the generated header, and these
#    protos discuss the ground-station messages in their documentation -- a
#    naive grep for "CommandResponse" hits that comment and fails the build for
#    a sentence. What matters is whether a type was *declared*, and that is what
#    the C symbol tells us.
for t in "${FORBIDDEN_IN_FIRMWARE[@]}"; do
    if grep -qE "(typedef struct _rocsar_v1_${t}\b|rocsar_v1_${t}[[:space:]]+[a-z_]+;)" \
        firmware/*.pb.h firmware/*.pb.c 2>/dev/null; then
        echo "[gen] FAIL: ground-station type rocsar.v1.$t is declared in the firmware C." >&2
        exit 1
    fi
done

# 3. The .c set is exactly what Arduino will compile. Proximity means a stray
#    .c here is a second translation unit nobody asked for.
EXPECTED_C="cobs.c pb_common.c pb_decode.c pb_encode.c common.pb.c pico.pb.c"
present="$(cd firmware && ls *.c 2>/dev/null | sort | tr '\n' ' ')"
expected="$(printf '%s\n' $EXPECTED_C | sort | tr '\n' ' ')"
if [[ "$present" != "$expected" ]]; then
    echo "[gen] FAIL: unexpected .c set in firmware/ (Arduino compiles all of them)." >&2
    echo "[gen]   present: $present" >&2
    echo "[gen]   expected: $expected" >&2
    exit 1
fi

echo "[gen] ok"
echo "[gen]   go:      $(ls api/rocsar/v1/*.pb.go 2>/dev/null | wc -l) files"
echo "[gen]   python:  $(ls gs/rocsar/v1/*_pb2.py 2>/dev/null | wc -l) files"
echo "[gen]   firmware: $(ls firmware/*.pb.[ch] 2>/dev/null | wc -l) files, $(ls firmware/*.c | wc -l) .c"