// Package gsview renders a telemetry snapshot as plain structs.
//
// It is the Ground Station's presentation logic without a presentation: no
// sockets, no window, no clock. The web console in cmd/gs serialises a View
// to JSON and draws it; the terminal console keeps its own text renderer. Both
// consume the same absence discipline, implemented once, here.
//
// The single most important rule in this package: absence is not zero. The
// proto3 wire makes absence and zero indistinguishable unless the consumer is
// deliberate, so every field that can be absent IS absent in the view -- as a
// nil pointer, never as a zero. A console that conflates them shows a
// healthy-looking 0 for a motor that has not moved, which is the failure this
// project exists to prevent. See GUI_ARCHITECTURE.md section 7.
//
// Input is telemetry.Snapshot, produced by transport.DecodeTelemetry, never
// the proto frame. Two reasons. The layering rule (test/layering_test.go)
// forbids this package the generated bindings, on purpose: assembly must stay
// testable without the schema. And the decoder already exists for exactly this
// -- "so a captured frame can be replayed through the same view logic that
// renders a live one" -- so a recorded flight and a live one cannot disagree
// about what a frame means.
//
// What this package does NOT do, deliberately:
//
//   - No link aging. Staleness ("no frame for 3×interval") is evaluated at
//     render time from timestamps, not frozen into a per-frame boolean: a frame
//     is never stale the moment it arrives. Sequence gaps and OBC restarts ride
//     alongside on client.Frame; link health rides on client.LinkState. This
//     package renders one instant; the console assembles instants into a story.
//   - No units conversion. Degrees in, degrees out. The double conversion that
//     shipped once is the reason test/gnss_test.go exists.
//   - No satellite count. There is none anywhere in the schema; the frozen
//     142-byte UDP_message does not carry one. A console showing "0 satellites"
//     beside a valid position is worse than one showing nothing.
//   - No sdr.last_output_file. Declared on the wire, never populated by the
//     encoder. Not a live value; not rendered.
package gsview

import (
	"fmt"
	"strings"
	"time"

	"github.com/rocsar/obc/internal/domain"
	"github.com/rocsar/obc/internal/telemetry"
)

// View is one rendered instant, JSON-ready.
//
// Pointers mean "absent" throughout: a nil field is "no data", never zero.
// The JSON shape is stable -- no omitempty anywhere -- so a nil renders as an
// explicit null rather than a missing key, and the frontend switches on
// presence instead of guessing from values.
type View struct {
	Sequence    uint64    `json:"sequence"`
	GeneratedAt time.Time `json:"generated_at"`
	UptimeS     float64   `json:"uptime_s"`

	System SystemView     `json:"system"`
	GNSS   []ReceiverView `json:"gnss"`
	Pico   *PicoView      `json:"pico"`
	// PicoConnected reports whether the link to the flight controller is open.
	// It is not part of PicoView on purpose: connection state is about the
	// link, and the Pico block is about the last frame -- a disconnected link
	// with a remembered frame must render "NOT connected" beside live-looking
	// numbers, which is exactly what the two fields side by side produce.
	PicoConnected bool       `json:"pico_connected"`
	PicoAck       *AckView   `json:"pico_last_ack"`
	SDR           SDRView    `json:"sdr"`
	Camera        CameraView `json:"camera"`
	Link          LinkView   `json:"link"`

	Healthy           bool     `json:"healthy"`
	NotHealthyReasons []string `json:"not_healthy_reasons"`
}

// SystemView is the OBC itself.
type SystemView struct {
	State    string  `json:"state"`
	UptimeS  float64 `json:"uptime_s"`
	CPUTempC float64 `json:"cpu_temp_c"`
	// Mocked names the subsystems running against fakes. Non-empty renders as
	// a persistent banner: telemetry built from mocks is visibly mock, always.
	Mocked []string `json:"mocked"`
}

// ReceiverView is one GNSS receiver. Position is nil unless the fix is
// currently valid -- a receiver without a fix still carries the last values it
// decoded, and rendering them beside "no fix" invites reading the number
// instead of the qualifier. FixAgeS is set whenever a fix was ever decoded,
// even a stale one: "last fix 5 s ago" and "never" are different sentences.
type ReceiverView struct {
	ReceiverID      int           `json:"receiver_id"`
	Selected        bool          `json:"selected"`
	FixOK           bool          `json:"fix_ok"`
	Position        *PositionView `json:"position"`
	FixAgeS         *float64      `json:"fix_age_s"`
	PacketsAccepted uint64        `json:"packets_accepted"`
	PacketsRejected uint64        `json:"packets_rejected"`
}

