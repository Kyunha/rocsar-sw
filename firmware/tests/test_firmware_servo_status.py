"""Firmware tests for the ST3215 status read: request, parser, and the fold.

This is P6's other half. `tests/test_firmware_commands.py` covers what the
firmware *writes* to the servo bus -- the goal-position packet, the deadband, the
tick arithmetic. This file covers what it *reads back*, which is the direction
that makes `current_tick`, `load` and `temperature_c` measurements instead of
echoes and constants.

The receive half did not exist in any file or language before this: `Serial1` was
only ever `write`/`flush`/`setTX`/`setRX`/`begin`, so byte accumulation,
resynchronisation, checksum verification and per-servo correlation all had to be
written. It was first written inside the `.ino`, where the only way to test any of
it was to flash a board and read a serial console -- so this whole file is also
the argument for `scanServoStatus()` living in `gondola_model.h`.

The parser is driven with byte sequences built here rather than captured from a
servo, because the failure modes that matter are the ones a bench produces
*by accident*: the bus echoing the Pico's own request back at it, the other
servo's reply interleaved on a shared conductor, a frame truncated by the read
window, line noise before the header.
"""

from __future__ import annotations

import re
import subprocess
from pathlib import Path

import pytest
from conftest import FIRMWARE_DIR

# ---------------------------------------------------------------------------
# Building bus bytes, the way a servo would
# ---------------------------------------------------------------------------

#: Header, id, length, error, position, speed, load, voltage, temperature, checksum.
REPLY_LEN = 14
STATUS_LENGTH_FIELD = 0x0A
REQUEST_LENGTH_FIELD = 0x04
#: The two header bytes plus the LEN byte, which LEN does not count.
FRAME_OVERHEAD = 4


def _checksum(body: bytes) -> int:
    """The ST3215 checksum: ones' complement of the bytes between header and check.

    The two 0xFF header bytes are excluded. Including them yields a checksum that
    validates nothing, which is why it is written once here rather than inline in
    each test.
    """
    return (~sum(body[2:-1])) & 0xFF


def _reply(
    servo_id: int,
    *,
    tick: int = 0,
    speed: int = 0,
    load: int = 0,
    voltage: int = 0,
    temperature: int = 0,
    error: int = 0,
) -> str:
    """A well-formed 14-byte status reply, as hex."""
    body = [
        0xFF,
        0xFF,
        servo_id,
        STATUS_LENGTH_FIELD,
        error,
        tick & 0xFF,
        (tick >> 8) & 0xFF,
        speed & 0xFF,
        (speed >> 8) & 0xFF,
        load & 0xFF,
        (load >> 8) & 0xFF,
        voltage,
        temperature,
    ]
    # The checksum covers the bytes between the header and itself, which in the
    # complete frame is bytes 2..12. It is computed over a placeholder-padded
    # array because the padding *is* the checksum slot: summing [2:-1] of a
    # 13-byte array would drop the temperature byte from the sum and produce a
    # checksum that the firmware correctly rejects.
    body.append(_checksum(bytes(body + [0])))
    assert len(body) == REPLY_LEN
    return bytes(body).hex()


def _request(probe: Path, servo_id: int) -> str:
    """The request the sketch would put on the bus, as hex, from the firmware itself."""
    result = subprocess.run(
        [str(probe), "--servo-status-request", str(servo_id)],
        capture_output=True,
        text=True,
        check=False,
    )
    assert result.returncode == 0, result.stderr
    return result.stdout.strip()


def _scan(probe: Path, wire: str, servo_id: int) -> dict[str, object]:
    result = subprocess.run(
        [str(probe), "--servo-status-scan", wire, str(servo_id)],
        capture_output=True,
        text=True,
        check=False,
    )
    assert result.returncode == 0, result.stderr
    fields: dict[str, object] = {}
    for token in result.stdout.split():
        key, sep, value = token.partition("=")
        if not sep:
            continue  # the leading "SCAN" marker carries no value
        fields[key] = float(value) if "." in value else int(value)
    return fields


