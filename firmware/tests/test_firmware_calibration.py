"""Teaching a servo its own centre, and the record that remembers it happened.

The centre tick used to be a firmware variable that `zero` overwrote with the
axis's current reading. That was lost on every power cycle, and it could be set
from a position that had never been measured -- an axis whose servo had never
answered would report its last *commanded* tick, and `zero` would adopt it as the
mechanical centre and report success.

The servo holds its own centre now: `zero` writes a position offset into the
ST3215's EEPROM (register 0x1F) and the firmware verifies both the register and
the resulting reported position before acknowledging. Two consequences drive this
file.

First, the verification has to be provable without a servo on a bus, so it lives
in gondola_model.h as `judgeServoZero()` rather than in the sketch's EEPROM
sequence. Every branch is reachable here.

Second, the servo cannot report whether it has been taught: an axis taught while
sitting at exactly 2048 stores an offset of *zero*, which is indistinguishable
from one never taught. So the fact lives in the flight controller's EEPROM
(calibration.h) and rides the wire as AntennaTelemetry.center_zeroed. An untaught
axis reporting 2048 must not read as a centred one.

Nothing here needs hardware. The bus transactions live in firmware.ino and are
out of scope by construction; what is in scope is everything that decides whether
those transactions worked.
"""

from __future__ import annotations

import re
import zlib
from pathlib import Path

import pytest

from conftest import run

FIRMWARE_DIR = Path(__file__).resolve().parents[1]
INO = (FIRMWARE_DIR / "firmware.ino").read_text()
MODEL = (FIRMWARE_DIR / "gondola_model.h").read_text()
CALIBRATION = (FIRMWARE_DIR / "calibration.h").read_text()
STORE = (FIRMWARE_DIR / "calibration_store.h").read_text()

# The centre tick a taught servo reports at the boresight. Named in the firmware
# so a test can assert against the same constant rather than restating 2048 and
# hoping the two stay in step.
CENTRE_TICK = 2048

# ST3215_SERVO_CENTRE_TICK in the firmware. Parsed rather than copied because a
# copy is a second home for the fact, which is the thing this project keeps
# avoiding.
CENTRE_TICK_RE = re.search(
    r"#define\s+ST3215_SERVO_CENTRE_TICK\s+(\d+)", MODEL
)
TOLERANCE_RE = re.search(
    r"#define\s+ST3215_ZERO_TOLERANCE_TICKS\s+(\d+)", MODEL
)

# Fail at collection rather than with a confusing AttributeError halfway through a
# test if either constant is renamed. These are the two numbers the firmware's
# teach correctness rests on, and a test that quietly stopped parsing them would
# still pass.
assert CENTRE_TICK_RE is not None, "ST3215_SERVO_CENTRE_TICK is gone from gondola_model.h"
assert TOLERANCE_RE is not None, "ST3215_ZERO_TOLERANCE_TICKS is gone from gondola_model.h"


def parse_hex_line(out: str) -> bytes:
    """The probe prints hex bytes compactly and lowercase; take that line."""
    for line in out.splitlines():
        stripped = line.strip()
        if stripped and re.fullmatch(r"[0-9a-fA-F]{2,}", stripped) and len(stripped) % 2 == 0:
            return bytes.fromhex(stripped)
    raise AssertionError(f"no hex line in probe output:\n{out}")


def zero_lines(out: str) -> list[dict[str, str]]:
    """The per-attempt ZERO lines, as dicts, in order."""
    found = []
    for line in out.splitlines():
        if not line.startswith("ZERO "):
            continue
        found.append(dict(part.split("=", 1) for part in line.split()[1:]))
    return found


def state_segments(out: str) -> list[dict[str, str]]:
    """One dict per `|`-separated segment of the *last* STATE line.

    Last, because `--zero-seq` prints one after every attempt and a test asking
    what the state is now means the final one. Segmented because the probe prints
    the global fields and then one block per antenna, and the antenna blocks are
    labelled `a0`, `a1` with no `=` -- so a flat key=value split over the whole
    line does not parse.
    """
    last = [line for line in out.splitlines() if line.startswith("STATE ")]
    for line in reversed(last):
        segments: list[dict[str, str]] = []
        for segment in line.split("|")[1:]:
            fields: dict[str, str] = {}
            for token in segment.split():
                key, sep, value = token.partition("=")
                if not sep:
                    continue
                fields[key] = value
            segments.append(fields)
        return segments
    raise AssertionError(f"no STATE line in probe output:\n{out}")


def axis(out: str, index: int) -> dict[str, str]:
    """The per-axis fields for one antenna, in the order the probe prints them."""
    return state_segments(out)[index]


def strip_comments(text: str) -> str:
    """Code with C/C++ and Python comments removed.

    Several assertions below are about what the code *does* -- no torque register
    is written, no EEPROM header is included by the pure header -- and those
    would all pass vacuously if the surrounding explanation of why could satisfy
    them. The prose matters; it just is not code.
    """
    text = re.sub(r"/\*.*?\*/", "", text, flags=re.S)
    text = re.sub(r"//[^\n]*", "", text)
    return text


