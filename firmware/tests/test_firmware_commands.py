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

import pytest
from conftest import FIRMWARE_DIR, run

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
        check=False,
    )
    assert result.returncode == 0, result.stderr
    return bytes.fromhex(result.stdout.strip())


def _fields(blob: str) -> dict[str, object]:
    """`a=1 b=2.5` -> {"a": 1, "b": 2.5}; integers stay integers."""
    return {
        key: float(value) if "." in value else int(value)
        for key, value in re.findall(r"(\w+)=([-\d.]+)", blob)
    }


def _apply_synthetic(probe: Path, *args: str) -> dict[str, object]:
    """Run one command through the firmware's real applyCommand().

    Built by the probe rather than decoded from a frame, because this repo has
    no host-side command encoder yet (see this file's docstring) -- and because
    the cases worth testing here are the ones a hex frame cannot express
    readably, like a NaN heading.
    """
    out = run(probe, "--apply-synth", *args)
    lines = out.splitlines()
    reply = next(line for line in lines if line.startswith("REPLY ")).split()
    return {
        "code": int(reply[2]),
        "name": reply[3],
        "state": _state_from(lines),
        "drive": _drive_from(lines),
    }


def _state_from(lines: list[str]) -> dict[str, object]:
    body = next(line for line in lines if line.startswith("STATE "))[len("STATE ") :]
    parsed: dict[str, object] = {"header": _fields(body.split("|")[0])}
    parsed["axes"] = {
        match.group(1): _fields(match.group(2))
        for match in re.finditer(r"a(\d) ([^|]+)", body)
    }
    return parsed


def _drive_from(lines: list[str]) -> dict[str, int]:
    line = next(line for line in lines if line.startswith("DRIVE "))
    return {k: int(v) for k, v in re.findall(r"a(\d)=(\d)", line)}


def _boot(probe: Path) -> dict[str, object]:
    """The power-up posture: state after one (hardware-less) control tick."""
    lines = run(probe, "--boot-state").splitlines()
    return {"state": _state_from(lines), "drive": _drive_from(lines)}


def test_the_model_header_is_the_single_source_of_command_logic():
    """The sketch must delegate, or the tested copy is not the shipped one.

    This is the structural half of the file: it cannot check *what* the model
    computes (the tests above do that), only that the sketch calls into it rather
    than keeping its own copy. A duplicate copy is worse than no test, because it
    looks covered.
    """
    ino = SKETCH.read_text()
    assert '#include "gondola_model.h"' in ino
    assert "applyCommand(gondola, cmd, millis())" in ino, (
        "the sketch must route commands through the model, and pass the clock: "
        "the heater dead-man stamps the time the command arrived"
    )
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
    assert "shouldDriveServo(antenna, targetTick)" in ino, (
        "the transmit decision -- boot interlock plus deadband -- must be the "
        "model's; a sketch-local condition would be a copy no test reaches"
    )


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


# ---------------------------------------------------------------------------
# The boot interlock
# ---------------------------------------------------------------------------


