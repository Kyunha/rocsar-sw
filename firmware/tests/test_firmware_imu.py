"""The heading policy: an absent IMU must not become a command.

This is the test that could not exist before the policy was lifted out of the
sketch. The heading integration used to be inline in the control loop, and the
probe TU compiles only `gondola_model.h` and `pico_wire.h` -- so an unguarded
`bno.getEvent()` on an uninitialised `sensors_event_t` passed the entire suite
while driving the servos from stack memory every 20 ms. The loop now calls
`applyImuHeading()`, so the absence case, the rejected-sample case, and the seam
guard are all drivable from the host.
"""

from __future__ import annotations

import math
import re

import pytest
from conftest import FIRMWARE_DIR, run

SKETCH = FIRMWARE_DIR / "firmware.ino"


def _heading(probe, *steps: str) -> list[dict[str, object]]:
    """Run samples through the firmware's real heading policy.

    Each step is a number (a real sample), ``none`` (no sensor fitted), or
    ``nan``/``inf`` (a sensor that failed to deliver).
    """
    steps_out: list[dict[str, object]] = []
    for line in run(probe, "--heading", *steps).splitlines():
        if not line.startswith("HEADING "):
            continue
        outcome, imu, heading = line[len("HEADING ") :].split()
        steps_out.append(
            {
                "outcome": outcome,
                "imu": imu == "imu=1",
                "heading": float(heading.removeprefix("heading=")),
            }
        )
    return steps_out


def _target_tick(probe, heading: str, target: str) -> int:
    return int(run(probe, "--target-tick", heading, target).split()[1])


class TestAbsentSensor:
    def test_no_sensor_holds_the_bearing(self, probe):
        """The original bug: an absent sensor changed the heading.

        With no BNO055 fitted the old loop integrated an uninitialised
        `sensors_event_t`; the bearing moved to garbage and the servos followed
        it. Holding is the correct posture -- it is what this firmware already
        does for a lost Pi link.
        """
        steps = _heading(probe, "none", "none", "none")
        assert [step["outcome"] for step in steps] == ["NO_SENSOR"] * 3
        assert all(step["heading"] == 0.0 for step in steps), (
            "an absent IMU must not move the heading"
        )

    def test_absence_is_reported(self, probe):
        """A held bearing has to be distinguishable from a reading.

        Without `imu` reaching the wire, the GUI would render a frozen heading as
        a live one -- the same placeholder-as-measurement trap the model tests
        cover on the host side.
        """
        assert _heading(probe, "none")[0]["imu"] is False

    def test_a_known_bearing_survives_the_absence(self, probe):
        """The hold is of the *last* bearing, not a reset to zero.

        Resetting would swing the antenna on the very loss of the reference it
        was being asked to hold.
        """
        steps = _heading(probe, "90", "none", "none")
        held = steps[0]["heading"]
        assert held != 0.0
        assert steps[1]["heading"] == pytest.approx(held)
        assert steps[2]["heading"] == pytest.approx(held)


class TestReconnectingSensor:
    def test_a_sensor_appearing_later_is_picked_up(self, probe):
        """A bench Pico may have an IMU fitted after boot; no reflash needed."""
        steps = _heading(probe, "none", "none", "45")
        assert steps[0]["imu"] is False
        assert steps[2]["imu"] is True
        assert steps[2]["outcome"] == "APPLIED"
        assert steps[2]["heading"] > 0.0

    def test_a_sensor_that_disappears_stops_being_reported_present(self, probe):
        steps = _heading(probe, "45", "none")
        assert steps[1]["imu"] is False
        assert steps[1]["outcome"] == "NO_SENSOR"


class TestRejectedSamples:
    def test_nan_is_rejected_and_does_not_move_the_bearing(self, probe):
        """A present sensor that fails still must not poison the heading.

        Integrating a NaN would persist: every later sample would compute
        `wrap180(NaN - NaN)` and there is no recovery short of re-initialising
        the state.
        """
        steps = _heading(probe, "90", "nan")
        before = steps[0]["heading"]
        assert steps[1]["outcome"] == "REJECTED"
        assert steps[1]["heading"] == pytest.approx(before)
        assert math.isfinite(steps[1]["heading"])

    def test_infinity_is_rejected(self, probe):
        steps = _heading(probe, "90", "inf")
        assert steps[1]["outcome"] == "REJECTED"
        assert math.isfinite(steps[1]["heading"])

    def test_a_nan_never_reaches_the_servo(self, probe):
        """The end of the chain: the commanded tick stays in range.

        This is the seam the old code had no guard on -- `calculateTargetTick`
        cast a float to int32_t, which is undefined behaviour on a non-finite
        value on this target.
        """
        steps = _heading(probe, "90", "nan")
        assert math.isfinite(steps[1]["heading"])


class TestTickSeam:
    def test_a_finite_geometry_still_commands_a_tick(self, probe):
        """The guard must not swallow legitimate commands.

        10 deg of antenna-relative error through a 5:1 gear is -568 ticks off
        centre, i.e. 1480 -- strictly interior, and different from the 2048 the
        guard returns, so this fails if the seam ever short-circuits a real
        command.
        """
        assert _target_tick(probe, "10", "290") == 1480

    def test_an_unreachable_geometry_saturates_rather_than_wrapping(self, probe):
        """A large error clamps at the rail. It is not a fault in the guard.

        90 - 10 is 170 deg of antenna-relative angle, which a 5:1 gear turns
        into ~9661 ticks against a 4096-tick servo. `clampTick` pins it to 0.
        This is the same saturation the real axis has always had, so it is
        pinned here to distinguish "saturated" from "not computed" -- note that
        0 is also what an uninitialised tick looks like, which is exactly the
        ambiguity the guard above removes for the non-finite case.

        NOTE: this pins the CURRENT behaviour, which is that saturation is
        silent. Nothing on the wire distinguishes this tick from a servo that
        arrived at the rail legitimately. Reporting that is the change these
        tests are meant to be updated against, not a bug to fix here.
        """
        assert _target_tick(probe, "10", "90") == 0

    def test_a_nan_heading_centres_instead_of_commanding_garbage(self, probe):
        tick = _target_tick(probe, "nan", "90")
        assert tick == 2048, "the axis must centre, not be sent a garbage tick"

    def test_a_nan_target_centres(self, probe):
        assert _target_tick(probe, "10", "nan") == 2048

    def test_an_infinite_heading_centres(self, probe):
        assert _target_tick(probe, "inf", "90") == 2048