# ============================================================================
# The offset arithmetic
# ============================================================================
class TestZeroCorrection:
    """The correction for the 0x1F fallback path -- and why it is a fallback.

    Register 0x1F is documented as "Position correction", two bytes, EEPROM,
    range **-2047 to +2047**, with bit 11 as the direction bit. An earlier version
    of this computed `(desired + 4096 - raw) & 0xFFF` on the reasoning that masking
    into [0, 4095] would work whether the servo read the register signed or
    unsigned. That was wrong, and the vendor table is what shows it: for a register
    whose bit 11 is the direction bit, masking a *negative* correction into
    [0, 4095] does not produce a valid encoding, it lands in the undocumented hole
    between +2047 and -2048. It put an out-of-spec value on the wire for every
    servo sitting above the centre tick.

    The primary path does not use this at all -- it writes 128 to the Torque switch
    and lets the servo do the sum. These tests exist to keep the fallback honest
    while it is still there.
    """

    def test_a_servo_at_the_default_needs_no_correction(self, probe):
        assert correction(probe, CENTRE_TICK) == (0, True)

    def test_a_servo_below_centre_gets_a_positive_correction(self, probe):
        assert correction(probe, 100) == (1948, True)

    def test_a_servo_above_centre_gets_a_negative_correction(self, probe):
        # The case the masking version got wrong. 4000 is above centre, so the
        # correction is -1952, not 2144.
        assert correction(probe, 4000) == (-1952, True)

    def test_every_correction_is_inside_the_documented_range(self, probe):
        # The assertion the old implementation would have failed for half of all
        # positions.
        limit = 2047
        for raw in range(0, 4096, 37):
            value, ok = correction(probe, raw)
            if ok:
                assert -limit <= value <= limit, f"raw {raw} gave {value}"

    def test_a_correction_of_plus_2048_is_refused_rather_than_clamped(self, probe):
        # A servo sitting at exactly tick 0 needs +2048, which does not fit. It is
        # reported, because clamping it would teach the wrong centre and the
        # read-back would then disagree with what was asked for -- which is the one
        # failure this mechanism exists to make impossible.
        value, ok = correction(probe, 0)
        assert value == 2048
        assert ok is False

    def test_the_extreme_negative_correction_is_still_representable(self, probe):
        assert correction(probe, 4095) == (-2047, True)

    def test_applying_the_correction_lands_on_the_centre(self, probe):
        # The property the fallback rests on, for every position that can be
        # encoded: raw + correction, taken mod 4096, must reach the centre tick.
        limit = 2047
        for raw in range(0, 4096, 97):
            value, ok = correction(probe, raw)
            if not ok:
                continue
            assert ring_distance((raw + value) % 4096, CENTRE_TICK) <= limit


def correction(probe, raw: int) -> tuple[int, bool]:
    """The correction the firmware would compute for a servo sitting at `raw`.

    Read back out of the ZERO line, which prints what the firmware produced -- so
    this tests its arithmetic rather than reimplementing it here. A host-side copy
    of the formula would be a second home for the same fact, and agreeing with a
    bug is exactly what a second home lets you do.

    Returns (correction, representable).
    """
    out = run(probe, "--zero-seq", "1", str(raw), "ok")
    line = zero_lines(out)[0]
    return int(line["correction"]), line["representable"] == "1"


def ring_distance(a: int, b: int) -> int:
    d = abs(a - b)
    return min(d, 4096 - d)


# ============================================================================
# Verifying a teach
# ============================================================================
class TestJudgeServoZero:
    """`judgeServoZero()` -- the decision, and why a half-failed teach cannot be
    acknowledged as success.

    The `expect` flag is the only thing the two paths differ on. On the 0x28
    command the servo computed the correction, so the firmware has no value to
    check 0x1F against and only the encoder's answer is trustworthy. On the 0x1F
    fallback the firmware chose the value, so it can demand the register reads
    back exactly that.
    """

    def test_a_clean_teach_on_the_command_path_passes(self, probe):
        assert judge(probe, expect=0, wrote=0, stored=1952, resolution=1, after=CENTRE_TICK) == "ok"

    def test_a_clean_teach_on_the_fallback_path_passes(self, probe):
        assert judge(probe, expect=1, wrote=1952, stored=1952, resolution=1, after=CENTRE_TICK) == "ok"

    def test_the_fallback_can_demand_the_register_agree(self, probe):
        assert judge(probe, expect=1, wrote=1952, stored=1953, resolution=1, after=CENTRE_TICK) == "correction-mismatch"

    def test_the_command_path_cannot_check_a_register_it_did_not_write(self, probe):
        # Nothing to compare against, so a differing stored value is not a failure
        # of the command path -- the servo chose it.
        assert judge(probe, expect=0, wrote=0, stored=4242, resolution=1, after=CENTRE_TICK) == "ok"

    def test_the_axis_landing_one_tick_out_still_counts(self, probe):
        tolerance = int(TOLERANCE_RE.group(1))
        assert tolerance == 1
        assert judge(probe, expect=0, wrote=0, stored=1, resolution=1, after=CENTRE_TICK + 1) == "ok"

    def test_the_axis_landing_two_ticks_out_does_not_count(self, probe):
        assert judge(probe, expect=0, wrote=0, stored=1, resolution=1, after=CENTRE_TICK + 2) == "position-mismatch"

    def test_a_position_that_does_not_move_is_refused(self, probe):
        # This is the case that catches the correction not doing what the memory
        # table says. If 0x28 or 0x1F were applied only to the commanded side, the
        # encoder would keep reporting where it always did.
        assert judge(probe, expect=0, wrote=0, stored=1952, resolution=1, after=4000) == "position-mismatch"

    def test_teaching_an_axis_already_at_centre_is_a_success(self, probe):
        # 0x28 means "current position correction is 2048" and the servo does the
        # arithmetic. An axis already sitting at 2048 has a correction of zero to
        # store, so nothing is written, 0x1F reads zero -- and the encoder still
        # reads centre, which is the entire claim being made.
        #
        # Judged by the register alone this looks like a volatile centre, and that
        # is what it reported on the bench: a teach that worked, refused. The
        # console invites exactly this case by promising the axis will not move.
        assert judge(probe, expect=0, wrote=0, stored=0, resolution=1,
                     after=CENTRE_TICK, required=0) == "ok"

    def test_a_correction_that_should_have_been_stored_and_was_not(self, probe):
        # The other half, and the reason required=0 above is not simply "accept
        # everything". The axis was off centre before the teach, so 0x28 had a
        # non-zero correction to store and stored nothing: the encoder reads centre
        # in RAM and will forget at the next power cycle. A centre that silently
        # evaporates is worse than no centre, because the axis looks centred until
        # the reboot.
        assert judge(probe, expect=0, wrote=0, stored=0, resolution=1,
                     after=CENTRE_TICK, required=1) == "not-persistent"

    def test_the_fallback_rule_is_about_what_it_wrote(self, probe):
        # The rule keys on what ought to be stored, not on which path ran. The
        # fallback wrote zero and zero reads back, so that is agreement. It wrote
        # 1952 and got zero, and that is reported as a mismatch rather than as a
        # bare "not persistent" -- more specific, and checked first, because a
        # register that disagrees with a value we know we wrote is a different
        # fault from one that never held anything.
        assert judge(probe, expect=1, wrote=0, stored=0, resolution=1,
                     after=CENTRE_TICK, required=0) == "ok"
        assert judge(probe, expect=1, wrote=1952, stored=0, resolution=1,
                     after=CENTRE_TICK, required=1) == "correction-mismatch"

    def test_a_changed_tick_scale_is_refused_before_anything_else(self, probe):
        # Checked first, and deliberately: a changed resolution makes every other
        # reading meaningless, so it must not be reported as a position problem.
        assert judge(probe, expect=1, wrote=1952, stored=1953, resolution=3, after=5000) == "angular-resolution"
        assert judge(probe, expect=1, wrote=1952, stored=1952, resolution=0, after=CENTRE_TICK) == "angular-resolution"

    def test_the_scale_must_be_exactly_one(self, probe):
        assert judge(probe, expect=0, wrote=0, stored=1, resolution=1, after=CENTRE_TICK) == "ok"
        for resolution in (0, 2, 3, 255):
            assert judge(probe, expect=0, wrote=0, stored=1, resolution=resolution, after=CENTRE_TICK) == "angular-resolution"

    def test_the_tolerance_is_measured_either_way_round_the_ring(self, probe):
        assert judge(probe, expect=0, wrote=0, stored=1, resolution=1, after=0) == "position-mismatch"
        assert judge(probe, expect=0, wrote=0, stored=1, resolution=1, after=CENTRE_TICK) == "ok"

    def test_a_scale_fault_outranks_a_position_fault(self, probe):
        # Order is load-bearing for whoever is debugging this: reporting the
        # downstream cause sends them to the wrong register.
        assert judge(probe, expect=0, wrote=0, stored=1, resolution=2, after=999) == "angular-resolution"