# ---------------------------------------------------------------------------
# The request
# ---------------------------------------------------------------------------


class TestTheStatusRequest:
    def test_it_reads_the_telemetry_register(self, probe: Path) -> None:
        wire = _request(probe, 1)
        # Byte offsets: 0-1 header, 2 id, 3 LEN, 4 instruction, 5 register,
        # 6 parameter length, 7 checksum. Two hex characters per byte.
        assert wire[0:4] == "ffff"
        assert wire[4:6] == "01", "the servo being addressed"
        assert wire[6:8] == f"{REQUEST_LENGTH_FIELD:02x}"
        assert wire[8:10] == "02", "instruction must be READ"
        assert wire[10:12] == "38", "start register must be 0x38"
        assert wire[12:14] == "08", "must ask for the 8-byte status block"

    def test_its_length_field_matches_its_own_length(self, probe: Path) -> None:
        for servo_id in (0, 1, 2, 253):
            assert len(_request(probe, servo_id)) // 2 == REQUEST_LENGTH_FIELD + FRAME_OVERHEAD

    def test_its_checksum_covers_the_body_and_excludes_the_header(
        self, probe: Path
    ) -> None:
        raw = bytes.fromhex(_request(probe, 3))
        assert raw[-1] == _checksum(raw)

    def test_a_buffer_too_small_is_refused_rather_than_overflowed(
        self, probe: Path
    ) -> None:
        """The write path has this guard and a test; the read path must too.

        `buildServoPacket` returning 0 on a short buffer is the same decision, and
        it is checked here because a builder that wrote past its buffer would be
        memory corruption on the servo bus rather than a compile error.
        """
        source = (FIRMWARE_DIR / "gondola_model.h").read_text()
        assert "outLen < SERVO_STATUS_REQUEST_LEN" in source


# ---------------------------------------------------------------------------
# The parser
# ---------------------------------------------------------------------------


class TestTheParserAccepts:
    def test_a_bare_status_reply(self, probe: Path) -> None:
        result = _scan(probe, _reply(1, tick=3000, load=420, temperature=31), 1)
        assert result["matched"] == 1
        assert result["tick"] == 3000
        assert result["load"] == pytest.approx(42.0)
        assert result["temp"] == 31

    def test_the_echo_of_our_own_request_is_skipped_structurally(
        self, probe: Path
    ) -> None:
        """The single most important property of this parser.

        TX and RX are the same conductor on this bus, so every request comes back
        at us as an 8-byte echo with LEN=0x04. The sketch used to handle that by
        draining exactly 8 bytes, which works only as long as the echo is always
        present and always that length -- and if either is false it silently eats
        the first 8 bytes of a real reply and reports a working servo as dead.
        The scanner cannot make that mistake because the length byte tells it
        what a frame is before it counts anything.
        """
        wire = _request(probe, 1) + _reply(1, tick=777)
        result = _scan(probe, wire, 1)
        assert result["matched"] == 1
        assert result["tick"] == 777
        assert result["consumed"] == 8 + REPLY_LEN

    def test_a_reply_that_follows_several_undelivered_echoes(
        self, probe: Path
    ) -> None:
        """Echoes accumulate while a servo is slow; they must not blind it."""
        wire = _request(probe, 1) * 4 + _reply(1, tick=1234)
        result = _scan(probe, wire, 1)
        assert result["matched"] == 1
        assert result["tick"] == 1234

    def test_the_other_antennas_reply_is_skipped_not_treated_as_ours(
        self, probe: Path
    ) -> None:
        """Both axes share one bus, so their replies interleave."""
        wire = _reply(2, tick=900) + _reply(1, tick=3000)
        result = _scan(probe, wire, 1)
        assert result["matched"] == 1
        assert result["tick"] == 3000
        assert result["consumed"] == 2 * REPLY_LEN

    def test_noise_before_the_header(self, probe: Path) -> None:
        result = _scan(probe, "deadbeef00" + _reply(1, tick=42), 1)
        assert result["matched"] == 1
        assert result["tick"] == 42

    def test_the_error_byte_is_reported_rather_than_discarded(
        self, probe: Path
    ) -> None:
        """The byte the reference sketches parsed straight past.

        A checksum-valid frame with a non-zero error is the servo saying "I am
        talking and I am in trouble". The reference sketch at
        `docs/ST3215_Configure` reads this frame and discards index 4, so an
        overheat or an over-voltage read as a clean status.

        (It used to be cited as `tools/ST3215_Configure`, a path that is not in
        the tree, alongside a `tools/servo_tester` that never was.)
        """
        result = _scan(probe, _reply(1, tick=3000, error=3), 1)
        assert result["matched"] == 1
        assert result["err"] == 3

    def test_a_negative_load_keeps_its_sign(self, probe: Path) -> None:
        """The register is signed; a load against the drive is not zero."""
        result = _scan(probe, _reply(1, load=-250), 1)
        assert result["load"] == pytest.approx(-25.0)


