"""The command and kinematics surface: what the firmware does with a command.

Six of the twenty-four tests in the original file.

The other eighteen had one root dependency: building or reading a protobuf
frame on the host. Fifteen of them built their input with the old Python
`pico_codec.encode_*` wrapped by `_cobs_encode` from
`controller.hardware.serial_pico`, and three read a telemetry frame back with
`pico_codec.decode_pico_message`. None of that exists here.

They are deliberately not reimplemented. Testing the firmware's decoder
against a Python codec written for the purpose would be a weaker check than
the one worth having -- pairing the encoder we actually ship, `internal/pico`,
against this same C decoder. That test would also cover the five
cross-language wire-format cases dropped from test_firmware_wire_format.py,
which share the dependency. It is outstanding work, and none of it is claimed
here.
"""

from __future__ import annotations

import re
import subprocess
from pathlib import Path

from conftest import FIRMWARE_DIR

SKETCH = FIRMWARE_DIR / "firmware.ino"
MODEL = FIRMWARE_DIR / "gondola_model.h"

#: The flight servo ids, mirroring ANTENNA_0_SERVO_ID/ANTENNA_1_SERVO_ID in
#: firmware/gondola_model.h. They are named rather than written inline because
#: they are load-bearing: applyCommand rejects any other id, so a hardcoded `5`
#: silently turns a behavioural test into a rejection test the moment the
#: firmware default changes.
ANTENNA_0 = 1
ANTENNA_1 = 2

def _servo_packet(probe: Path, servo_id: int, tick: int) -> bytes:
    result = subprocess.run(
        [str(probe), "--servo-packet", str(servo_id), str(tick)],
        capture_output=True,
        text=True,
    )
    assert result.returncode == 0, result.stderr
    return bytes.fromhex(result.stdout.strip())


def test_the_model_header_is_the_single_source_of_command_logic():
    """The sketch must delegate, or the tested copy is not the shipped one.

    This is the structural half of the file: it cannot check *what* the model
    computes (the tests above do that), only that the sketch calls into it rather
    than keeping its own copy. A duplicate copy is worse than no test, because it
    looks covered.
    """
    ino = SKETCH.read_text()
    assert '#include "gondola_model.h"' in ino
    assert "applyCommand(gondola, cmd)" in ino
    # No hand-rolled per-command switch may remain in the sketch.
    assert "CommandMessage_jog_tag" not in ino
    assert "CommandMessage_heater_tag" not in ino


def test_the_servo_packet_is_the_documented_sts3215_write(probe):
    """13 bytes: header, id, length, control, register 42, tick, checksum.

    These were inline in the .ino, so nothing but an eyeball ever checked them.
    A checksum missing one byte, or a big-endian position, is a servo that
    silently does not move -- the failure mode this whole test file exists for.
    """
    packet = _servo_packet(probe, ANTENNA_1, 3000)

    assert len(packet) == 13
    assert packet[0:2] == b"\xff\xff"
    assert packet[2] == ANTENNA_1
    assert packet[3] == 0x09  # packet length
    assert packet[4] == 0x03  # control: write
    assert packet[5] == 42  # STS goal position register
    assert packet[6:8] == (3000).to_bytes(2, "little")
    assert packet[8:12] == b"\x00\x00\x00\x00"


def test_the_servo_checksum_is_the_ones_complement_of_bytes_2_through_11(probe):
    for servo_id, tick in ((1, 0), (2, 3000), (3, 4095), (10, 1)):
        packet = _servo_packet(probe, servo_id, tick)
        checksum = (~sum(packet[2:12])) & 0xFF
        assert packet[12] == checksum, f"bad checksum for id={servo_id} tick={tick}"


def test_an_out_of_range_tick_is_clamped_on_the_wire(probe):
    """The servo is 12-bit; a larger command would be truncated by the bus."""
    assert _servo_packet(probe, 1, -5)[6:8] == (0).to_bytes(2, "little")
    assert _servo_packet(probe, 1, 99999)[6:8] == (4095).to_bytes(2, "little")


def test_the_sketch_writes_the_packet_the_model_built(probe):
    """The extraction must not have left the real frame in the sketch."""
    ino = SKETCH.read_text()
    assert "buildServoPacket(antenna.id, targetTick" in ino or (
        "buildServoPacket(id, position, pkt, sizeof(pkt))" in ino
    )
    # The checksum and the 0xFF header belong to the model now.
    assert "~sum" not in ino
    assert "shouldSendServoTick(antenna.lastSentTick, targetTick)" in ino


def test_num_antennas_matches_the_protobuf_max_count():
    """The two numbers that must agree live in files nothing cross-checks.

    `NUM_ANTENNAS` is a macro in the firmware; the array it indexes was sized by
    `antennas max_count:2` in api/rocsar/v1/pico.options, which no build step reads.
    A NUM_ANTENNAS above max_count writes past the end of the telemetry struct;
    the C static_assert in gondola_model.h catches that direction at build time,
    and this catches the other (a raised max_count leaving an antenna silently
    unreported).
    """
    options = (FIRMWARE_DIR.parent / "api" / "rocsar" / "v1" / "pico.options").read_text()
    declared = re.search(r"PicoTelemetry\.antennas max_count:(\d+)", options)
    assert declared, f"api/rocsar/v1/pico.options no longer declares a max_count: {options!r}"

    header = (FIRMWARE_DIR / "gondola_model.h").read_text()
    macro = re.search(r"#define NUM_ANTENNAS (\d+)", header)
    assert macro, "gondola_model.h no longer defines NUM_ANTENNAS"

    assert int(macro.group(1)) == int(declared.group(1))
