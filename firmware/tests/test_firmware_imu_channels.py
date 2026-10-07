"""What the BNO055 is already reporting, and what was being thrown away.

Before this, the sketch made two calls on the sensor: one VECTOR_EULER read from
which it kept one of three angles, and one getTemp(). Everything else the sensor
produces -- the other two Euler angles, the calibration status, the
gravity-free acceleration -- was discarded on every tick.

That is a stratospheric balloon, not a launch vehicle, and it inverts the usual
priority. There is no boost phase, so accelerations are gravity plus pendulum sway
and streaming raw 6-axis buys nothing while costing the frame budget that attitude
and calibration need. What this file covers is the set that answers questions the
mission actually has.

The policy lives in gondola_model.h and is host-testable; the sensor plumbing that
feeds it is in firmware.ino and is asserted by source shape here.
"""

from __future__ import annotations

import re
from pathlib import Path

import pytest

from conftest import run

FIRMWARE_DIR = Path(__file__).resolve().parents[1]
INO = (FIRMWARE_DIR / "firmware.ino").read_text()
MODEL = (FIRMWARE_DIR / "gondola_model.h").read_text()


def attitude_lines(out: str) -> list[dict[str, str]]:
    """Each ATTITUDE line is "<sample> roll=<..> pitch=<..>", so the outcome is a
    bare token and cannot be parsed as key=value."""
    lines = []
    for line in out.splitlines():
        if not line.startswith("ATTITUDE "):
            continue
        parts = line.split()[1:]
        entry = {"sample": parts[0]}
        for part in parts[1:]:
            key, sep, value = part.partition("=")
            if sep:
                entry[key] = value
        lines.append(entry)
    return lines


def peak_lines(out: str) -> list[dict[str, str]]:
    return [
        dict(part.split("=", 1) for part in line.split()[1:])
        for line in out.splitlines()
        if line.startswith("PEAK ")
    ]


def imu_state(out: str) -> dict[str, str]:
    """The IMU fields from the trailing STATE line."""
    for line in reversed(out.splitlines()):
        if not line.startswith("STATE "):
            continue
        fields = {}
        for token in line.split("|")[-1].split():
            key, sep, value = token.partition("=")
            if sep:
                fields[key] = value
        return fields
    raise AssertionError(f"no STATE line in probe output:\n{out}")


def peak_axes(out: str) -> tuple[float, float, float]:
    return tuple(float(v) for v in imu_state(out)["peak"].split(","))  # type: ignore[return-value]


# ============================================================================
# Tilt
# ============================================================================
class TestAttitude:
    """applyImuAttitude() -- roll and pitch, held on absence and rejected on a
    non-finite sample, exactly as the bearing is.

    The value of these two numbers is not attitude. It is that they are the only
    available evidence about whether the heading is any good: the BNO055
    tilt-compensates its fusion using its accelerometer, and a gondola on a 10-40 m
    tether swings at roughly 0.1 Hz, so sway corrupts the estimate the heading
    correction depends on.
    """

    def test_a_real_sample_is_applied(self, probe):
        out = run(probe, "--attitude", "3.5", "-2.25")
        line = attitude_lines(out)[0]
        assert line["roll"] == "3.500"
        assert line["pitch"] == "-2.250"

    def test_an_absent_sensor_holds_the_last_tilt(self, probe):
        # Not zeroed. A level gondola and a dead sensor both read 0/0, and the
        # second is the one that matters.
        out = run(probe, "--attitude", "12", "-7", "none", "none")
        lines = attitude_lines(out)
        assert lines[0]["roll"] == "12.000"
        assert lines[1]["roll"] == "12.000"
        assert lines[1]["pitch"] == "-7.000"
        assert lines[1]["sample"] == "NO_SENSOR"

    def test_a_non_finite_sample_is_rejected_not_applied(self, probe):
        for bad in ("nan", "inf", "-inf"):
            out = run(probe, "--attitude", "5", "1", bad, "0")
            lines = attitude_lines(out)
            assert lines[1]["roll"] == "5.000", bad
            assert lines[1]["pitch"] == "1.000", bad

    def test_the_outcome_names_the_three_cases_distinctly(self, probe):
        # A caller has to be able to tell "no sensor" from "the sensor said
        # nothing usable" from "here is a measurement", because all three leave
        # the tilt at its previous value and only one of them is a reading.
        out = run(probe, "--attitude", "1", "2", "none", "none", "nan", "0")
        assert [line["sample"] for line in attitude_lines(out)] == [
            "APPLIED",
            "NO_SENSOR",
            "REJECTED",
        ]

    def test_tilt_is_not_filtered(self):
        # The heading is filtered; tilt deliberately is not. Filtering the
        # evidence that the heading has degraded would only delay it.
        assert "IMU_ALPHA" not in _model_symbol("applyImuAttitude")

    def test_the_datum_is_not_folded_into_the_tilt(self):
        # The heading datum says which way the gondola points. It says nothing
        # about which way is up, and folding one into the other would make the
        # tilt move when an operator sets a bearing.
        assert "headingDatum" not in _model_symbol("applyImuAttitude")
        assert "gondolaHeading" not in _model_symbol("applyImuAttitude")