class TestTheParserRefuses:
    def test_a_reply_for_another_servo(self, probe: Path) -> None:
        result = _scan(probe, _reply(2, tick=900), 1)
        assert result["matched"] == 0

    def test_a_corrupted_checksum(self, probe: Path) -> None:
        good = _reply(1, tick=3000)
        result = _scan(probe, good[:-2] + "ff", 1)
        assert result["matched"] == 0

    def test_a_truncated_reply_which_is_kept_rather_than_guessed(
        self, probe: Path
    ) -> None:
        """A half-arrived frame must be held, not parsed from nine bytes."""
        result = _scan(probe, _reply(1, tick=3000)[:20], 1)
        assert result["matched"] == 0
        assert result["consumed"] == 0, "a partial frame must stay in the buffer"

    def test_an_echo_with_no_reply(self, probe: Path) -> None:
        assert _scan(probe, _request(probe, 1), 1)["matched"] == 0

    def test_an_echo_is_drained_even_when_no_reply_follows(
        self, probe: Path
    ) -> None:
        """The wedge this parser had once, and the reason for reading LEN first.

        Checking "have I 14 bytes yet?" before reading the length byte treats a
        complete 8-byte echo as a half-arrived status frame and holds it forever.
        A servo that has gone offline then accumulates one undrainable echo per
        poll until the buffer is full and the RX FIFO backs up behind it. The
        symptom is not a wrong reading; it is a bus that stops answering for good.
        """
        result = _scan(probe, _request(probe, 1) * 5, 1)
        assert result["matched"] == 0
        assert result["consumed"] == 5 * (len(_request(probe, 1)) // 2), (
            "every echoed request must be consumed, or the buffer wedges"
        )

    def test_a_length_byte_beyond_any_real_frame_resyncs(self, probe: Path) -> None:
        """Corruption, not a slow arrival -- so step, do not wait forever."""
        result = _scan(probe, "ffff01ff00", 1)
        assert result["matched"] == 0
        assert result["consumed"] > 0


# ---------------------------------------------------------------------------
# The fold into the axis an operator reads
# ---------------------------------------------------------------------------


def _states(stdout: str) -> list[dict]:
    """Every STATE line, parsed as {axis_index: {field: value}}.

    Shared by the --apply-feedback and --boot-state entry points: they print
    the same line, and a second parser for it would be a second opinion about
    a format that only exists so these tests can read it.
    """
    states = []
    for line in stdout.splitlines():
        if not line.startswith("STATE "):
            continue
        body = line[len("STATE ") :]
        axes = dict(re.findall(r"a(\d) ([^\|]+)", body))
        states.append(
            {
                index: {
                    k: (float(v) if "." in v else int(v))
                    for k, v in re.findall(r"(\w+)=([-\d.]+)", blob)
                }
                for index, blob in axes.items()
            }
        )
    return states


