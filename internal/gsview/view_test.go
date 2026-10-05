package gsview

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rocsar/obc/internal/domain"
	"github.com/rocsar/obc/internal/telemetry"
)

// baseSnapshot is a fully-populated instant. Tests delete or degrade parts of
// it; nothing here tests the zero value, because the zero value is "the OBC
// never built a frame" and every field already renders that as absent.
func baseSnapshot() telemetry.Snapshot {
	return telemetry.Snapshot{
		Sequence:    4382,
		GeneratedAt: time.Date(2026, 10, 5, 0, 58, 0, 0, time.UTC),
		System: telemetry.SystemSnapshot{
			State:   domain.SubsystemReady,
			Uptime:  4382 * time.Second,
			CPUTemp: 50.6,
		},
		GNSS: []domain.ReceiverStatus{
			{
				ReceiverID: 1, FixOK: true, HasFix: true,
				Fix: domain.Fix{
					LatitudeDeg: 41.178102, LongitudeDeg: -8.594808,
					AltitudeM: 206.75, GroundSpeedMP: 0.015, CourseDeg: 0,
				},
				FixAge:   774 * time.Millisecond,
				Accepted: 109,
			},
			{ReceiverID: 2, Selected: true},
			{ReceiverID: 3},
		},
		Pico: domain.PicoTelemetry{
			GondolaHeadingDeg: 152.81, TargetHeadingDeg: 0,
			IMUPresent: true,
			Axes: []domain.Axis{
				{
					ServoID: 1, ManualMode: true, CurrentTick: 2002,
					CurrentAngleDeg: -4.04, Load: 2, TemperatureC: 37,
					CenterTick: 2048, MountOffsetDeg: 270, DirMultiplier: -1,
					FeedbackState: domain.FeedbackMeasured,
				},
				{
					ServoID: 2, ManualMode: true, CurrentTick: 1797,
					CurrentAngleDeg: -22.06, Load: 105, TemperatureC: 36,
					CenterTick: 2048, MountOffsetDeg: 270, DirMultiplier: -1,
					FeedbackState: domain.FeedbackMeasured,
				},
			},
		},
		HasPico:       true,
		PicoConnected: true,
		PicoAck:       &domain.Ack{CommandSequence: 5, Success: true, Error: domain.ErrNone},
		SDR:           telemetry.SDRSnapshot{State: domain.SubsystemDisconnected, PID: -1},
		Camera:        domain.SubsystemReady, CameraDevice: "/dev/video0",
		PhotosTaken: 2, PhotosTakenKnown: true,
		Link: domain.LinkStatus{
			State: domain.SubsystemReady, InactiveReason: "link shaping disabled by configuration",
		},
	}
}

// --- absence (§7.1): every field that can be absent renders as absent ---

func TestAbsentPicoRendersAsNoData(t *testing.T) {
	s := baseSnapshot()
	s.HasPico, s.PicoConnected = false, false
	s.Pico = domain.PicoTelemetry{}

	v := Build(s)
	if v.Pico != nil {
		t.Errorf("Pico rendered %+v with no flight controller; want nil (not heading 0)", v.Pico)
	}
	if v.PicoConnected {
		t.Error("link rendered connected with no flight controller; the block and the link are separate facts")
	}
}

func TestDisconnectedLinkRendersBesideLiveNumbers(t *testing.T) {
	s := baseSnapshot()
	s.PicoConnected = false

	v := Build(s)
	if v.Pico == nil {
		t.Fatal("remembered frame suppressed on disconnect; the numbers are still the best information available")
	}
	if v.PicoConnected {
		t.Error("link rendered connected while disconnected")
	}
}

func TestAbsentAckRendersAsNoAck(t *testing.T) {
	s := baseSnapshot()
	s.PicoAck = nil

	if v := Build(s); v.PicoAck != nil {
		t.Errorf("PicoAck rendered %+v with no acknowledgement; want nil", v.PicoAck)
	}
}

func TestFixNotOKSuppressesCoordinates(t *testing.T) {
	s := baseSnapshot()
	s.GNSS[0].FixOK = false

	got := Build(s).GNSS[0]
	if got.Position != nil {
		t.Errorf("position rendered %+v beside fix_ok=false; a receiver with no fix still carries its last decoded values", got.Position)
	}
	if got.FixAgeS == nil {
		t.Error("fix age suppressed alongside the position; 'last fix 0.8 s ago' and 'never' are different sentences")
	}
}

func TestNeverHadFixRendersAsNever(t *testing.T) {
	s := baseSnapshot()
	s.GNSS[0].HasFix, s.GNSS[0].FixOK = false, false
	s.GNSS[0].FixAge = 0

	got := Build(s).GNSS[0]
	if got.Position != nil {
		t.Errorf("position rendered for a receiver that never had a fix: %+v", got.Position)
	}
	if got.FixAgeS != nil {
		t.Errorf("fix age rendered %v for a receiver that never had a fix; want nil (never), distinct from age 0", *got.FixAgeS)
	}
}

func TestUnknownPhotoCountRendersAsDash(t *testing.T) {
	s := baseSnapshot()
	s.PhotosTaken, s.PhotosTakenKnown = 0, false

	if got := Build(s).Camera.PhotosTaken; got != nil {
		t.Errorf("photo count rendered %d for an unreported count; want nil (—, not 0 photos)", *got)
	}
}

func TestZeroPhotoCountRendersAsZero(t *testing.T) {
	s := baseSnapshot()
	s.PhotosTaken, s.PhotosTakenKnown = 0, true

	got := Build(s).Camera.PhotosTaken
	if got == nil || *got != 0 {
		t.Errorf("photo count rendered %v for a reported zero; want 0, distinct from unknown", got)
	}
}