# ============================================================================
# Calibration status
# ============================================================================
class TestCalibrationStatus:
    """setImuCalibration() / packImuCalibration() -- register 0x35, verbatim.

    The wire carries the register byte and so does the model, because the packet
    register is bit-identical to repacking Adafruit's four unpacked 2-bit fields.
    The packing is pinned here so the layout cannot drift while the encoding
    still looks right.
    """

    def test_the_bit_layout_is_two_bits_per_sensor_system_first(self, probe):
        # sys 3, gyro 3, accel 2, mag 1 -> 0xF9.
        out = run(probe, "--calibration", "3", "3", "2", "1")
        assert "PACKED 0xF9" in out

    def test_it_round_trips(self, probe):
        for sys in range(4):
            for mag in range(4):
                out = run(probe, "--calibration", str(sys), "0", "0", str(mag))
                line = next(l for l in out.splitlines() if l.startswith("CALIB "))
                assert line.split()[1:] == [str(sys), "0", "0", str(mag)], (sys, mag)

    def test_the_most_significant_pair_is_the_system(self, probe):
        # Getting the order wrong is the failure this pins: a consumer asking
        # "is the magnetometer calibrated" would read the system bit instead and
        # be told yes by a sensor whose magnetometer has never been calibrated.
        out = run(probe, "--calibration", "0", "0", "0", "3")
        assert "PACKED 0x03" in out

    def test_it_is_stored_even_when_the_sensor_is_absent(self):
        # Unlike every reading, the calibration of a sensor that has stopped
        # answering is still the last thing it told us -- and it is what tells an
        # operator why.
        body = _model_symbol("setImuCalibration")
        assert "sensorPresent" not in body

    def test_it_is_a_plain_store_with_no_policy_in_it(self):
        assert "if" not in _model_symbol("setImuCalibration")


