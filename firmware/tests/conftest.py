"""Build the firmware's own encoder/decoder so it can be driven from the host.

The policy that steers the antennas -- the heading filter, the tick
calculation, the command handling and the ST3215 wire protocol -- lives in
`gondola_model.h`, which includes no Arduino headers. That is what makes it
testable at all: a C++ translation unit can compile it, link it against the
same nanopb sources the RP2040 build uses, and be run as a plain host binary.

What this deliberately does NOT cover is the 360 lines of `firmware.ino` that
glue that policy to I2C, Serial1 and millis(). Those are checked by reading
their source, which is weak but not nothing: see the receive-buffer tests in
test_firmware_wire_format.py.
"""

from __future__ import annotations

import os
import shutil
import subprocess
from pathlib import Path

import pytest

FIRMWARE_DIR = Path(__file__).resolve().parents[1]
PROBE_SOURCE = Path(__file__).resolve().parent / "firmware_encoder_probe.cpp"

# common.pb.c is here because the unified schema moved AntennaTelemetry into
# common.proto, shared with the SDR side. Its descriptor lives in that
# translation unit, so linking only pico.pb.o fails at rocsar_v1_AntennaTelemetry_msg.
NANOPB_SOURCES = [
    "pb_common.c",
    "pb_decode.c",
    "pb_encode.c",
    "pico.pb.c",
    "common.pb.c",
    "cobs.c",
]

def _toolchain_missing() -> bool:
    return shutil.which("g++") is None or shutil.which("gcc") is None


NO_TOOLCHAIN_REASON = (
    "no C/C++ toolchain, so the firmware encoder was NOT exercised: the COBS "
    "framing and nanopb message layout shared with firmware/ would go untested"
)


def pytest_collection_modifyitems(items):
    """Skip everything if there is no compiler.

    A module-level `pytestmark` in a conftest does NOT propagate to the test
    modules, so the guard has to be a hook. Left implicit, a laptop without
    g++ collects these tests, every `probe` fixture call fails on a missing
    binary, and the suite reports failures for a condition that is really an
    absent toolchain.
    """
    if _toolchain_missing():
        skip = pytest.mark.skip(reason=NO_TOOLCHAIN_REASON)
        for item in items:
            item.add_marker(skip)


def build_probe(binary_dir: Path) -> Path:
    """Compile the probe against the nanopb sources that sit next to the sketch."""
    # The nix gcc wrapper runs in pure mode, which silently strips any -I that
    # points inside $HOME. On a laptop the repository IS in $HOME, so the -I
    # below vanishes and the compile fails with "pb.h: No such file or
    # directory" -- pointing at a file that is present, and that gcc can read
    # when named by an absolute include. Clearing this is the difference
    # between a real toolchain problem and a silent one.
    env = {**os.environ, "NIX_ENFORCE_PURITY": "0"}

    objects: list[Path] = []
    for name in NANOPB_SOURCES:
        obj = binary_dir / name.replace(".c", ".o")
        subprocess.run(
            [
                "gcc",
                "-std=gnu99",
                "-c",
                "-I",
                str(FIRMWARE_DIR),
                str(FIRMWARE_DIR / name),
                "-o",
                str(obj),
            ],
            check=True,
            env=env,
        )
        objects.append(obj)

    binary = binary_dir / "firmware_probe"
    subprocess.run(
        [
            "g++",
            "-std=gnu++17",
            "-I",
            str(FIRMWARE_DIR),
            str(PROBE_SOURCE),
            *[str(obj) for obj in objects],
            "-o",
            str(binary),
        ],
        check=True,
        env=env,
    )
    return binary


@pytest.fixture(scope="session")
def probe(tmp_path_factory) -> Path:
    return build_probe(tmp_path_factory.mktemp("firmware_probe"))


def run(probe_path: Path, *args: str) -> str:
    result = subprocess.run([str(probe_path), *args], capture_output=True, text=True)
    assert result.returncode == 0, f"probe failed: {result.stderr}"
    return result.stdout.strip()


class CobsDecodeError(ValueError):
    pass


def cobs_decode(encoded: bytes) -> bytes:
    """Undo the framing in cobs.c, so a test can read a packet the firmware built.

    This is COBS itself, the same published algorithm cobs.c implements, not a
    reimplementation of anything ours: a host encoder for the Pico command
    messages would be a second source of truth about our own schema, which is
    the thing these tests are supposed to check.
    """
    if not encoded:
        raise CobsDecodeError("empty COBS frame")

    decoded = bytearray()
    index = 0
    length = len(encoded)
    while index < length:
        code = encoded[index]
        if code == 0:
            raise CobsDecodeError("COBS frame contains a zero byte")

        index += 1
        end = index + code - 1
        if end > length:
            raise CobsDecodeError("COBS code exceeds frame length")

        decoded.extend(encoded[index:end])
        index = end
        if code != 0xFF and index < length:
            decoded.append(0)

    return bytes(decoded)