// PositionView is a valid fix. Degrees, metres, metres per second, degrees --
// the wire units, unchanged.
type PositionView struct {
	LatitudeDeg    float64 `json:"latitude_deg"`
	LongitudeDeg   float64 `json:"longitude_deg"`
	AltitudeM      float64 `json:"altitude_m"`
	GroundSpeedMps float64 `json:"ground_speed_mps"`
	CourseDeg      float64 `json:"course_deg"`

	// Climbrate in m/s, positive up. Nil until the estimator has a window worth
	// fitting, which for a launch is about a minute.
	//
	// Nil rather than zero, and this is the one field where the difference is the
	// whole point: a balloon at float genuinely reads ~0, so a client that
	// cannot tell "no rate yet" from "not climbing" will eventually call the
	// first minute of every ascent a stall.
	VerticalRateMps *float64 `json:"vertical_rate_mps"`
}

// PicoView is the flight controller. Nil View.Pico means never heard from it,
// not "all zero": populating a zeroed block on a silent link is precisely how
// a console shows a healthy-looking 0 for a motor that has not moved.
type PicoView struct {
	GondolaHeadingDeg float64 `json:"gondola_heading_deg"`
	TargetHeadingDeg  float64 `json:"target_heading_deg"`
	Heater1On         bool    `json:"heater1_on"`
	Heater2On         bool    `json:"heater2_on"`
	IMU               string  `json:"imu"`
	// ImuTemperatureC is the BNO055 die temperature in degrees Celsius. Nil
	// unless the IMU is present (measured): a held value is the last reading,
	// and showing it as a measurement repeats the held-bearing trap this
	// column exists to prevent.
	ImuTemperatureC *float64 `json:"imu_temperature_c"`

	// Roll and pitch in degrees. Nil unless the IMU is present, on the same
	// reasoning as the temperature: a held tilt next to a live bearing invites
	// reading the two together, and only one of them is current.
	GondolaRollDeg  *float64 `json:"gondola_roll_deg"`
	GondolaPitchDeg *float64 `json:"gondola_pitch_deg"`

	// The BNO055's calibration status as the raw register byte: two bits per
	// sensor, most significant first (system, gyroscope, accelerometer,
	// magnetometer), each 0 uncalibrated to 3 fully. Nil unless present, for the
	// same reason.
	//
	// Carried verbatim rather than unpacked into four numbers. It is one register,
	// the firmware does not act on it, and a decoded form would invite four
	// separate "calibrated: yes/no" claims where one byte says it once.
	ImuCalibration *uint32 `json:"imu_calibration"`

	// The largest linear acceleration seen since boot, m/s^2, gravity already
	// removed by the sensor. Monotonic, so this never falls -- it is "hardest
	// thing that has happened", not "how hard things are going now". PeakAccelEvent
	// beside it is what says whether anything new has.
	ImuPeakAccelMs2 [3]float64 `json:"imu_peak_accel_ms2"`

	// Increments once per sample that set a new peak. One increment per shock,
	// not per axis: a chute deployment moves all three at once and is one event.
	ImuPeakAccelEvent uint32     `json:"imu_peak_accel_event"`
	Antennas          []AxisView `json:"antennas"`
}

// AxisView is one antenna axis. Load and TemperatureC are nil unless the servo
// actually answered (feedback MEASURED): a held value is the last reading or
// the command echo, and showing it as a measurement is the specific thing this
// column exists to prevent.
type AxisView struct {
	ServoID         uint32  `json:"servo_id"`
	ManualMode      bool    `json:"manual_mode"`
	CurrentTick     uint32  `json:"current_tick"`
	CurrentAngleDeg float64 `json:"current_angle_deg"`
	Load            *int32  `json:"load"`
	TemperatureC    *int32  `json:"temperature_c"`
	CenterTick      uint32  `json:"center_tick"`
	// Whether this servo has been taught its centre. False means CenterTick is an
	// assumption -- the default 2048 -- and not something anyone measured. The
	// console has to say which, because an untaught axis and a centred one report
	// the same number and differ only here.
	CenterZeroed   bool    `json:"center_zeroed"`
	MountOffsetDeg float64 `json:"mount_offset_deg"`
	DirMultiplier  float64 `json:"dir_multiplier"`
	Feedback       string  `json:"feedback"`
	// FeedbackError names the ST3215 fault (overheat, overload) when the servo
	// reports one while answering perfectly well-formed frames. Nil when the
	// servo reports no fault: a servo in trouble otherwise reads clean.
	FeedbackError *string `json:"feedback_error"`
}

