"""The heater dead-man: a heater that is not being asked for must go off.

Two halves that only work as a pair. The firmware (this file) latches an "on"
for at most HEATER_AUTO_OFF_MS and then clears it, whatever anyone intended;
the host re-sends an acknowledged heater-on every HeaterRefreshInterval, which
keeps restarting that clock. Every way the host half can fail -- crash, cut
cable, reboot, a Ground Station that never came up -- leaves the firmware half
to do the turning off, which is the direction that is safe to fail in.

The host half lives in internal/command/heater_keeper.go and is not executable
here, so its side of the contract is asserted as text: the refresh interval
must fit at least twice into the firmware's window, or one lost refresh would
flicker a heater that is meant to be on.

Both halves are driven through the firmware's own applyCommand() and
expireHeaters() by the probe, because the policy lives in gondola_model.h and
the sketch's job is only to call it at the right moment -- which is what the
structural tests at the bottom pin.
"""

from __future__ import annotations

import re

from conftest import FIRMWARE_DIR, run

SKETCH = FIRMWARE_DIR / "firmware.ino"
MODEL = FIRMWARE_DIR / "gondola_model.h"
KEEPER = FIRMWARE_DIR.parent / "internal" / "command" / "heater_keeper.go"

#: The firmware's window in milliseconds, as written in the model. Named here
#: so a change to HEATER_AUTO_OFF_MS fails a test that says what broke instead
#: of a heuristic drifting with it.
AUTO_OFF_MS = 20_000


def _heater_lifetime(probe, *steps: str) -> list[dict[str, int]]:
    """Drive the dead-man timeline; one dict per step."""
    out = run(probe, "--heater-lifetime", *steps)
    frames = []
    for line in out.splitlines():
        assert line.startswith("HEATER "), line
        frames.append(
            {key: int(value) for key, value in re.findall(r"(\w+)=(\d+)", line)}
        )
    assert len(frames) == len(steps), (
        "every step must report the state it leaves behind, or the timeline is "
        "unreadable after the first difference"
    )
    return frames


class TestTheFirmwareExpiresOnItsOwn:
    def test_a_heater_goes_off_twenty_seconds_after_its_last_on(self, probe):
        """The window, from both sides of it.

        19999 ms in, the heater is still on: expiring early would drop a
        heater the host is actively keeping alive. At 20000 ms it is off, with
        no further command -- which is the whole point.
        """
        frames = _heater_lifetime(probe, "t=0", "on1", "t=19999", "t=20000")
        assert frames[1]["h1"] == 1, "an on command must latch"
        assert frames[2]["h1"] == 1, "one millisecond inside the window is still on"
        assert frames[3]["h1"] == 0, "and the boundary itself expires"

    def test_a_refresh_inside_the_window_extends_it(self, probe):
        """What the host's keep-alive buys: 10 s in, another 20 s of life.

        The expiry therefore moves from t=20000 to t=30000 -- not to 40000:
        a refresh restarts the window, it does not stack on the old one.
        """
        frames = _heater_lifetime(
            probe, "t=0", "on1", "t=10000", "on1", "t=29999", "t=30000"
        )
        assert frames[4]["h1"] == 1, "still inside the refreshed window"
        assert frames[5]["h1"] == 0, "the refreshed window ends 20 s after the refresh"

    def test_an_off_is_immediate_and_not_an_expiry(self, probe):
        """An operator who asks for off gets off now, not in 20 seconds."""
        frames = _heater_lifetime(probe, "t=0", "on1", "t=1000", "off1")
        assert frames[2]["h1"] == 1, "on at t=1000, before the off is applied"
        assert frames[3]["h1"] == 0, "the off lands in the same step"

    def test_the_two_heaters_keep_separate_clocks(self, probe):
        """One window lapsing must not touch the other heater's."""
        frames = _heater_lifetime(
            probe, "t=0", "on1", "t=5000", "on2", "t=20000", "t=25000"
        )
        at_boundary, after = frames[4], frames[5]
        assert (at_boundary["h1"], at_boundary["h2"]) == (0, 1), (
            "heater 1 has had its 20 s; heater 2 was refreshed 15 s ago"
        )
        assert (after["h1"], after["h2"]) == (0, 0), "heater 2's window lapses 5 s later"

    def test_an_off_command_can_never_turn_a_heater_on(self, probe):
        """The dead-man may only ever cool. A stray off at any time is inert."""
        frames = _heater_lifetime(probe, "t=0", "off1", "t=1", "off2")
        assert all(frame["h1"] == 0 and frame["h2"] == 0 for frame in frames)


class TestTheTwoHalvesAgree:
    def test_the_host_refreshes_at_least_twice_per_firmware_window(self):
        """The pairing, asserted where neither half can drift alone.

        Two refreshes must fit in one window so that a single lost refresh
        does not flicker a heater that is on; one refresh would leave the
        system one scheduling hiccup away from an unintended cooldown.
        """
        keeper = KEEPER.read_text()
        match = re.search(r"HeaterRefreshInterval = (\d+) \* time\.Second", keeper)
        assert match, (
            "heater_keeper.go no longer declares its refresh interval as a "
            "named constant, so the pairing with the firmware has no home"
        )
        refresh_ms = int(match.group(1)) * 1000

        model = MODEL.read_text()
        window = re.search(r"#define HEATER_AUTO_OFF_MS (\d+)", model)
        assert window, "gondola_model.h no longer names the firmware window"
        assert int(window.group(1)) == AUTO_OFF_MS, (
            "the firmware window changed; this test's expectation is the "
            "documented 20 s, so update it deliberately"
        )
        assert 2 * refresh_ms <= AUTO_OFF_MS, (
            f"a {refresh_ms} ms refresh no longer fits twice into the "
            f"{AUTO_OFF_MS} ms window: one lost keep-alive would flicker the heater"
        )

    def test_the_sketch_expires_the_model_and_then_follows_it_to_the_pins(self):
        """The sketch's whole job here: call both, in the control tick.

        If expireHeaters() is not called every tick the firmware half does not
        exist (only commands would expire things); if syncHeaterPins() is not
        called after it, the flags change and the heater stays hot.
        """
        ino = SKETCH.read_text()
        assert "expireHeaters(gondola, now)" in ino
        # After the IMU read, before telemetry: the frame this tick must already
        # report an expiry that happened this tick.
        assert ino.index("expireHeaters(gondola, now)") < ino.index("sendTelemetryMessage();")
        assert "syncHeaterPins();" in ino

    def test_nothing_writes_a_heater_pin_except_sync_heater_pins(self):
        """One writer, or the dead-man can be bypassed by a stray digitalWrite.

        The model flags are cleared correctly in a dozen places; if any of them
        wrote the pin directly, the flag and the heater would disagree and the
        telemetry (which reads the flag) would report the lie.
        """
        ino = SKETCH.read_text()
        assert "void syncHeaterPins()" in ino
        writes = re.findall(r"digitalWrite\(HEATER_\d_PIN", ino)
        assert len(writes) == 2, (
            "both heater pins must be written only inside syncHeaterPins(); "
            f"found {len(writes)} digitalWrite calls on them"
        )

    def test_a_heater_command_carries_the_clock_into_the_model(self):
        """`nowMs` is the dead-man's only source of time.

        Without it the stamp would be set from some other clock (or none), and
        HEATER_AUTO_OFF_MS would measure the wrong thing while looking correct.
        """
        assert "applyCommand(gondola, cmd, millis())" in SKETCH.read_text()
        model = MODEL.read_text()
        assert "unsigned long nowMs" in model
        assert "noteHeaterCommand(state, heaterId" in model.replace("\n", " ")