def judge(probe, expect: int, wrote: int, stored: int, resolution: int,
          after: int, required: int = 1) -> str:
    out = run(probe, "--judge-zero", str(expect), str(wrote), str(stored),
              str(resolution), str(after), str(required))
    assert out.startswith("JUDGE "), out
    return out.split()[1]


# ============================================================================
# The whole command, through the model's three functions
# ============================================================================
class TestZeroCommand:
    """`zero` end to end, minus the bus.

    The sketch calls validateZeroCommand(), then the hardware, then
    commitServoZero(). Driving those in that order here is what makes the
    behaviour of a failed teach -- and of retrying it -- checkable.
    """

    def test_a_clean_teach_marks_the_axis_and_moves_nothing(self, probe):
        out = run(probe, "--zero-seq", "1", "4000", "ok")
        state = axis(out, 0)
        assert state["zeroed"] == "1"
        assert state["center"] == str(CENTRE_TICK)
        # A teach relabels the position the axis is already holding. If the angle
        # were not re-derived here it would keep reporting its old value until the
        # next status poll, and the wire would carry current_tick == center_tick
        # alongside a non-zero angle -- the contradiction the old code left behind.
        assert state["tick"] == str(CENTRE_TICK)

    def test_a_teach_does_not_release_the_boot_interlock(self, probe):
        # Zeroing is not an expressed intent to move an axis nobody has
        # commanded. This held before the change and has to keep holding: a teach
        # that started an axis moving would be the first thing this command ever
        # did to the hardware.
        out = run(probe, "--zero-seq", "1", "4000", "ok")
        assert axis(out, 0)["await"] == "1"

    def test_a_dead_servo_changes_nothing_at_all(self, probe):
        out = run(probe, "--zero-seq", "1", "4000", "no-reply")
        state = axis(out, 0)
        assert zero_lines(out)[0]["outcome"] == "no-servo-reply"
        assert state["zeroed"] == "0"
        assert state["center"] == str(CENTRE_TICK)

    def test_a_write_that_did_not_store_is_not_recorded_as_taught(self, probe):
        # This is the case that could not exist before: the centre is in the
        # servo's EEPROM, so a lost write has left hardware in an unknown state,
        # and the record must not claim otherwise.
        out = run(probe, "--zero-seq", "1", "4000", "correction-lost")
        assert zero_lines(out)[0]["outcome"] == "correction-mismatch"
        assert axis(out, 0)["zeroed"] == "0"

    def test_an_offset_that_stored_without_moving_the_encoder_is_refused(self, probe):
        out = run(probe, "--zero-seq", "1", "4000", "position-odd")
        assert zero_lines(out)[0]["outcome"] == "position-mismatch"
        assert axis(out, 0)["zeroed"] == "0"

    def test_retrying_after_a_failure_converges(self, probe):
        # The recovery path, and the reason no reset command is needed: the offset
        # is always recomputed from wherever the servo actually is, so a run that
        # half-failed is fixed by running it again. Both halves of that have to
        # hold -- the first attempt must not have recorded anything, and the
        # second must actually record.
        out = run(probe, "--zero-seq", "1", "4000", "correction-lost", "4000", "ok")
        attempts = zero_lines(out)
        assert [a["outcome"] for a in attempts] == ["correction-mismatch", "ok"]
        assert attempts[0]["committed"] == "0"
        assert attempts[1]["committed"] == "1"
        assert axis(out, 0)["zeroed"] == "1"

    def test_retrying_from_a_different_position_also_converges(self, probe):
        # The servo may have moved between attempts -- an operator nudged the
        # axis, or it was never where the first attempt found it.
        out = run(probe, "--zero-seq", "1", "4000", "position-odd", "1500", "ok")
        assert zero_lines(out)[1]["outcome"] == "ok"
        assert axis(out, 0)["zeroed"] == "1"

    def test_only_the_named_axis_is_touched(self, probe):
        out = run(probe, "--zero-seq", "1", "4000", "ok")
        assert axis(out, 0)["zeroed"] == "1"
        assert axis(out, 1)["zeroed"] == "0"

    def test_an_unknown_servo_is_refused_before_any_hardware_work(self, probe):
        # The axis resolution happens first, so a typo cannot start an EEPROM
        # sequence against a servo that does not exist.
        out = run(probe, "--zero-seq", "99", "4000", "ok")
        assert "VALIDATE axis=-1" in out
        assert "ZERO" not in out

    def test_a_correction_that_cannot_be_encoded_is_reported_not_clamped(self, probe):
        # A servo sitting at exactly tick 0 needs +2048 and 0x1F tops out at 2047.
        # Clamping would teach the wrong centre.
        out = run(probe, "--zero-seq", "1", "0", "unrepresentable")
        assert zero_lines(out)[0]["outcome"] == "unrepresentable"
        assert axis(out, 0)["zeroed"] == "0"

    def test_a_correction_that_lands_but_does_not_store_is_not_persistent(self, probe):
        out = run(probe, "--zero-seq", "1", "4000", "volatile")
        assert zero_lines(out)[0]["outcome"] == "not-persistent"
        assert axis(out, 0)["zeroed"] == "0"

    def test_the_zero_command_is_not_in_applycommand(self, probe):
        # The old zero case was three lines in applyCommand that set centerTick.
        # It has to stay gone: a centre set without a verified read-back is the
        # defect this whole change exists to remove, and applyCommand is the one
        # function that cannot know whether the bus work succeeded.
        assert "rocsar_v1_PicoCommand_zero_tag:" not in MODEL
        assert "zero_tag" not in MODEL.split("inline rocsar_v1_ErrorCode applyCommand")[-1]

    def test_no_code_path_sets_center_tick_outside_a_commit(self, probe):
        body = MODEL.split("struct AntennaAxis")[1]
        setters = re.findall(r"\.?(centerTick)\s*=", body)
        # One in init(), one in commitServoZero(). Nowhere else.
        assert len(setters) == 2, setters
        commit = MODEL.split("inline void commitServoZero")[1].split("\n}\n")[0]
        assert "centerTick" in commit