// AckView is the flight controller's last answer. Nil means never acknowledged.
type AckView struct {
	CommandSequence uint32 `json:"command_sequence"`
	Success         bool   `json:"success"`
	Error           string `json:"error"`
}

// SDRView is the acquisition program.
type SDRView struct {
	State     string  `json:"state"`
	Running   bool    `json:"running"`
	PID       int64   `json:"pid"`
	LastLog   *string `json:"last_log"`
	LastError *string `json:"last_error"`
}

// CameraView is the USB camera. PhotosTaken is nil unless the subsystem
// reported a count: "—", not 0 photos.
type CameraView struct {
	State       string  `json:"state"`
	Device      *string `json:"device"`
	LastPhoto   *string `json:"last_photo"`
	PhotosTaken *uint64 `json:"photos_taken"`
}

// LinkView is the traffic-control state.
type LinkView struct {
	State          string  `json:"state"`
	Device         string  `json:"device"`
	RateKbps       uint32  `json:"rate_kbps"`
	PriorityKbps   uint32  `json:"priority_kbps"`
	ShapingActive  bool    `json:"shaping_active"`
	InactiveReason *string `json:"inactive_reason"`
}

// Build renders one snapshot.
//
// Pure: no I/O, no clock, no window. The same input always produces the same
// view, which is what makes the replay test meaningful -- a recorded frame and
// a live frame go through this function and cannot disagree.
func Build(snap telemetry.Snapshot) View {
	v := View{
		Sequence:      snap.Sequence,
		GeneratedAt:   snap.GeneratedAt,
		UptimeS:       snap.System.Uptime.Seconds(),
		PicoConnected: snap.PicoConnected,
		System: SystemView{
			State:    snap.System.State.String(),
			UptimeS:  snap.System.Uptime.Seconds(),
			CPUTempC: snap.System.CPUTemp,
			Mocked:   append([]string{}, snap.System.Mocked...),
		},
		GNSS:   make([]ReceiverView, 0, len(snap.GNSS)),
		SDR:    buildSDR(snap.SDR),
		Camera: buildCamera(snap),
		Link:   buildLink(snap.Link),
	}

	for _, r := range snap.GNSS {
		v.GNSS = append(v.GNSS, buildReceiver(r))
	}

	if snap.HasPico {
		v.Pico = buildPico(snap.Pico)
	}
	if snap.PicoAck != nil {
		v.PicoAck = &AckView{
			CommandSequence: snap.PicoAck.CommandSequence,
			Success:         snap.PicoAck.Success,
			Error:           snap.PicoAck.Error.String(),
		}
	}

	v.Healthy = snap.Healthy()
	v.NotHealthyReasons = unhealthyReasons(snap)
	if v.NotHealthyReasons == nil {
		v.NotHealthyReasons = []string{}
	}

	return v
}

func buildReceiver(r domain.ReceiverStatus) ReceiverView {
	out := ReceiverView{
		ReceiverID:      r.ReceiverID,
		Selected:        r.Selected,
		FixOK:           r.FixOK,
		PacketsAccepted: r.Accepted,
		PacketsRejected: r.Rejected,
	}
	if r.FixOK {
		out.Position = &PositionView{
			LatitudeDeg:     r.Fix.LatitudeDeg,
			LongitudeDeg:    r.Fix.LongitudeDeg,
			AltitudeM:       r.Fix.AltitudeM,
			GroundSpeedMps:  r.Fix.GroundSpeedMP,
			CourseDeg:       r.Fix.CourseDeg,
			VerticalRateMps: r.Fix.VerticalRateMP,
		}
	}
	if r.HasFix {
		age := r.FixAge.Seconds()
		out.FixAgeS = &age
	}
	return out
}