func TestIMUAbsentRendersHeld(t *testing.T) {
	s := baseSnapshot()
	s.Pico.IMUPresent = false

	if got := Build(s).Pico.IMU; got != "HELD" {
		t.Errorf("imu rendered %q with imu_present=false; the heading is held, not measured", got)
	}
}

func TestIMUPresentRendersTemperature(t *testing.T) {
	s := baseSnapshot()
	s.Pico.ImuTemperatureC = 27.5

	got := Build(s).Pico.ImuTemperatureC
	if got == nil || *got != 27.5 {
		t.Errorf("imu temperature rendered %v for a present IMU; want 27.5, distinct from held", got)
	}
}

func TestIMUAbsentHidesTemperature(t *testing.T) {
	s := baseSnapshot()
	s.Pico.IMUPresent = false
	s.Pico.ImuTemperatureC = 27.5

	if got := Build(s).Pico.ImuTemperatureC; got != nil {
		t.Errorf("imu temperature rendered %v with imu_present=false; a held value is the last reading, not a measurement", *got)
	}
}

// --- feedback (§7.2): the int32 that must never print as a number ---

func TestHeldServoHidesLoadAndTemp(t *testing.T) {
	s := baseSnapshot()
	s.Pico.Axes[0].FeedbackState = domain.FeedbackHeld
	s.Pico.Axes[0].Load, s.Pico.Axes[0].TemperatureC = 42, 60

	got := Build(s).Pico.Antennas[0]
	if got.Feedback != "HELD" {
		t.Errorf("feedback rendered %q, want HELD", got.Feedback)
	}
	if got.Load != nil || got.TemperatureC != nil {
		t.Errorf("load/temp rendered for a HELD servo; a held value is the last reading or the command echo, not a measurement")
	}
}

func TestUnknownServoRendersNoReading(t *testing.T) {
	s := baseSnapshot()
	s.Pico.Axes[0].FeedbackState = domain.FeedbackUnknown

	got := Build(s).Pico.Antennas[0]
	if got.Feedback != "no reading" {
		t.Errorf("feedback rendered %q, want 'no reading'", got.Feedback)
	}
	if got.Load != nil || got.TemperatureC != nil {
		t.Error("load/temp rendered for a servo that never answered")
	}
}

func TestFutureFeedbackStateRendersHonestly(t *testing.T) {
	s := baseSnapshot()
	s.Pico.Axes[0].FeedbackState = domain.FeedbackState(7)

	if got := Build(s).Pico.Antennas[0].Feedback; got != "state 7" {
		t.Errorf("unknown feedback state rendered %q; a guessed name would be worse than the number", got)
	}
}

func TestMeasuredLoadOver100IsNotClamped(t *testing.T) {
	s := baseSnapshot() // axis 2 carries load 105, measured, from the live vehicle

	got := Build(s).Pico.Antennas[1]
	if got.Load == nil || *got.Load != 105 {
		t.Errorf("measured load rendered %v; clamping is interpretation, not rendering", got.Load)
	}
}

func TestServoFaultRendersOnlyWhenPresent(t *testing.T) {
	s := baseSnapshot()
	if got := Build(s).Pico.Antennas[0].FeedbackError; got != nil {
		t.Errorf("fault rendered %q for a servo reporting none; a servo in trouble must not read clean, and a healthy one must not read faulty", *got)
	}

	s.Pico.Axes[0].FeedbackError = int32(domain.ErrHardwareFault)
	got := Build(s).Pico.Antennas[0].FeedbackError
	if got == nil || *got == "" {
		t.Errorf("fault not rendered for a servo reporting one: %v", got)
	}
}

// --- banners and health ---

func TestMockedSubsystemsRenderForBanner(t *testing.T) {
	s := baseSnapshot()
	s.System.Mocked = []string{"pico", "camera"}

	if got := Build(s).System.Mocked; len(got) != 2 {
		t.Errorf("mocked rendered %v; telemetry built from mocks is visibly mock, always", got)
	}
}

func TestHealthyCarriesReasons(t *testing.T) {
	s := baseSnapshot()
	if v := Build(s); !v.Healthy || len(v.NotHealthyReasons) != 0 {
		t.Errorf("healthy snapshot rendered healthy=%v reasons=%v", v.Healthy, v.NotHealthyReasons)
	}

	s.System.State = domain.SubsystemError
	s.PicoConnected = false
	s.Camera = domain.SubsystemError
	s.SDR.State = domain.SubsystemError
	v := Build(s)
	if v.Healthy {
		t.Error("fully degraded snapshot rendered healthy")
	}
	if len(v.NotHealthyReasons) != 4 {
		t.Errorf("reasons = %v, want one per failed conjunct (system, pico, camera, sdr)", v.NotHealthyReasons)
	}
}

// --- things the view must not do (§7.3, §7.4) ---

func TestCoordinatesPassThroughUnconverted(t *testing.T) {
	s := baseSnapshot()

	got := Build(s).GNSS[0].Position
	if got == nil {
		t.Fatal("no position for a valid fix")
	}
	if got.LatitudeDeg != 41.178102 || got.LongitudeDeg != -8.594808 {
		t.Errorf("coordinates altered: %+v; degrees in, degrees out", got)
	}
}

func TestSDROmitsLastOutputFile(t *testing.T) {
	// last_output_file is declared on the wire and never populated by the
	// encoder. The view has deliberately no field for it, pinned here by
	// asserting on the serialised shape rather than on Go struct tags: if a
	// future encoder populates it, this fails and the field gets added --
	// together, in one commit, not one without the other.
	body, err := json.Marshal(Build(baseSnapshot()).SDR)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "last_output_file") {
		t.Errorf("SDR view carries last_output_file: %s", body)
	}
}