# ============================================================================
# The bus packets
# ============================================================================
class TestCalibrationBusPackets:
    """The packets the teach puts on the wire.

    These are compared against docs/ST3215_Configure/ST3215_Configure.ino, which
    is the vendor's own implementation and the only checked-in authority for this
    bus there was.
    """

    def test_unlocking_the_eeprom_matches_the_vendor_tool_byte_for_byte(self, probe):
        # ST3215_Configure.ino writeReg8(1, REG_EEPROM_LOCK=0x37, 0x00)
        out = run(probe, "--servo-write8", "1", "0x37", "0x00")
        packet = parse_hex_line(out)
        assert list(packet[:7]) == [0xFF, 0xFF, 0x01, 0x04, 0x03, 0x37, 0x00]
        assert len(packet) == 8

    def test_relocking_the_eeprom_is_the_same_packet(self, probe):
        out = run(probe, "--servo-write8", "1", "0x37", "0x01")
        packet = parse_hex_line(out)
        assert list(packet[:7]) == [0xFF, 0xFF, 0x01, 0x04, 0x03, 0x37, 0x01]

    def test_writing_the_offset_matches_the_vendor_tool_byte_for_byte(self, probe):
        # ST3215_Configure.ino writeReg16: {FF, FF, id, 0x05, INST_WRITE, reg, lo, hi, chk}
        out = run(probe, "--servo-write16", "1", "0x1F", "2144")
        packet = parse_hex_line(out)
        assert list(packet[:8]) == [0xFF, 0xFF, 0x01, 0x05, 0x03, 0x1F, 0x60, 0x08]
        assert len(packet) == 9

    def test_the_offset_is_written_little_endian(self, probe):
        out = run(probe, "--servo-write16", "1", "0x1F", "0x0102")
        packet = parse_hex_line(out)
        assert packet[6] == 0x02 and packet[7] == 0x01

    def test_the_readback_asks_for_four_registers(self, probe):
        # Four, not two, and the reason is in gondola_model.h: a 2-byte read comes
        # back as an 8-byte frame at LEN=0x04, which is exactly the shape of the
        # echo of our own request. Reading four gives LEN=0x06 and cannot be
        # confused with one.
        out = run(probe, "--servo-register-read", "1", "0x1E", "4")
        packet = parse_hex_line(out)
        assert list(packet[:7]) == [0xFF, 0xFF, 0x01, 0x04, 0x02, 0x1E, 0x04]

    def test_the_readback_still_asks_the_telemetry_block_for_the_telemetry_read(self, probe):
        out = run(probe, "--servo-status-request", "1")
        packet = parse_hex_line(out)
        assert list(packet[:7]) == [0xFF, 0xFF, 0x01, 0x04, 0x02, 0x38, 0x08]

    def test_the_checksum_is_the_ones_complement_of_bytes_two_to_len_minus_two(self, probe):
        for args in (
            ("--servo-write8", "1", "0x37", "0x00"),
            ("--servo-write16", "1", "0x1F", "2144"),
            ("--servo-register-read", "1", "0x1E", "4"),
        ):
            out = run(probe, *args)
            packet = parse_hex_line(out)
            total = sum(packet[2:-1])
            assert packet[-1] == (~total & 0xFF), args

    def test_the_goal_position_packet_is_unchanged_by_the_refactor(self, probe):
        # buildServoPacket was rewritten in terms of a generic builder. These are
        # the bytes it produced before that refactor, and a change to them would be
        # a change to every motion command on the aircraft.
        out = run(probe, "--servo-packet", "1", "3000")
        packet = parse_hex_line(out)
        assert list(packet) == [0xFF, 0xFF, 0x01, 0x09, 0x03, 0x2A, 0xB8, 0x0B,
                                0x00, 0x00, 0x00, 0x00, 0x05]


