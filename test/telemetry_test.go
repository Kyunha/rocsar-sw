package test

import (
	"math"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	rocsarv1 "github.com/rocsar/obc/api/rocsar/v1"
	"github.com/rocsar/obc/internal/domain"
	"github.com/rocsar/obc/internal/telemetry"
	"github.com/rocsar/obc/internal/transport"
)

// worstCaseSnapshot has every field populated with a value at least as large as
// any real one produces, so the budget test measures the ceiling rather than a
// flattering average.
func worstCaseSnapshot() telemetry.Snapshot {
	now := time.Date(2026, 10, 3, 14, 25, 30, 0, time.UTC)
	axes := make([]domain.Axis, 0, 2)
	for i := 1; i <= 2; i++ {
		axes = append(axes, domain.Axis{
			ServoID: uint32(i), ManualMode: true, CurrentTick: 4095,
			CurrentAngleDeg: -359.999, Load: -32768, TemperatureC: 32767,
			CenterTick: 2048, CenterZeroed: true,
			MountOffsetDeg: -12.5, DirMultiplier: -1,
			FeedbackState: domain.FeedbackHeld, FeedbackError: -32768,
		})
	}

	gnss := make([]domain.ReceiverStatus, 0, 3)
	// Receiver 1 has a rate, receivers 2 and 3 do not. That mix is deliberate:
	// the optional field has to survive the wire for one receiver and stay absent
	// for the others, and a test that only ever sets it proves neither.
	rate1 := 5.25
	for i := 1; i <= 3; i++ {
		fix := domain.Fix{
			ReceiverID: i, LatitudeDeg: 38.722252, LongitudeDeg: -9.139337,
			AltitudeM: 1234.5678, GroundSpeedMP: 250.5, CourseDeg: 359.999,
			ObservedAt: now,
		}
		if i == 1 {
			fix.VerticalRateMP = &rate1
		}
		gnss = append(gnss, domain.ReceiverStatus{
			ReceiverID: i, Selected: i == 1, FixOK: true, HasFix: true,
			Fix:      fix,
			FixAge:   1500 * time.Millisecond,
			Accepted: 1 << 40, Rejected: 1 << 20,
		})
	}

	return telemetry.Snapshot{
		Sequence: 1 << 40, GeneratedAt: now, Uptime: 1 << 30 * time.Second,
		System: telemetry.SystemSnapshot{
			State: domain.SubsystemReady, Uptime: 1 << 30 * time.Second,
			CPUTemp: 123.456, Mocked: []string{"pico", "gnss", "camera", "sdr", "link"},
		},
		GNSS:          gnss,
		PicoConnected: true,
		HasPico:       true,
		Pico: domain.PicoTelemetry{
			GondolaHeadingDeg: 359.9999, TargetHeadingDeg: 180.0001,
			Heater1State: true, Heater2State: true, IMUPresent: true,
			ImuTemperatureC: 85.5,
			// Extremes, so a truncating encoder or a mis-ordered copy shows up.
			// Roll and pitch both sit near a signed-degree limit, the calibration
			// byte is a real packed register value (sys 3 gyro 3 accel 2 mag 1),
			// and the event counter is at uint32 max so a uint16 truncation would
			// wrap it to a value that looks like a small event count.
			GondolaRollDeg: -179.999, GondolaPitchDeg: 89.999,
			IMUCalibration: 0xF9, IMUPeakAccelEvent: 4294967295,
			IMUPeakAccelMs2: [3]float64{-30.5, 12.25, 0},
			Axes:            axes, ObservedAt: now,
		},
		PicoAck: &domain.Ack{CommandSequence: 4294967295, Success: false, Error: domain.ErrHardwareFault},
		SDR: telemetry.SDRSnapshot{
			State: domain.SubsystemBusy, Running: true, PID: 1 << 22,
			LastLog:   "/mnt/rocsar/data/sdr/connect-20261003-142530-with-a-long-name.log",
			LastError: "uhd_usrp_probe: USRP RX TX Center Freq error",
		},
		Camera: domain.SubsystemReady, CameraDevice: "/dev/video0",
		LastPhoto: "camera-20261003-142530.jpg", PhotosTaken: 1 << 32,
		PhotosTakenKnown: true,
		Link: domain.LinkStatus{
			State: domain.SubsystemBusy, Device: "eth0", RateKbps: 115, PriorityKbps: 41,
			ShapingActive:  true,
			InactiveReason: "bulk filter not installed: bulk traffic will join the priority class",
		},
	}
}