func buildPico(p domain.PicoTelemetry) *PicoView {
	out := &PicoView{
		GondolaHeadingDeg: p.GondolaHeadingDeg,
		TargetHeadingDeg:  p.TargetHeadingDeg,
		Heater1On:         p.Heater1State,
		Heater2On:         p.Heater2State,
		IMU:               measuredOrHeld(p.IMUPresent),
		Antennas:          make([]AxisView, 0, len(p.Axes)),
	}
	if p.IMUPresent {
		temp := p.ImuTemperatureC
		out.ImuTemperatureC = &temp
		roll, pitch := p.GondolaRollDeg, p.GondolaPitchDeg
		out.GondolaRollDeg, out.GondolaPitchDeg = &roll, &pitch
		calib := p.IMUCalibration
		out.ImuCalibration = &calib
		// The peak is copied unconditionally, even when the sensor is absent.
		// It is a monotonic maximum over the whole flight, so it remains the
		// hardest thing that has happened after the sensor stops answering --
		// which is exactly when an operator most wants to know it. Zeroing it on
		// absence would erase the record of the event that stopped the sensor.
		out.ImuPeakAccelMs2 = p.IMUPeakAccelMs2
		out.ImuPeakAccelEvent = p.IMUPeakAccelEvent
	}
	for _, a := range p.Axes {
		axis := AxisView{
			ServoID:         a.ServoID,
			ManualMode:      a.ManualMode,
			CurrentTick:     a.CurrentTick,
			CurrentAngleDeg: a.CurrentAngleDeg,
			CenterTick:      a.CenterTick,
			CenterZeroed:    a.CenterZeroed,
			MountOffsetDeg:  a.MountOffsetDeg,
			DirMultiplier:   a.DirMultiplier,
			Feedback:        feedbackName(a.FeedbackState),
		}
		if a.FeedbackState == domain.FeedbackMeasured {
			load, temp := a.Load, a.TemperatureC
			axis.Load, axis.TemperatureC = &load, &temp
		}
		if a.FeedbackError != 0 {
			// The servo's own status bits, not an ErrorCode. See domain.ServoFaults
			// for why sharing that enum was wrong.
			name := strings.Join(domain.ServoFaults(a.FeedbackError), "+")
			axis.FeedbackError = &name
		}
		out.Antennas = append(out.Antennas, axis)
	}
	return out
}

func buildSDR(s telemetry.SDRSnapshot) SDRView {
	out := SDRView{
		State:   s.State.String(),
		Running: s.Running,
		PID:     s.PID,
	}
	if s.LastLog != "" {
		out.LastLog = &s.LastLog
	}
	if s.LastError != "" {
		out.LastError = &s.LastError
	}
	return out
}

func buildCamera(snap telemetry.Snapshot) CameraView {
	out := CameraView{State: snap.Camera.String()}
	if snap.CameraDevice != "" {
		out.Device = &snap.CameraDevice
	}
	if snap.LastPhoto != "" {
		out.LastPhoto = &snap.LastPhoto
	}
	if snap.PhotosTakenKnown {
		out.PhotosTaken = &snap.PhotosTaken
	}
	return out
}

func buildLink(l domain.LinkStatus) LinkView {
	out := LinkView{
		State:         l.State.String(),
		Device:        l.Device,
		RateKbps:      l.RateKbps,
		PriorityKbps:  l.PriorityKbps,
		ShapingActive: l.ShapingActive,
	}
	if l.InactiveReason != "" {
		out.InactiveReason = &l.InactiveReason
	}
	return out
}

// unhealthyReasons mirrors Snapshot.Healthy's conjuncts, one sentence each. A
// banner saying "not healthy" without why is unactionable; a reason that does
// not correspond to a conjunct is a second opinion about health, which this
// package must not have.
func unhealthyReasons(snap telemetry.Snapshot) []string {
	var out []string
	if snap.System.State != domain.SubsystemReady {
		out = append(out, fmt.Sprintf("system is %s", snap.System.State))
	}
	if !snap.PicoConnected {
		out = append(out, "flight controller not connected")
	}
	if snap.Camera == domain.SubsystemError {
		out = append(out, "camera error")
	}
	if snap.SDR.State == domain.SubsystemError {
		out = append(out, "sdr error")
	}
	return out
}

// measuredOrHeld distinguishes a reading from a bearing the flight controller
// is holding because the IMU went quiet. imu_present is the only thing
// separating the two; a heading shown without it is a guess.
func measuredOrHeld(present bool) string {
	if present {
		return "MEASURED"
	}
	return "HELD"
}

// feedbackName renders feedback_state, which travels as an int32 rather than
// the enum so a value from a future firmware decodes to a plain integer instead
// of failing the frame. The default case is deliberate: a number never seen
// before is more useful than a name guessed for it.
func feedbackName(v domain.FeedbackState) string {
	switch v {
	case domain.FeedbackMeasured:
		return "MEASURED"
	case domain.FeedbackHeld:
		return "HELD"
	case domain.FeedbackUnknown:
		return "no reading"
	default:
		return fmt.Sprintf("state %d", int(v))
	}
}