# ============================================================================
# Scanning the read-back
# ============================================================================
def servo_checksum(frame: bytes) -> int:
    return (~sum(frame[2:-1])) & 0xFF


def calib_reply(servo_id: int, resolution: int = 1, offset: int = 0, mode: int = 0,
                err: int = 0, length: int = 0x06) -> bytes:
    """A four-byte calibration read reply, built the way the servo would send it."""
    body = [err, resolution, offset & 0xFF, (offset >> 8) & 0xFF, mode]
    frame = bytes([0xFF, 0xFF, servo_id, length] + body)
    return frame + bytes([servo_checksum(frame)])


def request_echo(servo_id: int, reg: int = 0x1E, count: int = 4) -> bytes:
    """The 8-byte echo of our own register-read request."""
    frame = bytes([0xFF, 0xFF, servo_id, 0x04, 0x02, reg, count])
    return frame + bytes([servo_checksum(frame)])


class TestCalibrationScan:
    """`scanServoCalibRead()`.

    The scanner has to reject the echo of the request that provoked the read,
    skip the other antenna's frame without desynchronising, and never wedge on a
    frame it has already refused.
    """

    def test_it_reads_the_offset_resolution_and_mode(self, probe):
        out = run(probe, "--servo-calib-scan", calib_reply(1, 1, 2144, 0).hex(), "1")
        assert "matched=1" in out
        assert "offset=2144" in out
        assert "resolution=1" in out
        assert "mode=0" in out

    def test_it_rejects_the_echo_of_its_own_request(self, probe):
        # The whole reason the read is four bytes. A 2-byte read would be an
        # 8-byte frame at LEN=0x04 and this scanner would take it for an answer.
        out = run(probe, "--servo-calib-scan", request_echo(1).hex(), "1")
        assert "matched=0" in out

    def test_it_finds_the_reply_behind_the_echo(self, probe):
        stream = request_echo(1) + calib_reply(1, 1, 99, 0)
        out = run(probe, "--servo-calib-scan", stream.hex(), "1")
        assert "matched=1" in out and "offset=99" in out

    def test_it_skips_the_other_antennas_reply_and_keeps_looking(self, probe):
        # Both antennas share one bus, so the other axis's frame arrives
        # interleaved. Throwing it away without continuing would lose the frame we
        # did want.
        stream = calib_reply(2, 1, 111, 0) + calib_reply(1, 1, 222, 0)
        out = run(probe, "--servo-calib-scan", stream.hex(), "1")
        assert "matched=1" in out and "offset=222" in out

    def test_it_rejects_a_corrupt_checksum_and_resynchronises(self, probe):
        broken = bytearray(calib_reply(1, 1, 99, 0))
        broken[-1] ^= 0xFF
        stream = bytes(broken) + calib_reply(1, 1, 222, 0)
        out = run(probe, "--servo-calib-scan", stream.hex(), "1")
        assert "matched=1" in out and "offset=222" in out

    def test_it_leading_noise_lands_on_the_frame(self, probe):
        stream = bytes([0x00, 0xFF, 0x13, 0x37]) + calib_reply(1, 1, 42, 0)
        out = run(probe, "--servo-calib-scan", stream.hex(), "1")
        assert "matched=1" in out and "offset=42" in out

    def test_a_partial_frame_is_held_rather_than_rejected(self, probe):
        # The rest may still be on the wire. Treating it as complete and moving on
        # would discard a good answer.
        full = calib_reply(1, 1, 42, 0)
        out = run(probe, "--servo-calib-scan", full[:-3].hex(), "1")
        assert "matched=0" in out
        assert "consumed=0" in out

    def test_it_consumes_everything_it_rejected(self, probe):
        # scanServoStatus guarantees the caller can always advance, so a byte the
        # scanner has already refused is never re-examined on the next pass.
        out = run(probe, "--servo-calib-scan", request_echo(1).hex(), "1")
        assert "consumed=8 of 8" in out

    def test_an_absurd_length_byte_does_not_wedge_it(self, probe):
        frame = bytes([0xFF, 0xFF, 0x01, 0xF0, 0x00, 0x00, 0x00])
        frame += bytes([servo_checksum(frame)])
        stream = frame + calib_reply(1, 1, 7, 0)
        out = run(probe, "--servo-calib-scan", stream.hex(), "1")
        assert "matched=1" in out and "offset=7" in out

    def test_it_ignores_a_14_byte_status_reply(self, probe):
        # The other scanner's frame. A teach must not mistake a telemetry frame
        # for its register read.
        frame = bytes([0xFF, 0xFF, 0x01, 0x0A, 0x00]) + bytes(8)
        frame += bytes([servo_checksum(frame)])
        stream = frame + calib_reply(1, 1, 55, 0)
        out = run(probe, "--servo-calib-scan", stream.hex(), "1")
        assert "matched=1" in out and "offset=55" in out