def _feedback_steps(probe: Path, steps: list[tuple[str, int]]) -> list[dict]:
    args: list[str] = []
    for wire, servo_id in steps:
        args += [wire, str(servo_id)]
    result = subprocess.run(
        [str(probe), "--apply-feedback", *args], capture_output=True, text=True, check=False
    )
    assert result.returncode == 0, result.stderr
    return _states(result.stdout)


def _boot_axes(probe: Path) -> dict[str, dict]:
    """The power-up axes, after one hardware-less control tick."""
    result = subprocess.run(
        [str(probe), "--boot-state"], capture_output=True, text=True, check=False
    )
    assert result.returncode == 0, result.stderr
    states = _states(result.stdout)
    assert len(states) == 1, "the boot state is one tick, not a sequence"
    return states[0]


class TestFoldingFeedbackIntoTheAxis:
    def test_before_any_read_the_axis_falls_back_to_the_command(
        self, probe: Path
    ) -> None:
        """The pre-P6 behaviour, kept as a fallback rather than removed.

        It is kept because it is the only thing an operator can see before the
        first status read lands, and because it costs one branch. It must never
        run once a real reading exists -- that is the next test.
        """
        states = _feedback_steps(probe, [(_reply(1, tick=3000), 1)])
        first = states[0]["0"]
        assert first["fb_state"] == 0
        assert first["tick"] == 1000, "the fallback reports the commanded tick"
        assert first["load"] == 0

    def test_before_any_command_the_fallback_reports_the_centre(
        self, probe: Path
    ) -> None:
        """The never-sent sentinel must not reach the wire as a position.

        lastSentTick is 0xFFFF until something is transmitted, and 0xFFFF is
        65535 -- about 5580 degrees off centre, more than fourteen full turns
        of a servo that stops at 4095. With feedback_state=UNKNOWN beside it a
        careful reader might notice; nothing else on the wire says so, and the
        GUI reads these numbers as numbers. The axis that has never been
        commanded holds at centre, so centre is what the fallback reports.
        """
        axes = _boot_axes(probe)
        for index in ("0", "1"):
            assert axes[index]["tick"] == 2048, (
                "the sentinel must be reported as the centre it stands for"
            )
            assert axes[index]["fb_state"] == 0, (
                "and still as UNKNOWN: nothing has been measured yet"
            )

    def test_a_measured_tick_replaces_the_command_echo(
        self, probe: Path
    ) -> None:
        """This is P6. The wire now says where the encoder is, not where we asked."""
        states = _feedback_steps(
            probe, [(_reply(1, tick=3000, load=420, temperature=31), 1)]
        )
        axis = states[1]["0"]
        assert axis["fb_state"] == 1, "FEEDBACK_MEASURED"
        assert axis["tick"] == 3000
        assert axis["load"] == pytest.approx(42.0)
        assert axis["temp"] == pytest.approx(31.0)

    def test_a_servo_that_stops_answering_goes_held_and_keeps_its_numbers(
        self, probe: Path
    ) -> None:
        """The distinction the whole flag exists for.

        Zeroing the fields here would be the one thing this must never do: a servo
        that has gone away and a servo reading a genuine zero load are the same
        three numbers, which is precisely how the GUI came to show a healthy
        stall it could not have measured.
        """
        states = _feedback_steps(
            probe,
            [
                (_reply(1, tick=3000, load=420, temperature=31), 1),
                (_reply(2, tick=900, load=100), 2),
                (_request(probe, 1), 1),  # servo 1 stops answering
            ],
        )
        held = states[-1]["0"]
        assert held["fb_state"] == 2, "FEEDBACK_HELD: a real reading gone stale"
        assert held["tick"] == 3000, "the last real reading is held, not zeroed"
        assert held["load"] == pytest.approx(42.0)
        assert held["temp"] == pytest.approx(31.0)

        # The other axis is untouched by its neighbour's silence.
        assert states[-1]["1"]["fb_state"] == 1
        assert states[-1]["1"]["load"] == pytest.approx(10.0)

    def test_an_erroring_servo_still_reports_its_position_and_the_error(
        self, probe: Path
    ) -> None:
        """A well-formed frame with a non-zero error is still a valid reading.

        Folding it is right; hiding it is not. `feedback_valid` says the numbers
        are real, `feedback_error` says the servo is unhappy about them.
        """
        states = _feedback_steps(probe, [(_reply(1, tick=2500, error=1), 1)])
        axis = states[1]["0"]
        assert axis["fb_state"] == 1, "a well-formed frame is still a reading"
        assert axis["fb_err"] == 1
        assert axis["tick"] == 2500

    def test_a_servo_that_never_answered_stays_unknown_not_held(
        self, probe: Path
    ) -> None:
        """The distinction a single bool could not carry.

        Observed on the bench (2026-10-02): with `feedback_valid` as a bool, an
        axis whose servo had never answered read as False, and the console
        labelled the *command echo* "held" -- claiming the encoder was at the
        commanded tick. `invalidateAxisFeedback()` is the last place that
        difference is knowable, so it is where it has to be decided: only a state
        that was MEASURED can become HELD.
        """
        states = _feedback_steps(probe, [(_request(probe, 1), 1)])
        axis = states[-1]["0"]
        assert axis["fb_state"] == 0, (
            "an axis that was never measured is UNKNOWN, not HELD -- its numbers "
            "are what we asked for, not what a servo reported"
        )
        assert axis["tick"] == 1000, "still the commanded tick, and labelled as such"

    def test_the_fallback_does_not_overwrite_a_measurement(self, probe: Path) -> None:
        """`updateAxisFeedback` runs every control tick; it must stand down."""
        source = (
            FIRMWARE_DIR / "gondola_model.h"
        ).read_text()
        assert (
            "if (axis.feedbackState != rocsar_v1_FeedbackState_FEEDBACK_UNKNOWN) {"
            in source
        ), (
            "the command-echo fallback must not run over a real reading, or "
            "current_tick on the wire is the command again"
        )