class TestDroppedTicks:
    def test_a_fitted_sensor_that_delivers_nothing_holds_the_bearing(self, probe):
        """"No sample" must not be folded in as a sample of zero.

        This is the trap in the seam. The first version of the sketch's
        getEvent() failure path called `applyImuHeading(gondola, 0.0f, true)`,
        and 0.0f is *finite* -- so it sailed past the guard and integrated a
        reading of zero, dragging the bearing toward north on every dropped
        tick. A silently wrong heading is worse than the uninitialised read it
        replaced, because it looks plausible.
        """
        steps = _heading(probe, "90", "nodata", "nodata")
        held = float(steps[0]["heading"])
        assert held > 0.0
        assert [step["outcome"] for step in steps[1:]] == ["NO_DATA", "NO_DATA"]
        assert steps[1]["heading"] == pytest.approx(held)
        assert steps[2]["heading"] == pytest.approx(held)

    def test_a_dropped_tick_does_not_clear_the_presence_flag(self, probe):
        """The sensor is fitted; it just did not answer. That is not the same as
        having no IMU, and reporting it as absent would send an operator looking
        for a part that is plugged in."""
        steps = _heading(probe, "90", "nodata")
        assert steps[1]["imu"] is True


class TestTheSketchPlumbsTheSensor:
    """The probe cannot see the `.ino`, so the one regression it structurally
    cannot catch is asserted as text.

    `TestDroppedTicks` above models a dropped tick as a no-op, because that is
    what the sketch is supposed to do. That makes it a description of the intent
    rather than a check of the code -- if someone reinstates
    `applyImuHeading(gondola, 0.0f, true)` in the sketch, every behavioural test
    here still passes. These assertions read the sketch instead, which is the
    same technique `test_firmware_wire_format.py` uses for the receive buffer,
    and for the same reason: it is the only place the sketch's own body is
    visible to the suite.
    """

    def _sketch_code(self) -> str:
        """The sketch with `//` comments stripped.

        Necessary, not cosmetic: the comments around this code explain *why* the
        calls must not be made, so they contain the very text the assertions
        below look for. Matching them would make the trap unsatisfiable and would
        quietly reward deleting the explanation.
        """
        return "\n".join(line.split("//")[0] for line in SKETCH.read_text().splitlines())

    def test_a_dropped_tick_does_not_integrate_a_zero_sample(self):
        assert "applyImuHeading(gondola, 0.0f, true)" not in self._sketch_code(), (
            "a getEvent() failure must hold, not integrate 0.0f -- a zero sample "
            "is finite, so it passes the guard and drags the bearing toward north"
        )

    def test_the_sensor_read_is_guarded(self):
        """`getEvent()`'s return value was the original defect."""
        assert "if (!bno.getEvent(" in self._sketch_code(), (
            "the sketch must branch on getEvent()'s success before using the event"
        )

    def test_the_heading_policy_is_not_inlined_in_the_loop(self):
        """The reason this file is possible at all.

        If this fails, the fix has been undone by moving the policy back into the
        sketch where nothing can test it -- and nothing else in the suite will
        notice, which is precisely how the unguarded read survived the first
        time.
        """
        code = self._sketch_code()
        assert "IMU_ALPHA" not in code, (
            "the filter constant belongs to gondola_model.h; the sketch must "
            "call applyImuHeading(), not re-implement the integration"
        )
        assert "readImuHeading(now)" in code, "the loop must delegate to readImuHeading()"

    def test_the_re_probe_is_wired_to_the_model_state(self):
        """Presence has one home. A parallel sketch-local flag could disagree
        with the state it is supposed to describe, and the telemetry reads the
        state."""
        code = self._sketch_code()
        assert "gondola.imuPresent" in code
        assert not re.search(r"^\s*bool imuPresent\s*=", code, re.MULTILINE), (
            "presence must live in GondolaState, not in a sketch-local variable"
        )


class TestHeadingIntegration:
    def test_a_real_sample_moves_the_heading_toward_itself(self, probe):
        steps = _heading(probe, "0", "90", "90", "90", "90", "90", "90")
        headings = [float(step["heading"]) for step in steps]
        # strict=False is correct, not a waiver: the shifted list is one shorter
        # by construction.
        assert all(
            later > earlier for earlier, later in zip(headings, headings[1:], strict=False)
        ), f"the EMA should climb monotonically toward the sample: {headings}"

    def test_the_heading_stays_wrapped(self, probe):
        """Sustained samples across the 360/0 seam must not run away.

        The filter is an EMA on a wrapped delta, so it converges without ever
        leaving [0, 360); a heading that exceeded the range would put the
        antenna base into `wrap360` twice and drift.
        """
        steps = _heading(probe, *(["350"] * 8))
        for step in steps:
            assert 0.0 <= float(step["heading"]) <= 360.0