# ============================================================================
# The stored record
# ============================================================================
class TestCalibrationRecord:
    """calibration.h -- the flight controller's own record of what was taught."""

    def test_the_record_is_small_enough_to_state_in_a_comment(self, probe):
        out = run(probe, "--calib", "1", "0")
        # One axis flag per antenna, plus an 8-byte header and a 4-byte CRC.
        assert 14 <= len(parse_hex_line(out)) <= 24

    def test_a_round_trip_preserves_both_axes(self, probe):
        for flags in (("0", "0"), ("1", "0"), ("0", "1"), ("1", "1")):
            out = run(probe, "--calib", *flags)
            assert f"CALIB ok {flags[0]} {flags[1]}" in out, flags

    def test_the_crc_is_a_real_crc32(self, probe):
        # Compared against zlib's, which is the same polynomial. A CRC that only
        # agrees with itself detects nothing; this is what makes it a torn-write
        # check rather than decoration.
        out = run(probe, "--calib", "1", "1")
        raw = parse_hex_line(out)
        body, stored = raw[:-4], raw[-4:]
        assert int.from_bytes(stored, "little") == zlib.crc32(body)

    def test_an_erased_record_is_reported_as_absent_not_corrupt(self, probe):
        # A blank board and a corrupt board are different facts, and an operator
        # should be told which one they are looking at.
        out = run(probe, "--calib-decode", "FF" * 32)
        assert _result(out) == "no record"

    def test_a_wrong_magic_is_rejected(self, probe):
        raw = bytearray(parse_hex_line(run(probe, "--calib", "1", "1")))
        raw[0] = ord("X")
        out = run(probe, "--calib-decode", raw.hex())
        assert _result(out) == "bad magic"

    def test_an_unknown_schema_version_is_rejected_not_reinterpreted(self, probe):
        # Reading a mount offset out of bytes that are now a datum is how a board
        # ends up aiming somewhere confident.
        raw = bytearray(parse_hex_line(run(probe, "--calib", "1", "1")))
        raw[4] = 99
        _refix(raw)
        out = run(probe, "--calib-decode", raw.hex())
        assert _result(out) == "unknown schema version"

    def test_an_axis_count_from_another_build_is_rejected(self, probe):
        raw = bytearray(parse_hex_line(run(probe, "--calib", "1", "1")))
        raw[5] = 3
        _refix(raw)
        out = run(probe, "--calib-decode", raw.hex())
        assert _result(out) == "axis count mismatch"

    def test_a_torn_write_is_caught_by_the_crc(self, probe):
        # The failure this whole record exists for. EEPROMClass::commit() erases
        # the sector and reprograms it with interrupts off; a power cut in that
        # window leaves bytes that are neither the old record nor the new one.
        raw = bytearray(parse_hex_line(run(probe, "--calib", "1", "1")))
        raw[9] ^= 0x01  # flip one bit in the axis flags, leave the CRC alone
        out = run(probe, "--calib-decode", raw.hex())
        assert _result(out) == "bad CRC"

    def test_every_single_bit_flip_in_the_record_is_caught(self, probe):
        # Stronger than picking one byte: the CRC is the only thing standing
        # between a torn write and an antenna aimed using a centre that never
        # existed, so it is worth knowing there is no hole in it.
        raw = parse_hex_line(run(probe, "--calib", "1", "0"))
        for index in range(len(raw)):
            for bit in range(8):
                mutated = bytearray(raw)
                mutated[index] ^= 1 << bit
                out = run(probe, "--calib-decode", mutated.hex())
                assert "CALIB ok" not in out, f"bit {bit} of byte {index} slipped through"

    def test_a_rejected_record_leaves_the_record_untouched(self, probe):
        # The contract that keeps a rejected record from being half-applied. The
        # probe prints -1 for the flags precisely so this is observable.
        raw = bytearray(parse_hex_line(run(probe, "--calib", "1", "1")))
        raw[0] = ord("X")
        out = run(probe, "--calib-decode", raw.hex())
        assert _result(out) == "bad magic" and "-1 -1" in out

    def test_a_truncated_record_is_rejected(self, probe):
        raw = parse_hex_line(run(probe, "--calib", "1", "1"))
        out = run(probe, "--calib-decode", raw[:8].hex())
        assert "CALIB" in out and "ok" not in out.split("CALIB ")[1]


def _result(out: str) -> str:
    """The decode result, without the parenthetical the firmware explains it with.

    calibrationResultName() returns "bad CRC (write was torn)" rather than just
    "bad CRC", which is the right thing for a boot log and the wrong shape for an
    equality assertion -- so the name is compared, not the whole line.
    """
    line = out.split("CALIB ")[1]
    name = line.split(" (")[0]
    # Then drop the two flag columns the probe prints after it. Only some of the
    # names carry a parenthetical explanation, so neither half of the format can
    # be relied on to be the boundary.
    return re.sub(r"\s+(-?\d+)\s+(-?\d+)\s*$", "", name).strip()


def _refix(raw: bytearray) -> None:
    """Recompute the CRC after a test has deliberately corrupted a header byte."""
    body = bytes(raw[:-4])
    crc = zlib.crc32(body).to_bytes(4, "little")
    raw[-4:] = crc