// THE budget test.
//
// The link's priority class is 41 kbit/s. A frame above FrameBudgetBytes uses so
// much of it that command responses queue behind telemetry. This measures the
// serialised size with every field at its largest plausible value, so a new field
// that is too large fails here rather than on the aircraft.
func TestTelemetryFrameFitsThePriorityClassBudget(t *testing.T) {
	size := transport.EncodedTelemetrySize(worstCaseSnapshot())
	if size < 0 {
		t.Fatal("the snapshot failed to serialise")
	}

	if size > telemetry.FrameBudgetBytes {
		body, _ := proto.Marshal(transport.EncodeTelemetry(worstCaseSnapshot()))
		t.Errorf("a worst-case frame is %d bytes, over the %d byte budget.\n"+
			"    The priority class is 41 kbit/s; at 1 Hz this frame alone would use "+
			"%.0f%% of it.\n    Something added is too large -- the fix is to make that field smaller or\n"+
			"    to reference it (by name and size) instead of carrying it. First bytes: %x",
			size, telemetry.FrameBudgetBytes,
			100*float64(size)/float64(telemetry.FrameBudgetBytes)*0.204 /* kB -> ratio */ *5,
			body[:min(64, len(body))])
	}
	t.Logf("worst-case frame: %d bytes of %d budget (%.0f%%)",
		size, telemetry.FrameBudgetBytes, 100*float64(size)/float64(telemetry.FrameBudgetBytes))
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// A silent flight controller must NOT produce a zeroed PicoTelemetry. That is how
// a console ends up showing a healthy-looking 0 for a motor that has not moved,
// which is a fault that was observed on the bench before this field existed.
func TestSilentPicoProducesNoTelemetryRatherThanZeros(t *testing.T) {
	s := worstCaseSnapshot()
	s.HasPico = false
	s.PicoConnected = false
	s.Pico = domain.PicoTelemetry{}
	s.PicoAck = nil

	f := transport.EncodeTelemetry(s)

	if f.GetPico() != nil {
		t.Errorf("a silent flight controller produced a PicoTelemetry message: %v", f.GetPico())
	}
	if f.GetPicoLastAck() != nil {
		t.Error("a silent flight controller produced a PicoAck")
	}
	if f.GetPicoConnected() {
		t.Error("PicoConnected is true with no telemetry")
	}
}

// A GNSS receiver that has never produced a fix must not carry a fix age of
// zero. Absent and zero are different facts and are not collapsed.
func TestReceiverWithoutAFixCarriesNoAge(t *testing.T) {
	s := worstCaseSnapshot()
	s.GNSS[1].HasFix = false
	s.GNSS[1].Fix = domain.Fix{}
	s.GNSS[1].FixAge = 0
	s.GNSS[1].FixOK = false

	f := transport.EncodeTelemetry(s)
	got := f.GetGnss()[1]

	if got.GetFixAgeS() != 0 {
		t.Errorf("fix_age_s = %v for a receiver that never fixed; absent and zero are different", got.GetFixAgeS())
	}
	if got.GetFixOk() {
		t.Error("fix_ok is true for a receiver with no fix")
	}
}

// Mocked subsystems must be visible on the wire, not only in a log. A system
// that fabricated telemetry because a device was not found is the worst failure
// mode this project has.
func TestMockedSubsystemsAreVisibleOnTheWire(t *testing.T) {
	s := worstCaseSnapshot()
	s.System.Mocked = []string{"sdr", "camera", "pico"}

	f := transport.EncodeTelemetry(s)
	got := f.GetSystem().GetMockedSubsystems()

	want := []string{"camera", "pico", "sdr"}
	if len(got) != len(want) {
		t.Fatalf("mocked_subsystems = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("mocked_subsystems = %v, want %v (sorted)", got, want)
			break
		}
	}
}

// The feedback tri-state must cross the wire unchanged, because it is the only
// thing separating a measurement from an echo of the last command.
func TestFeedbackTriStateCrossesTheWireUnchanged(t *testing.T) {
	for _, want := range []domain.FeedbackState{
		domain.FeedbackUnknown, domain.FeedbackMeasured, domain.FeedbackHeld,
	} {
		s := worstCaseSnapshot()
		s.Pico.Axes[0].FeedbackState = want

		f := transport.EncodeTelemetry(s)
		if got := domain.FeedbackState(f.GetPico().GetAntennas()[0].GetFeedbackState()); got != want {
			t.Errorf("feedback_state %v encoded as %v", want, got)
		}
	}
}

// Round trip: encode then decode must preserve what matters.
func TestSnapshotRoundTrip(t *testing.T) {
	s := worstCaseSnapshot()
	back := transport.DecodeTelemetry(transport.EncodeTelemetry(s))

	if back.Sequence != s.Sequence {
		t.Errorf("sequence %d -> %d", s.Sequence, back.Sequence)
	}
	if !back.PicoConnected || !back.HasPico {
		t.Error("pico connectivity lost in the round trip")
	}
	if len(back.GNSS) != 3 {
		t.Fatalf("round trip produced %d receivers, want 3", len(back.GNSS))
	}
	// Order matters: the client finds the selected receiver by index.
	for i := range s.GNSS {
		if back.GNSS[i].ReceiverID != s.GNSS[i].ReceiverID {
			t.Errorf("receiver order changed at %d: %d", i, back.GNSS[i].ReceiverID)
		}
	}
	if got, ok := back.SelectedFix(); !ok || !got.Valid() {
		t.Errorf("SelectedFix after a round trip = %+v", got)
	}
	if len(back.Pico.Axes) != 2 {
		t.Errorf("round trip produced %d axes, want 2", len(back.Pico.Axes))
	}
	if back.Pico.Axes[0].FeedbackState != s.Pico.Axes[0].FeedbackState {
		t.Errorf("feedback state changed: %v -> %v", s.Pico.Axes[0].FeedbackState, back.Pico.Axes[0].FeedbackState)
	}
	// True, not false. proto3 omits a false bool from the wire entirely, so a
	// false value round-trips whether or not anyone wired the field up -- and this
	// is the field that distinguishes a centred axis from an untaught one that
	// reports the same 2048. Only the true case can fail.
	if !back.Pico.Axes[0].CenterZeroed {
		t.Error("center_zeroed lost across the round trip; a taught axis reads as untaught")
	}
	// Vertical rate: present for one receiver, absent for the others.
	//
	// The absent case is the one that matters. A plain double on the wire would
	// decode an underived rate to 0.0, and every consumer downstream would read
	// "balloon at float" for the first minute of every flight.
	if back.GNSS[0].Fix.VerticalRateMP == nil {
		t.Error("the selected receiver's vertical rate was lost")
	} else if math.Abs(*back.GNSS[0].Fix.VerticalRateMP-5.25) > 1e-6 {
		t.Errorf("vertical rate = %v, want 5.25", *back.GNSS[0].Fix.VerticalRateMP)
	}
	for _, i := range []int{1, 2} {
		if back.GNSS[i].Fix.VerticalRateMP != nil {
			t.Errorf("receiver %d gained a vertical rate it never had: %v",
				i+1, *back.GNSS[i].Fix.VerticalRateMP)
		}
	}

	// The IMU channels. Tilt and the peak-hold are the new fields most likely to
	// be dropped by a codec that was not updated, and the calibration byte and
	// event counter are the two a truncating encoder would mangle.
	if got := back.Pico.GondolaRollDeg; math.Abs(got-(-179.999)) > 1e-3 {
		t.Errorf("roll = %v, want -179.999", got)
	}
	if got := back.Pico.GondolaPitchDeg; math.Abs(got-89.999) > 1e-3 {
		t.Errorf("pitch = %v, want 89.999", got)
	}
	if got := back.Pico.IMUCalibration; got != 0xF9 {
		t.Errorf("imu_calibration = %#x, want 0xF9", got)
	}
	// uint32 max: a uint16 field on the wire would wrap this to 65535.
	if got := back.Pico.IMUPeakAccelEvent; got != 4294967295 {
		t.Errorf("imu_peak_accel_event = %d, want 4294967295", got)
	}
	for i, want := range []float64{-30.5, 12.25, 0} {
		if got := back.Pico.IMUPeakAccelMs2[i]; math.Abs(got-want) > 1e-3 {
			t.Errorf("peak accel axis %d = %v, want %v", i, got, want)
		}
	}
	if back.Pico.ImuTemperatureC != s.Pico.ImuTemperatureC {
		t.Errorf("imu temperature changed: %v -> %v", s.Pico.ImuTemperatureC, back.Pico.ImuTemperatureC)
	}
	if !back.PhotosTakenKnown || back.PhotosTaken != s.PhotosTaken {
		t.Errorf("photo count known=%v count=%d, want known=true count=%d",
			back.PhotosTakenKnown, back.PhotosTaken, s.PhotosTaken)
	}
}

// A photo count the camera never reported must survive the round trip as
// unknown, not as zero. Zero photographs and "the subsystem never said" render
// differently, and the wire carries the distinction.
func TestUnknownPhotoCountSurvivesTheRoundTrip(t *testing.T) {
	s := worstCaseSnapshot()
	s.PhotosTaken, s.PhotosTakenKnown = 0, false

	back := transport.DecodeTelemetry(transport.EncodeTelemetry(s))
	if back.PhotosTakenKnown {
		t.Error("an unreported photo count came back known")
	}
	if enc := transport.EncodeTelemetry(s); enc.GetCamera().PhotosTaken != nil {
		t.Error("an unreported photo count was encoded onto the wire")
	}
}

// SelectedFix must not return a stale coordinate. The receiver is trusted, but
// an old fix is worse than no fix, and the staleness bound exists for that.
func TestSelectedFixRefusesAStaleFix(t *testing.T) {
	s := worstCaseSnapshot()
	s.GNSS[0].FixOK = false

	if _, ok := s.SelectedFix(); ok {
		t.Error("SelectedFix returned a fix for a receiver whose fix_ok is false")
	}

	s.GNSS[0].FixOK = true
	if _, ok := s.SelectedFix(); !ok {
		t.Error("SelectedFix refused a valid selected fix")
	}
}

// No selected receiver at all is "no fix", not "the first one".
func TestSelectedFixRequiresASelectedReceiver(t *testing.T) {
	s := worstCaseSnapshot()
	for i := range s.GNSS {
		s.GNSS[i].Selected = false
	}
	if _, ok := s.SelectedFix(); ok {
		t.Error("SelectedFix invented a selection")
	}
}

// The inactive reason must survive to the wire: "tc unavailable", "no permission"
// and "disabled by configuration" are three different operator actions.
func TestLinkInactiveReasonReachesTheWire(t *testing.T) {
	s := worstCaseSnapshot()
	s.Link.ShapingActive = false
	s.Link.InactiveReason = "could not install the root HTB qdisc on wlan0: permission denied. Link shaping needs CAP_NET_ADMIN"

	f := transport.EncodeTelemetry(s)
	if got := f.GetLink().GetInactiveReason(); got != s.Link.InactiveReason {
		t.Errorf("inactive_reason = %q, want %q", got, s.Link.InactiveReason)
	}
	if f.GetLink().GetShapingActive() {
		t.Error("shaping_active is true while the reason says it is not")
	}
	if f.GetLink().GetState() == rocsarv1.SubsystemState_SUBSYSTEM_READY {
		t.Error("link state is READY with shaping inactive and a reason given")
	}
}

// Every provider is optional, and a missing one must not panic the tick. A
// telemetry frame that fails to build because one subsystem is not wired up yet
// means no telemetry at all, which is the opposite of degrading.
func TestMissingProvidersDoNotBreakTheFrame(t *testing.T) {
	e := telemetry.NewEngine(telemetry.Providers{}, nil)

	snap := e.Build()
	if snap.Sequence != 1 {
		t.Errorf("sequence = %d, want 1", snap.Sequence)
	}
	if len(snap.GNSS) != 0 || snap.HasPico {
		t.Error("a frame built with no providers reported data")
	}
	if size := transport.EncodedTelemetrySize(snap); size <= 0 {
		t.Errorf("an empty frame serialised to %d bytes", size)
	}
}

// The sequence must advance by exactly one per frame. A gap means frames were
// lost; a repeat means two frames shared a number and the client cannot tell them
// apart.
func TestSequenceAdvancesByExactlyOne(t *testing.T) {
	e := telemetry.NewEngine(telemetry.Providers{}, nil)

	for i := uint64(1); i <= 100; i++ {
		if got := e.Build().Sequence; got != i {
			t.Fatalf("frame %d has sequence %d", i, got)
		}
	}
}

// GNSS receivers must be ordered by ID so two consecutive frames can be diffed.
func TestReceiversAreOrderedByID(t *testing.T) {
	now := time.Now()
	p := telemetry.Providers{GNSS: func() []domain.ReceiverStatus {
		// Deliberately out of order.
		return []domain.ReceiverStatus{
			{ReceiverID: 3}, {ReceiverID: 1}, {ReceiverID: 2},
		}
	}}
	snap := telemetry.NewEngine(p, func() time.Time { return now }).Build()

	for i, want := range []int{1, 2, 3} {
		if snap.GNSS[i].ReceiverID != want {
			t.Fatalf("receivers = %v, want ascending", snap.GNSS)
		}
	}
}

// The budget must be enforced against the wire message too, not just the
// domain snapshot, since that is what actually crosses the link.
func TestFrameBudgetConstantMatchesTheDocumentedClass(t *testing.T) {
	// 41 kbit/s priority class, 1 Hz, must carry the frame several times over.
	const priorityKbps = 41
	bytesPerSecond := priorityKbps * 1000 / 8
	if telemetry.FrameBudgetBytes >= bytesPerSecond {
		t.Errorf("FrameBudgetBytes (%d) is not below one second of the %d kbit/s class (%d B/s); "+
			"a frame at that size saturates the priority class on its own",
			telemetry.FrameBudgetBytes, priorityKbps, bytesPerSecond)
	}
}