# ============================================================================
# Peak-hold acceleration
# ============================================================================
class TestPeakAccel:
    """noteImuPeakAccel() -- a monotonic maximum with an event counter beside it.

    The events worth catching are rare and transient: balloon rupture into
    free-fall until the chute deploys, the valve transition, recovery shock. A
    20 ms sample stream would miss most of them and cost the frame budget that
    attitude and calibration need. A held maximum costs a fixed 18 bytes and
    catches the whole transient.
    """

    def test_the_first_sample_raises_and_counts_one_event(self, probe):
        out = run(probe, "--peak-accel", "1", "2", "3")
        line = peak_lines(out)[0]
        assert line["raised"] == "1"
        assert line["events"] == "1"
        assert peak_axes(out) == (1.0, 2.0, 3.0)

    def test_a_smaller_sample_raises_nothing(self, probe):
        out = run(probe, "--peak-accel", "1", "2", "3", "0.5", "1", "2")
        lines = peak_lines(out)
        assert lines[1]["raised"] == "0"
        assert lines[1]["events"] == "1"
        assert peak_axes(out) == (1.0, 2.0, 3.0)

    def test_the_peak_never_falls(self, probe):
        out = run(probe, "--peak-accel", "9", "0", "0", "1", "1", "1", "0.1", "0.1", "0.1")
        assert peak_axes(out) == (9.0, 1.0, 1.0)
        assert peak_lines(out)[-1]["events"] == "2"

    def test_magnitude_is_tracked_not_the_signed_value(self, probe):
        # -5 m/s^2 and +5 m/s^2 are the same event to a peak detector, and
        # keeping the sign would make the held value depend on which direction
        # the gondola happened to be swinging when it was hit.
        out = run(probe, "--peak-accel", "-5", "0", "0")
        assert peak_axes(out) == (5.0, 0.0, 0.0)

    def test_a_shock_on_three_axes_is_one_event_not_three(self, probe):
        # This is the case the counter exists for. A chute deployment moves all
        # three axes at once; a counter that ticked three times for it would read
        # as three events on a 1 Hz link.
        out = run(probe, "--peak-accel", "30", "20", "40")
        line = peak_lines(out)[0]
        assert line["events"] == "1"
        assert peak_axes(out) == (30.0, 20.0, 40.0)

    def test_two_separate_shocks_are_two_events(self, probe):
        out = run(probe, "--peak-accel", "10", "0", "0", "0", "0", "0", "20", "0", "0")
        assert [l["events"] for l in peak_lines(out)] == ["1", "1", "2"]

    def test_a_non_finite_sample_is_ignored_rather_than_compared(self, probe):
        # NaN compares false against everything, so a naive maximum would keep
        # the previous peak and the sample would look like a quiet one instead of
        # a broken sensor.
        out = run(probe, "--peak-accel", "5", "0", "0", "nan", "inf", "0")
        lines = peak_lines(out)
        assert lines[1]["raised"] == "0"
        assert lines[1]["events"] == "1"
        assert peak_axes(out) == (5.0, 0.0, 0.0)

    def test_the_maximum_is_over_axes_not_over_the_vector(self, probe):
        # A single 30 m/s^2 on one axis is the event. Requiring the vector norm to
        # exceed a threshold would bury it in two quiet axes.
        out = run(probe, "--peak-accel", "30", "0", "0")
        assert peak_axes(out)[0] == 30.0


# ============================================================================
# On the wire
# ============================================================================
class TestOnTheWire:
    def test_all_of_it_reaches_fillTelemetryMessage(self, probe):
        body = _model_symbol("fillTelemetryMessage")
        for field in (
            "gondola_roll_deg",
            "gondola_pitch_deg",
            "imu_calibration",
            "imu_peak_accel_x_ms2",
            "imu_peak_accel_y_ms2",
            "imu_peak_accel_z_ms2",
            "imu_peak_accel_event",
        ):
            assert field in body, field

    def test_the_peak_axes_are_copied_in_order(self, probe):
        # A hand-rolled loop would be one transposition away from reporting the
        # peak of Z on the X channel, and nothing would look wrong.
        body = _model_symbol("fillTelemetryMessage")
        x = body.index("imu_peak_accel_x_ms2 = state.peakAccelMs2[0]")
        y = body.index("imu_peak_accel_y_ms2 = state.peakAccelMs2[1]")
        z = body.index("imu_peak_accel_z_ms2 = state.peakAccelMs2[2]")
        assert x < y < z
        assert "[3]" not in body.split("imu_peak_accel_x_ms2")[0].split("imu_calibration")[-1]

    def test_the_calibration_byte_is_widened_not_truncated(self, probe):
        assert "imu_calibration = (uint32_t)state.imuCalibration" in _model_symbol(
            "fillTelemetryMessage"
        )

    def test_the_frame_still_fits(self):
        # The budget that matters. pico_wire.h static_asserts the real constant at
        # build time, so this is the check that the prose above it has not gone
        # stale again -- it said 198 bytes with 58 spare for two changes.
        header = (FIRMWARE_DIR / "pico_wire.h").read_text()
        assert "rocsar_v1_PicoMessage_size <= PICO_TX_BUFFER" in header
        assert "#define PICO_TX_BUFFER 256" in header
        # And the documented size must be the generated one.
        pb = (FIRMWARE_DIR / "pico.pb.h").read_text()
        size = int(re.search(r"#define rocsar_v1_PicoMessage_size\s+(\d+)", pb).group(1))
        assert size <= 256, f"worst-case frame is {size} bytes"
        assert f"{size} bytes" in header, (
            "pico_wire.h documents a frame size that is not the generated one"
        )