class TestBootHold:
    """Power-up must be silent until someone asks for motion.

    Before this, the firmware booted straight into auto mode with
    target_heading_deg=0. With the mount offset at 270 degrees and a 5:1 gear,
    that target is reachable only for gondola headings in a 72-degree window --
    so roughly four boots in five drove an axis to the clamp at the rail within
    one control tick of power-up, and telemetry's only hint was a tick pinned
    at 4095 with no alarm behind it.
    """

    def test_power_up_holds_both_axes_and_transmits_nothing(self, probe):
        """The posture and its consequence, from one run.

        DRIVE is the assertion that matters: manual_mode says what the firmware
        believes it is doing, DRIVE says whether a byte reaches the bus, and the
        two agree only because the interlock is in the transmit path and not
        merely in the telemetry.
        """
        boot = _boot(probe)
        header = boot["state"]["header"]
        assert header["h1"] == 0 and header["h2"] == 0, "heaters boot off"

        for index in "01":
            axis = boot["state"]["axes"][index]
            assert axis["manual"] == 1, "held in manual mode, not tracking a target"
            assert axis["manualTick"] == 2048, "held at centre"
            assert axis["await"] == 1, "the interlock is set"
            assert axis["tick"] == 2048, (
                "the never-sent sentinel must not reach the wire as 65535 ticks "
                "(~5580 degrees); the fallback reports the centre it holds"
            )
            assert axis["fb_state"] == 0, "FEEDBACK_UNKNOWN: nothing has been read"
        assert boot["drive"] == {"0": 0, "1": 0}, "no axis may transmit at boot"

    def test_a_target_releases_both_axes(self, probe):
        """set_target is the expressed intent the interlock waits for."""
        result = _apply_synthetic(probe, "set_target", "90")
        assert result["name"] == "ERROR_NONE"
        assert result["state"]["header"]["target"] == pytest.approx(90.0)
        for index in "01":
            axis = result["state"]["axes"][index]
            assert axis["manual"] == 0, "a target puts the axis back in auto mode"
            assert axis["await"] == 0
        assert result["drive"] == {"0": 1, "1": 1}, (
            "the first commanded position must be transmitted even though "
            "lastSentTick has never been written"
        )

    def test_a_jog_releases_only_the_axis_it_names(self, probe):
        """One axis being commanded must not start the other one moving."""
        result = _apply_synthetic(probe, "jog", str(ANTENNA_0), "3000")
        assert result["name"] == "ERROR_NONE"
        first, second = result["state"]["axes"]["0"], result["state"]["axes"]["1"]
        assert first["manual"] == 1 and first["manualTick"] == 3000
        assert first["await"] == 0
        assert second["await"] == 1, "the uncommanded axis stays held"
        assert result["drive"] == {"0": 1, "1": 0}

    @pytest.mark.parametrize(
        "command",
        [
            ("stop", "1"),
            ("zero", "1"),
            ("mount", "1", "90"),
            ("dir", "1", "-1"),
            ("heater", "1", "1"),
        ],
    )
    def test_a_command_that_moves_nothing_does_not_release_the_interlock(
        self, probe, command
    ):
        """stop, zero, mount, dir and heater are postures and configuration.

        None of them asks a servo to go anywhere, so none of them may be able to
        start motion on an axis that has never been commanded -- which is what
        releasing the interlock would do, because lastSentTick=0xFFFF differs
        from every possible target by more than the deadband.
        """
        result = _apply_synthetic(probe, *command)
        assert result["name"] == "ERROR_NONE"
        assert result["state"]["axes"]["0"]["await"] == 1
        assert result["drive"] == {"0": 0, "1": 0}

    def test_the_first_target_is_sent_even_when_it_computes_to_centre(self, probe):
        """The interlock must not swallow a command that matches the belief.

        Heading 0 with a 270-degree mount offset gives zero antenna-relative
        angle, so this target computes to exactly centre -- the position the
        model already claims. lastSentTick is still the never-sent sentinel, so
        the send must happen anyway: the servo's real position at boot is
        unknown, and a first command that happens to agree with a guess is
        still the only chance to put it on the bus.
        """
        result = _apply_synthetic(probe, "set_target", "270")
        assert result["name"] == "ERROR_NONE"
        assert result["drive"] == {"0": 1, "1": 1}


# ---------------------------------------------------------------------------
# Commands whose payload is not a number
# ---------------------------------------------------------------------------


class TestRejectingNonFiniteCommands:
    """A NaN off the wire must be refused where it enters, not tolerated.

    Two seams: wrap360(NaN) is NaN, so a non-finite target or mount offset
    would be latched as live geometry and every calculateTargetTick() after it
    would take the non-finite branch and centre -- the command would report
    success and quietly do nothing. And the final step of the tick arithmetic
    is an (int32_t) cast, which is undefined behaviour for NaN on this target.
    """

    @pytest.mark.parametrize("value", ["nan", "inf", "-inf"])
    def test_a_non_finite_target_is_refused_and_changes_nothing(self, probe, value):
        result = _apply_synthetic(probe, "set_target", value)
        assert result["name"] == "ERROR_INVALID_PARAMETER"
        header = result["state"]["header"]
        assert header["target"] == 0.0, "a refused command must not latch a heading"
        for index in "01":
            assert result["state"]["axes"][index]["await"] == 1, (
                "a refused target is not an expressed intent: the interlock "
                "stays set"
            )

    @pytest.mark.parametrize("value", ["nan", "inf"])
    def test_a_non_finite_mount_offset_is_refused(self, probe, value):
        result = _apply_synthetic(probe, "mount", str(ANTENNA_0), value)
        assert result["name"] == "ERROR_INVALID_PARAMETER"
        assert result["state"]["axes"]["0"]["offset"] == pytest.approx(270.0)

    @pytest.mark.parametrize("value", ["nan", "inf"])
    def test_a_non_finite_direction_multiplier_is_refused(self, probe, value):
        """This case always rejected NaN; pin it, because it is the same seam."""
        result = _apply_synthetic(probe, "dir", str(ANTENNA_0), value)
        assert result["name"] == "ERROR_INVALID_PARAMETER"
        assert result["state"]["axes"]["0"]["dir"] == pytest.approx(-1.0)

    def test_a_real_target_and_mount_are_still_accepted(self, probe):
        """The guards must not swallow legitimate commands."""
        target = _apply_synthetic(probe, "set_target", "90")
        assert target["name"] == "ERROR_NONE"
        mount = _apply_synthetic(probe, "mount", str(ANTENNA_0), "12.5")
        assert mount["name"] == "ERROR_NONE"
        assert mount["state"]["axes"]["0"]["offset"] == pytest.approx(12.5)