# ============================================================================
# Source shape
# ============================================================================
class TestShape:
    """The structural claims, which are the ones a reviewer has to take on trust
    from reading the diff."""

    def test_the_teach_writes_the_correction_command_and_never_disables_torque(self, probe):
        # Register 0x28 carries three commands: 0 turns torque off, 1 turns it on,
        # and 128 corrects the current position to 2048. Only the third is ever
        # written here.
        #
        # This is the inverse of what the test asserted before the vendor's memory
        # table was found: it used to be "0x28 appears nowhere", on the reasoning
        # that disabling torque on a 5:1 gear train carrying an antenna means the
        # axis goes limp. That reasoning still holds -- it is just that 0x28 turns
        # out to be the documented way to set a centre at all.
        teach = strip_comments(_ino_symbol("zeroServoHardware"))
        assert "REG_ST3215_TORQUE_SWITCH" in teach
        assert "ST3215_CORRECT_POSITION_COMMAND" in teach
        # And never 0 or 1 to it: no torque-off, so nothing drops.
        assert "TORQUE_OFF" not in teach
        assert not re.search(r"REG_ST3215_TORQUE_SWITCH,\s*0\b", teach)
        assert not re.search(r"REG_ST3215_TORQUE_SWITCH,\s*1\b", teach)

    def test_the_correction_command_is_the_documented_value(self, probe):
        # "write 128: current position correction is 2048", from register 0x28's
        # entry in the vendor memory table.
        assert re.search(r"#define\s+ST3215_CORRECT_POSITION_COMMAND\s+128\b", MODEL)

    def test_the_command_path_is_tried_before_the_fallback(self, probe):
        teach = _ino_symbol("zeroServoHardware")
        command = teach.index("REG_ST3215_TORQUE_SWITCH")
        fallback = teach.index("REG_ST3215_POSITION_CORRECTION")
        assert command < fallback

    def test_the_fallback_is_only_reached_when_the_command_did_not_move_the_axis(self, probe):
        # Falling back on, say, a changed tick scale would be guessing: the
        # fallback writes a correction the scale has already invalidated.
        teach = _ino_symbol("zeroServoHardware")
        guard = teach.index("ZERO_POSITION_MISMATCH")
        assert teach.index("verifyServoTeach") < guard
        assert teach.index("servoZeroCorrection") > guard

    def test_the_vendor_settle_delay_is_used_and_is_theirs(self):
        assert "delay(ST3215_EEPROM_SETTLE_MS)" in INO
        assert re.search(r"#define\s+ST3215_EEPROM_SETTLE_MS\s+20\b", MODEL)

    def test_the_eeprom_is_unlocked_and_relocked_around_the_write(self):
        # The vendor's sequence, in its order. Leaving a servo's EEPROM unlocked
        # is a thing a later unrelated write could land in, and there is no way to
        # know from here whether the register write itself took.
        teach = strip_comments(_ino_symbol("zeroServoHardware"))
        unlock = teach.index("ST3215_EEPROM_UNLOCKED")
        write = teach.index("writeServoRegister16")
        relock = teach.index("ST3215_EEPROM_LOCKED")
        assert unlock < write < relock

    def test_the_readback_happens_after_the_write_not_before(self):
        # The order is the verification. Reading the register before writing it
        # would verify the old value and pass.
        #
        # Checked in verifyServoTeach(), which both paths share -- so a check here
        # covers the command and the fallback at once.
        teach = _ino_symbol("zeroServoHardware")
        verify = _ino_symbol("verifyServoTeach")
        assert verify.index("readServoRegisterHardware") < verify.index("readServoTelemetryHardware")
        assert teach.index("verifyServoTeach") < teach.index("servoZeroCorrection")

    def test_the_decision_comes_after_both_readbacks(self):
        # judgeServoZero needs the register read back and the position read back.
        # Deciding before the last read would decide on a position that had not
        # been observed.
        verify = _ino_symbol("verifyServoTeach")
        assert verify.rindex("readServoTelemetryHardware") < verify.index("judgeServoZero")

    def test_the_bus_is_drained_before_each_read(self):
        # Half-duplex: every request comes back as an echo. The official tool
        # drains too, and without it a read can parse its own echo.
        assert "drainServoBus();" in INO
        assert "drainServoBus();\n  Serial1.write(request" in INO

    def test_the_teach_takes_its_own_reading(self):
        # A `zero` in the first 40 ms after boot would otherwise have nothing to
        # measure from, because the alternating poll had not run yet.
        assert "readServoTelemetryHardware(id, before)" in INO

    def test_the_teach_acknowledgement_follows_the_hardware(self):
        # The ack that reports the teach's outcome is built from it, after the bus
        # work. An ack sent first would report success for a write that never
        # landed -- which is the one thing this command must not do.
        handler = _ino_symbol("handleZeroCommand")
        assert handler.index("zeroServoHardware") < handler.rindex("sendCommandResponse")

    def test_the_only_early_ack_is_a_validation_refusal(self):
        # There is one ack before the hardware, and it answers "no such servo" --
        # where no EEPROM sequence was started and there is no result to wait for.
        # It is deliberately ERROR_INVALID_SERVO and not ERROR_CALIBRATION_FAILED,
        # because nothing was written and nothing is in doubt.
        handler = _ino_symbol("handleZeroCommand")
        guard = handler.index("if (axisIndex < 0)")
        assert guard < handler.index("sendCommandResponse")
        # Everything between the guard and the ack that answers it is the refusal.
        refusal = handler[guard:handler.index("sendCommandResponse") + 120]
        assert "ERROR_INVALID_SERVO" in refusal

    def test_a_failed_teach_reports_calibration_failed_not_a_generic_fault(self):
        # It means "the servo may now hold a partial change", which is a different
        # thing from "nothing answered on the bus". The mapping lives in the model
        # now, in one function, so the two places that used to reason about which
        # error means what cannot drift apart.
        mapping = _model_symbol("servoZeroErrorCode")
        assert "ERROR_CALIBRATION_FAILED" in mapping
        assert "ERROR_HARDWARE_FAULT" in mapping
        assert "ZERO_NO_SERVO_REPLY" in mapping
        # And the sketch uses that mapping rather than switching on the outcome.
        assert "servoZeroErrorCode(outcome)" in _ino_symbol("handleZeroCommand")
        assert "switch (outcome)" not in _ino_symbol("handleZeroCommand")

    def test_the_record_is_saved_only_on_a_teach_that_took(self):
        handler = _ino_symbol("handleZeroCommand")
        assert handler.index("commitServoZero") < handler.index("calibrationSave")

    def test_nothing_in_the_control_loop_commits_to_eeprom(self):
        # EEPROMClass::commit() takes interrupts off for a whole sector erase --
        # tens of milliseconds -- and it must never be reachable from the 20 ms
        # tick.
        loop_body = INO.split("void loop()")[1]
        assert "commit()" not in loop_body
        assert "calibrationSave" not in loop_body
        assert "calibrationReset" not in loop_body

    def test_only_the_store_includes_eeprom(self):
        # calibration.h is compiled and driven on a host by conftest.py. The
        # moment it needs an Arduino header that stops being true, and every
        # record test in this file silently stops running.
        assert "#include <EEPROM.h>" not in strip_comments(CALIBRATION)
        assert "#include <EEPROM.h>" in STORE
        assert "EEPROM" not in strip_comments(CALIBRATION)

    def test_the_format_does_not_memcpy_a_struct(self):
        # EEPROMClass::get<T>() and put<T>() are exactly that, and a struct brings
        # its padding and the host's endianness into a format that has to survive
        # both.
        code = strip_comments(STORE)
        assert "EEPROM.get<" not in code and "EEPROM.put<" not in code
        assert "EEPROM.write" in code

    def test_the_layout_is_pinned(self):
        assert "static_assert" in CALIBRATION

    def test_the_centre_constant_is_named_and_overridable(self):
        assert int(CENTRE_TICK_RE.group(1)) == CENTRE_TICK
        assert "#ifndef ST3215_SERVO_CENTRE_TICK" in MODEL
        # And the bare literal it replaced must not still be sitting in the
        # initialiser, where it read as a measurement.
        initialiser = _model_symbol("initGondolaState")
        assert "2048" not in initialiser
        assert "ST3215_SERVO_CENTRE_TICK" in initialiser

    def test_the_framing_citation_points_at_a_file_that_exists(self):
        # It said tools/ST3215_Configure/... and that path was not in the tree, so
        # nothing in this firmware had a checked-in authority for the bus it
        # drives.
        assert "docs/ST3215_Configure/ST3215_Configure.ino" in MODEL
        assert (FIRMWARE_DIR.parent / "docs/ST3215_Configure/ST3215_Configure.ino").exists()

    def test_the_untaught_flag_rides_the_wire(self):
        assert "entry.center_zeroed" in MODEL

    def test_the_boot_log_says_when_there_is_no_record(self):
        # The one moment an operator is guaranteed to see it. A calibration that
        # quietly fell back to defaults is the failure this replaces.
        assert "calibrationResultName" in INO
        assert "assumed centre" in INO