# ============================================================================
# The sensor plumbing
# ============================================================================
class TestSensorPlumbing:
    """firmware.ino -- the part that needs hardware.

    Asserted by source shape because it cannot be driven: conftest.py compiles
    the model, not the sketch. Two of these exist because getting them wrong is
    silent.
    """

    def test_linear_acceleration_is_a_second_read_not_a_second_union_read(self):
        # The BNO055 fills the sensors_event_t union with ONE vector per call.
        # Asking for linear acceleration through the same read as the Euler
        # angles returns the Euler angles again, and peak-holding them produces
        # plausible numbers that are not accelerations.
        peak = _ino_symbol("readImuPeakAccel")
        assert "VECTOR_LINEARACCEL" in peak
        assert "VECTOR_EULER" not in peak

    def test_the_peak_read_guards_on_the_event_type(self):
        # Adafruit_BNO055 fills the SAME union member (event->acceleration) for
        # VECTOR_LINEARACCEL, VECTOR_ACCELEROMETER and VECTOR_GRAVITY and tells
        # them apart only by event->type. So the raw accelerometer would
        # peak-hold at 1 g on a level gondola and the peak would never mean a
        # shock -- with nothing about the numbers looking wrong.
        peak = _ino_symbol("readImuPeakAccel")
        assert "SENSOR_TYPE_LINEAR_ACCELERATION" in peak
        assert "linear.type !=" in peak

    def test_the_two_reads_are_separate_functions(self):
        assert "void readImuCalibration()" in INO
        assert "void readImuPeakAccel()" in INO
        assert "void readImuHeading(" in INO

    def test_tilt_comes_out_of_the_read_that_already_happens(self):
        # No extra sensor read for the other two Euler angles: they arrive in the
        # same getEvent() the heading used.
        heading = _ino_symbol("readImuHeading")
        assert "event.orientation.x" in heading
        assert "event.orientation.y" in heading
        assert "event.orientation.z" in heading
        assert "applyImuAttitude" in heading

    def test_the_second_reads_run_unconditionally(self):
        # Guarding them on imuPresent would leave a stale calibration byte and a
        # peak that silently stops updating when a sensor answers intermittently.
        loop = INO.split("void loop()")[1]
        assert "readImuCalibration()" in loop
        assert "readImuPeakAccel()" in loop

    def test_a_failed_peak_read_is_not_counted_as_a_sensor_miss(self):
        # The miss counter drives the re-probe and the wire's presence bit, both
        # of which are about the HEADING read. Counting a second read's failure
        # would declare a working sensor absent.
        peak = _ino_symbol("readImuPeakAccel")
        assert "noteImuMiss" not in peak
        assert "return" in peak

    def test_the_control_loop_never_polls_the_calibration_register_on_its_own(self):
        # One register read per tick, inside the tick. A second calibration read
        # anywhere else would be a second I2C transaction on the same bus the
        # servo status poll uses.
        assert INO.count("getCalibration(") == 1


def _ino_symbol(name: str) -> str:
    return strip_comments(_definition_body(INO, name))


def _model_symbol(name: str) -> str:
    return strip_comments(_definition_body(MODEL, name))


def _definition_body(text: str, name: str) -> str:
    for match in re.finditer(
        rf"^[\w:<>&*\[\] ,]*\b{name}\s*\([^;{{]*\)\s*\{{", text, re.MULTILINE
    ):
        start = match.end() - 1
        depth = 0
        for index in range(start, len(text)):
            if text[index] == "{":
                depth += 1
            elif text[index] == "}":
                depth -= 1
                if depth == 0:
                    return text[start + 1 : index]
    raise AssertionError(f"no definition of {name} found")


def strip_comments(text: str) -> str:
    text = re.sub(r"/\*.*?\*/", "", text, flags=re.S)
    return re.sub(r"//[^\n]*", "", text)