# ---------------------------------------------------------------------------
# The scale constants
# ---------------------------------------------------------------------------


class TestTheScaleConstants:
    def test_load_is_scaled_to_percent_and_the_constant_is_named(self, probe: Path) -> None:
        """The scale has a vendor datum behind it now.

        This test used to assert that "UNVERIFIED" was present in
        gondola_model.h, and to explain in its docstring that the servo manual was
        not in the tree -- it had been confused with a Raspberry Pi cooling-unit
        manual at a path that does not exist here.

        Both halves of that were wrong. The vendor's memory table is checked in at
        docs/Smart  Bus Servo Communication Protocol Manua/sts3215_memory_table.xlsx
        and documents register 0x10, Maximum torque, as "set 1000 = 100% * locked
        torque". So the /10 scale rests on the vendor's own datum rather than on a
        widely-published control table.

        The assertion below therefore pins the citation rather than the doubt. A
        test that can only pass by keeping a false claim in the source is a test
        that protects the falsehood, and it is why the comment survived as long as
        it did. One bench step is still outstanding and is described in the
        comment: whether 0x3C reports current as the same fraction of that limit.
        """
        source = (FIRMWARE_DIR / "gondola_model.h").read_text()
        assert "#define SERVO_LOAD_PERCENT_SCALE 10.0f" in source
        assert "sts3215_memory_table.xlsx" in source, (
            "the load scale must cite the vendor memory table it now rests on"
        )
        assert "set 1000 = 100% * locked torque" in source, (
            "the datum behind the /10 scale should be quoted, not paraphrased"
        )
        assert "UNVERIFIED" not in source, (
            "no scale in this header is unverified now; if one becomes so, say "
            "which register it came from rather than labelling the whole file"
        )
        assert _scan(probe, _reply(1, load=1000), 1)["load"] == pytest.approx(100.0)