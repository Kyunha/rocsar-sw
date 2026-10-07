package main

import (
	"fmt"
	"strings"
	"time"

	rocsarv1 "github.com/rocsar/obc/api/rocsar/v1"
	"github.com/rocsar/obc/internal/domain"
)

// This file is presentation and nothing else.
//
// It is the terminal's rendering of a telemetry frame. The equivalent for the
// window in cmd/gs is internal/gsview, which produces structs rather than text
// and is testable without a screen. Neither is the place where a value is
// decoded, validated or fetched -- that is internal/client, which both consoles
// share. What lives here is only the question of what a human reads.

// printTelemetry renders one frame as a fixed-shape block.
//
// Fixed shape on purpose: an operator watching a scrolling console wants the same
// lines every second so their eye finds the changed one. Proportional or
// conditional layout defeats that.
//
// Every value is qualified. A reading that is absent, held or simulated says so,
// because a bare number is indistinguishable from a real one -- and a console
// that renders a held value as a live one is the failure this project exists to
// prevent.
func printTelemetry(f *rocsarv1.TelemetryFrame) {
	s := f.System
	up := time.Duration(s.GetUptimeS()) * time.Second

	fmt.Printf("[seq %-6d] %s  uptime %s  %.0f C\n",
		f.Sequence, s.GetState(), up.Truncate(time.Second), s.GetCpuTempC())

	if m := s.GetMockedSubsystems(); len(m) > 0 {
		fmt.Printf("            SIMULATED: %s   <- fabricated, not measured\n", strings.Join(m, ", "))
	}
	if r := s.GetState(); r != rocsarv1.SubsystemState_SUBSYSTEM_READY {
		fmt.Printf("            system is %s\n", r)
	}

	for _, g := range f.Gnss {
		sel := " "
		if g.GetSelected() {
			sel = "*"
		}
		if !g.GetFixOk() {
			// Deliberately does not print the coordinates. A receiver with no fix
			// still carries the last values it decoded, and printing them next to
			// "no fix" invites reading the number instead of the qualifier.
			fmt.Printf("         gnss%s %d  no fix   <- not a position\n", sel, g.GetReceiverId())
			continue
		}
		// Climbrate, and the em dash when there is none.
		//
		// Optional on the wire because the estimator needs about a minute of
		// window before it will fit anything. Printing 0.00 m/s for that first
		// minute would assert the balloon is at float, which is the one thing it
		// is not; a balloon that really is at float prints a small number from the
		// same branch.
		climb := "        "
		if v := g.VerticalRateMps; v != nil {
			climb = fmt.Sprintf("%+6.2f m/s", *v)
		}
		fmt.Printf("         gnss%s %d  %10.6f, %11.6f  alt %7.1f m  climb %s  %5.1f m/s  crs %5.1f\n",
			sel, g.GetReceiverId(), g.GetLatitudeDeg(), g.GetLongitudeDeg(),
			g.GetAltitudeM(), climb, g.GetGroundSpeedMps(), g.GetCourseDeg())
	}

	if p := f.Pico; p != nil {
		conn := "NOT connected"
		if f.GetPicoConnected() {
			conn = "connected"
		}
		fmt.Printf("         pico   %s  heading %.2f (target %.2f)  imu %s",
			conn, p.GetGondolaHeadingDeg(), p.GetTargetHeadingDeg(),
			measuredOrHeld(p.GetImuPresent()))
		if p.GetImuPresent() {
			// Only meaningful as a measurement: otherwise it is held from the last
			// reading, and printing it beside HELD invites reading it as one.
			fmt.Printf("  imu temp %.1fC", p.GetImuTemperatureC())
		}

		// Tilt and calibration, guarded by presence for the same reason: a held
		// tilt beside a live bearing invites reading the two together when only the
		// bearing is current.
		//
		// Tilt is a confidence signal on the heading rather than attitude in its own
		// right. The BNO055 tilt-compensates its fusion using its accelerometer, and
		// a gondola on a 10-40 m tether swings at roughly 0.1 Hz, so sway corrupts
		// the estimate the heading correction depends on. Erratic tilt is the tell
		// that the bearing has gone bad.
		if p.GetImuPresent() {
			fmt.Printf("         imu    tilt roll %6.2f pitch %6.2f deg\n",
				p.GetGondolaRollDeg(), p.GetGondolaPitchDeg())

			// Calibration decoded from register 0x35: two bits per sensor, system
			// first. Only the magnetometer decides whether a heading means anything,
			// so it is the one called out by name.
			calib := p.GetImuCalibration()
			mag := calib & 3
			verdict := "mag UNCALIBRATED -- heading not trustworthy"
			if mag == 3 {
				verdict = "mag calibrated"
			}
			fmt.Printf("         imu    calib sys %d gyro %d accel %d mag %d  %s\n",
				(calib>>6)&3, (calib>>4)&3, (calib>>2)&3, mag, verdict)
		}

		// The peak never falls, so it is the flight's hardest moment rather than a
		// live reading, and the counter beside it says whether anything has happened
		// since the last frame -- which is the only question a 1 Hz link can answer
		// about a millisecond transient.
		//
		// Printed unconditionally, and that is deliberate. The peak of a sensor that
		// has since stopped answering is exactly what an operator wants afterwards,
		// and guarding it on presence would erase the record of whatever stopped it.
		peak := p.GetImuPeakAccelXMs2()
		if y := p.GetImuPeakAccelYMs2(); y > peak {
			peak = y
		}
		if z := p.GetImuPeakAccelZMs2(); z > peak {
			peak = z
		}
		fmt.Printf("         imu    accel peak %6.1f m/s^2 since boot (%d events)\n",
			peak, p.GetImuPeakAccelEvent())

		fmt.Printf("  heaters %s/%s\n",
			onOff(p.GetHeater1State()), onOff(p.GetHeater2State()))
		for _, a := range p.GetAntennas() {
			load := "load ?"
			if a.GetFeedbackState() == 1 {
				// Only meaningful when the servo actually answered. A held value is
				// the last reading or the command echo, and printing it as a
				// measurement is the thing this column exists to prevent.
				load = fmt.Sprintf("load %d%% %dC", a.GetLoad(), a.GetTemperatureC())
			} else {
				load = "load held"
			}
			fmt.Printf("                servo %d  tick %4d  %7.2f deg  %-12s  %s\n",
				a.GetServoId(), a.GetCurrentTick(), a.GetCurrentAngleDeg(),
				load, feedbackName(a.GetFeedbackState()))
			// The centre, and whether it is real. The servo holds its own zero
			// now, so an axis that was never taught reports the same 2048 as one
			// that was; printing the tick alone would present an assumption as a
			// measurement.
			if a.GetCenterZeroed() {
				fmt.Printf("                servo %d  centre %d (taught)\n",
					a.GetServoId(), a.GetCenterTick())
			} else {
				fmt.Printf("                servo %d  centre UNTAUGHT (assumed %d)\n",
					a.GetServoId(), a.GetCenterTick())
			}
			if e := a.GetFeedbackError(); e != 0 {
				// The ST3215's own status bits -- overheat, overload, undervoltage --
				// which it reports while answering perfectly well-formed frames, so
				// a servo in trouble reads clean unless this is looked at. Named with
				// domain.ServoFaults rather than the command ErrorCode, which called
				// an overheating servo ERROR_INVALID_COMMAND.
				fmt.Printf("                servo %d  FAULT %s\n", a.GetServoId(),
					strings.Join(domain.ServoFaults(e), "+"))
			}
		}
	}

	if c := f.Camera; c != nil {
		fmt.Printf("         camera %s  %d photo(s)", c.GetState(), c.GetPhotosTaken())
		if n := c.GetLastPhotoName(); n != "" {
			fmt.Printf("  last %s", n)
		}
		fmt.Println()
	}
	if d := f.Sdr; d != nil {
		line := fmt.Sprintf("         sdr    %s", d.GetState())
		if d.GetRunning() {
			line += fmt.Sprintf("  running pid %d", d.GetPid())
		}
		if e := d.GetLastError(); e != "" {
			line += fmt.Sprintf("  last error: %s", e)
		}
		fmt.Println(line)
	}
	if l := f.Link; l != nil {
		shaping := "shaping off"
		if l.GetShapingActive() {
			shaping = "shaping on"
		} else if r := l.GetInactiveReason(); r != "" {
			shaping = "shaping off: " + r
		}
		fmt.Printf("         link   %s  %s  %d kbit/s  %s\n",
			l.GetState(), l.GetDevice(), l.GetRateKbps(), shaping)
	}
	fmt.Println()
}

// measuredOrHeld distinguishes a reading from a bearing the flight controller is
// holding because the IMU has gone quiet. imu_present is the only thing that
// separates the two, and a heading shown without it is a guess.
func measuredOrHeld(present bool) string {
	if present {
		return "MEASURED"
	}
	return "HELD"
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// feedbackName renders feedback_state, which is an int32 on the wire rather than
// the enum, so that a value from a future firmware decodes to a plain integer
// instead of failing the whole frame. See api/rocsar/v1/common.proto and
// test/schema_test.go, which fails to compile if that ever becomes an enum.
//
// The unknown case is deliberate: a number the operator has never seen is more
// useful than a name we guessed.
func feedbackName(v int32) string {
	switch v {
	case 1:
		return "MEASURED"
	case 2:
		return "HELD"
	case 0:
		return "no reading"
	default:
		return fmt.Sprintf("state %d", v)
	}
}
