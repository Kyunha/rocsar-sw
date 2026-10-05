// Package domain holds the vocabulary of the system and the interfaces its
// outside world is reached through.
//
// It imports nothing from this repository and nothing from outside the standard
// library. Not the protobuf bindings, not ZeroMQ, not net, not os/exec. That
// restriction is not stylistic: it is what makes the control logic testable
// without a serial port, and it is enforced mechanically by
// test/layering_test.go rather than by review.
//
// Every hardware or wire dependency is declared here as an interface, in the
// package that consumes it, so the dependency arrow points from user to
// implementation. Each interface has a real adapter and a mock; a test asserts
// both exist, because an interface with one implementation is a class with no
// seam.
package domain

import "time"

// SubsystemState is the health of one piece of hardware or software.
//
// It mirrors rocsar.v1.SubsystemState. It is redeclared here rather than
// imported so that this package stays free of the generated bindings; the
// mapping between the two lives in internal/transport and is covered by a test.
type SubsystemState int

const (
	SubsystemUnspecified SubsystemState = iota
	SubsystemDisconnected
	SubsystemReady
	SubsystemBusy
	SubsystemError
)

func (s SubsystemState) String() string {
	switch s {
	case SubsystemDisconnected:
		return "DISCONNECTED"
	case SubsystemReady:
		return "READY"
	case SubsystemBusy:
		return "BUSY"
	case SubsystemError:
		return "ERROR"
	default:
		return "UNSPECIFIED"
	}
}

// ErrorCode is why an operation failed.
//
// It mirrors rocsar.v1.ErrorCode. ERROR_NONE is the zero value, and callers
// key their success decision off the boolean that accompanies it rather than
// off this type -- a command that succeeded carries ERROR_NONE and a boolean
// true, and the enum exists for a log line a human reads.
type ErrorCode int

const (
	ErrNone ErrorCode = iota
	ErrInvalidCommand
	ErrInvalidParameter
	ErrInvalidServo
	ErrInvalidHeater
	ErrNotConnected
	ErrUnsupported
	ErrHardwareFault
	ErrInternal
)

func (e ErrorCode) String() string {
	switch e {
	case ErrNone:
		return "ERROR_NONE"
	case ErrInvalidCommand:
		return "ERROR_INVALID_COMMAND"
	case ErrInvalidParameter:
		return "ERROR_INVALID_PARAMETER"
	case ErrInvalidServo:
		return "ERROR_INVALID_SERVO"
	case ErrInvalidHeater:
		return "ERROR_INVALID_HEATER"
	case ErrNotConnected:
		return "ERROR_NOT_CONNECTED"
	case ErrUnsupported:
		return "ERROR_UNSUPPORTED"
	case ErrHardwareFault:
		return "ERROR_HARDWARE_FAULT"
	default:
		return "ERROR_INTERNAL"
	}
}

// FeedbackState says whether an antenna's position, load and temperature are a
// measurement.
//
// The distinction is not decoration. A servo that has never answered and a
// servo that answered and then stopped both have stale numbers in those fields,
// and with a single bool the first is indistinguishable from the second -- a
// console that says "0 A" for a motor that has not moved is lying to an
// operator who is deciding whether to land.
type FeedbackState int

const (
	// FeedbackUnknown: this servo has never answered. Position is the command
	// echo and load/temperature are init-time constants. Nothing has been
	// measured and nothing can be.
	FeedbackUnknown FeedbackState = iota
	// FeedbackMeasured: the ST3215 answered this read and the numbers are real.
	FeedbackMeasured
	// FeedbackHeld: it answered, then stopped. The numbers are the last real
	// reading and they are stale.
	FeedbackHeld
)

// Fix is one GNSS position solution.
//
// Coordinates are DEGREES. This is not a formatting detail and it is not
// negotiable: the C producer converts radians to degrees before the datagram
// leaves Read_uB, and a consumer that converts again produces positions that
// are wrong by a factor of 57.3 while still looking plausible. That bug shipped.
// See NOTES.md.
type Fix struct {
	ReceiverID    int
	LatitudeDeg   float64
	LongitudeDeg  float64
	AltitudeM     float64
	GroundSpeedMP float64
	CourseDeg     float64
	Satellites    uint32

	// ObservedAt is when this fix was decoded, not when it was measured. The
	// distinction matters: a receiver can stop answering while still holding a
	// perfectly valid fix, and only the age reveals which case this is.
	ObservedAt time.Time
}

// Valid reports whether the fix is usable right now.
//
// Coordinate bounds, exported because gnss.Validate checks the same thing and
// two private copies of "90" in two packages is exactly the drift this project
// is trying to avoid: change one and the decoder rejects what the domain accepts.
const (
	MaxLatitudeDeg  = 90
	MaxLongitudeDeg = 180
)

// u-blox's invalid solution, as it appears on the wire. See Fix.Valid.
const (
	ubloxInvalidLatitudeDeg  = 90.0
	ubloxInvalidLongitudeDeg = 0.0
)