def _ino_symbol(name: str) -> str:
    """The body of one *definition* in the sketch, comments stripped.

    Brace-matched rather than split on a delimiter, because neither of the obvious
    delimiters works here. `loop()` contains `handleCommand(cmd);`, so a last-match
    search lands on a call site; and handleZeroCommand has an early return, so a
    split on the closing brace cuts the body in half. Comments are stripped after
    the match so braces inside prose cannot unbalance it.
    """
    return strip_comments(_definition_body(INO, name))


def _definition_body(text: str, name: str) -> str:
    """The `{ ... }` body of the function `name`, matched on braces.

    Picks the definition rather than a declaration or a call by requiring the
    signature to end with `{` before any `;`, which a prototype and a call site
    both fail.
    """
    for match in re.finditer(rf"^[\w:<>&*\[\] ,]*\b{name}\s*\([^;{{]*\)\s*\{{", text,
                             re.MULTILINE):
        start = match.end() - 1
        depth = 0
        for index in range(start, len(text)):
            if text[index] == "{":
                depth += 1
            elif text[index] == "}":
                depth -= 1
                if depth == 0:
                    return text[start + 1:index]
    raise AssertionError(f"no definition of {name} found")


def _model_symbol(name: str) -> str:
    """The body of one inline function in the model, comments stripped."""
    return strip_comments(_definition_body(MODEL, name))


# ============================================================================
# The command that reaches the wire
# ============================================================================
class TestZeroOnTheWire:
    """The command itself did not change. This says so, because that was the
    constraint: the host, the GUI and the CLI all speak the same `zero` as
    before, and only the firmware's implementation of it moved."""

    def test_the_command_is_still_a_bare_servo_id(self):
        proto = (FIRMWARE_DIR.parent / "api/rocsar/v1/pico.proto").read_text()
        block = proto.split("message ZeroCommand {")[1].split("}")[0]
        fields = [f.strip() for f in block.strip().splitlines() if f.strip()]
        assert fields == ["uint32 servo_id = 1;"]

    def test_it_is_still_in_the_oneof_at_the_same_number(self):
        proto = (FIRMWARE_DIR.parent / "api/rocsar/v1/pico.proto").read_text()
        assert "ZeroCommand zero = 4;" in proto

    def test_the_proto_says_it_relabels_rather_than_moves(self):
        # The old comment said "return an axis to its mechanical centre", which
        # reads as motion. It does not move the axis, and an operator who braced
        # for a movement would have been misled. So the schema -- which is what a
        # reader of the host code sees first -- has to say so too.
        proto = (FIRMWARE_DIR.parent / "api/rocsar/v1/pico.proto").read_text()
        header = proto[: proto.index("message ZeroCommand {")]
        assert "does NOT drive the axis anywhere" in header