// An out-of-range coordinate is not a valid fix at any age. Publishing
// lat=90, lon=1e300 is worse than publishing nothing, because the Ground
// Station cannot tell it apart from a real reading and the operator would act
// on it.
//
// The bounds check alone does NOT catch the case that actually happens. A u-blox
// receiver with no fix does not send nothing and does not send a flag: the
// 142-byte UDP_message has no validity field to send one in, so Read_uB fills the
// message with u-blox's documented invalid-solution encoding instead --
//
//	latitude  90.0        (0x42600000)
//	longitude 0.0
//	height    -6399593.6  (~ -6.4e6 m)
//
// Every one of those values is inside its range and finite. A receiver sitting
// on a bench with no sky view was therefore published as a real position at the
// north pole, 6,399 km below sea level, and the Ground Station drew it.
//
// It is caught here, in Valid(), rather than in the decoder, for two reasons.
// Valid() is what the receiver consults to compute FixOK, so one check fixes
// every consumer -- telemetry, the GS, anything added later. And a datagram that
// decodes cleanly is still worth keeping: "the receiver is answering and has no
// fix" is a different fact from "the receiver is silent", and an operator needs
// to tell those apart. Dropping the datagram would collapse them.
//
// The test is the PAIR, not latitude alone. Latitude 90 is a real coordinate --
// the pole -- and rejecting it on its own would throw away a legitimate fix once
// in a blue moon. Longitude exactly 0 at latitude exactly 90 is the encoding and
// nothing else.
func (f Fix) Valid() bool {
	if f.LatitudeDeg == ubloxInvalidLatitudeDeg && f.LongitudeDeg == ubloxInvalidLongitudeDeg {
		return false
	}
	return f.LatitudeDeg >= -MaxLatitudeDeg && f.LatitudeDeg <= MaxLatitudeDeg &&
		f.LongitudeDeg >= -MaxLongitudeDeg && f.LongitudeDeg <= MaxLongitudeDeg
}

// ReceiverStatus is one GNSS receiver's health, reported whether or not it is
// the selected one.
//
// All three receivers appear in telemetry so the operator can see the
// redundancy working and know which one is trusted. That is the entire reason
// there are three.
type ReceiverStatus struct {
	ReceiverID int
	Selected   bool

	// FixOK is false once the last fix is older than the staleness bound, even
	// though the last decoded value was valid. An old fix is worse than no fix.
	FixOK    bool
	Fix      Fix
	FixAge   time.Duration
	HasFix   bool
	Accepted uint64
	Rejected uint64
}

// Axis is one antenna axis as the flight controller reports it.
type Axis struct {
	ServoID         uint32
	ManualMode      bool
	CurrentTick     uint32
	CurrentAngleDeg float64
	Load            int32
	TemperatureC    int32
	CenterTick      uint32
	MountOffsetDeg  float64
	DirMultiplier   float64
	FeedbackState   FeedbackState
	FeedbackError   int32
}

// PicoTelemetry is the flight controller's 50 Hz report.
type PicoTelemetry struct {
	GondolaHeadingDeg float64
	TargetHeadingDeg  float64
	Heater1State      bool
	Heater2State      bool
	Axes              []Axis

	// IMUPresent is whether GondolaHeadingDeg is a measurement. False when no
	// BNO055 is fitted, in which case the heading is the last bearing held and
	// the two are indistinguishable without this bit.
	IMUPresent bool

	// ImuTemperatureC is the BNO055 die temperature in degrees Celsius, read
	// alongside the heading. Only meaningful when IMUPresent is true:
	// otherwise it is held from the last reading (zero at boot), and a zero
	// must be read as "no measurement" via IMUPresent -- the same absence
	// discipline as GondolaHeadingDeg.
	ImuTemperatureC float64
	ObservedAt time.Time
}

// Photo is a captured image on disk, not in memory.
//
// It is a reference because the air link is 115 kbit/s. The bytes are fetched
// over HTTP; putting them in a command response would put a megabyte through the
// priority class and starve the control channel.
type Photo struct {
	Name      string
	SizeBytes uint64
	Kind      string
	Path      string
}

// LinkStatus describes the traffic shaper.
//
// InactiveReason exists because "tc unavailable", "no permission" and
// "disabled by configuration" are three different operator actions. A single
// bool sends someone to debug the wrong thing.
type LinkStatus struct {
	State          SubsystemState
	Device         string
	RateKbps       uint32
	PriorityKbps   uint32
	ShapingActive  bool
	InactiveReason string
}

// Result is the outcome of a command.
//
// Success and Error are deliberately redundant: callers branch on Success, and
// Error carries the reason. A caller that forgets to check Success and reads
// Error sees ERROR_NONE and believes it worked, which is the failure mode a
// single return value invites.
type Result struct {
	Success bool
	Error   ErrorCode
	Message string

	// Photo is set only by take_photo. Absent means no image was produced.
	Photo *Photo
}
