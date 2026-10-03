// Package gnss ingests the three u-blox receivers and presents them as a bank
// with one selected receiver.
//
// The 142-byte wire format is a FROZEN contract with the vendored C++ in
// third_party/Read_uB. It is documented field by field, with units, in
// NOTES.md. Do not "improve" it here; change the producer and this decoder in
// the same commit, and change NOTES.md with them.
package gnss

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"
)

// DatagramSize is the exact length of a UDP_message.
//
// There is no magic number and no version field in the wire format, so length
// is the only structural check available. It is checked exactly, not as a
// minimum: a 150-byte datagram is not a UDP_message with padding, it is
// something else entirely.
const DatagramSize = 142

// StartMarker and EndMarker bracket the payload. Read_uB writes 0xAA and 0x99.
const (
	StartMarker = 0xAA
	EndMarker   = 0x99
)

// RawFix is the payload of a UDP_message, in the order the producer writes it.
//
// Offsets are explicit because this is a binary contract with C code that is
// not ours to change, and a struct-field reorder in Go would silently
// reinterpret every field. Offsets are derived from these constants rather than
// from Go's binary.Read on a struct, so the layout is stated once and cannot
// drift with field alignment rules.
type RawFix struct {
	// GPStime is the receiver's time of week in seconds. Not a Unix timestamp:
	// converting it requires the leap-second table, which this package does not
	// carry. It is decoded and passed through untouched.
	GPStime float64

	// ANGLES ARE IN DEGREES.
	//
	// The producer multiplies radians by 180/pi before the datagram leaves
	// Read_uB (Read_uB.cpp, BroadcastData: `meas.latitude * RAD2DEG`). A
	// consumer that multiplies again gets positions wrong by a factor of 57.3
	// that still look plausible -- lat 41.15 becomes 2356 degrees -- and that
	// is exactly the bug that shipped in obc_rocsar's gnss_listener.go, where
	// NOTES.md said "radians" and the code obeyed it.
	//
	// There is no unit conversion in this file. That is the whole design.
	Latitude  float64
	Longitude float64
	Height    float64

	// Velocity, metres per second, in the local NED-ish frame the producer
	// uses. Vdown is negated from the producer's Vd convention.
	Vnorth float64
	Veast  float64
	Vdown  float64

	// Attitude in radians. The producer hardcodes roll and pitch to 0.0 and
	// only fills heading, so those two are not meaningful yet.
	Roll    float64
	Pitch   float64
	Heading float64

	// Accelerometer and gyroscope biases. The producer writes 0.0 to all six.
	AccBiasX, AccBiasY, AccBiasZ    float64
	GyroBiasX, GyroBiasY, GyroBiasZ float64

	// Gravity anomaly. The producer writes 0.0.
	GravityAnomaly float64
}

// Field offsets within the 136-byte payload.
const (
	offGPStime = 0
	offLat     = 8
	offLon     = 16
	offHeight  = 24
	offVn      = 32
	offVe      = 40
	offVd      = 48
	offRoll    = 56
	offPitch   = 64
	offHeading = 72
	offAccX    = 80
	offAccY    = 88
	offAccZ    = 96
	offGyroX   = 104
	offGyroY   = 112
	offGyroZ   = 120
	offAnomaly = 128

	// payloadSize must equal 136 and the header is Start(1) + Epochs(4).
	payloadSize = 136
)

// ErrShortDatagram and friends. They are separate values so a caller can tell
// "another program is sending on this port" from "our receiver is producing
// nonsense", which are different faults with different fixes.
var (
	ErrShortDatagram = errors.New("gnss: datagram is not 142 bytes")
	ErrBadMarker     = errors.New("gnss: datagram start/end marker missing")
	ErrNotFinite     = errors.New("gnss: datagram contains a non-finite value")
	ErrOutOfRange    = errors.New("gnss: coordinate outside valid range")
)

// Decode parses one datagram into a Fix.
//
// The order of checks is deliberate. Size, then markers, then finiteness, then
// range. Each stage's failure is counted separately, because "the receiver is
// not sending" and "the receiver is sending garbage" are different problems and
// an operator needs to be able to tell them apart from telemetry.
func Decode(datagram []byte, receiverID int, now time.Time) (RawFix, error) {
	var out RawFix

	if len(datagram) != DatagramSize {
		return out, fmt.Errorf("%w: got %d", ErrShortDatagram, len(datagram))
	}
	if datagram[0] != StartMarker {
		return out, fmt.Errorf("%w: start 0x%02x, want 0x%02x", ErrBadMarker, datagram[0], StartMarker)
	}
	if datagram[DatagramSize-1] != EndMarker {
		return out, fmt.Errorf("%w: end 0x%02x, want 0x%02x", ErrBadMarker, datagram[DatagramSize-1], EndMarker)
	}

	// Epochs sits at offset 1, little-endian uint32. It counts epochs since the
	// receiver started and is useful for spotting a silent restart: a counter
	// that went backwards means the receiver rebooted.
	epochOffset := 1

	payload := datagram[epochOffset+4 : epochOffset+4+payloadSize]

	le := binary.LittleEndian
	f := func(off int) float64 { return math.Float64frombits(le.Uint64(payload[off : off+8])) }

	out.GPStime = f(offGPStime)
	out.Latitude = f(offLat)
	out.Longitude = f(offLon)
	out.Height = f(offHeight)
	out.Vnorth = f(offVn)
	out.Veast = f(offVe)
	out.Vdown = f(offVd)
	out.Roll = f(offRoll)
	out.Pitch = f(offPitch)
	out.Heading = f(offHeading)
	out.AccBiasX, out.AccBiasY, out.AccBiasZ = f(offAccX), f(offAccY), f(offAccZ)
	out.GyroBiasX, out.GyroBiasY, out.GyroBiasZ = f(offGyroX), f(offGyroY), f(offGyroZ)
	out.GravityAnomaly = f(offAnomaly)

	// A NaN or an infinity survives a float64 round trip perfectly, so the
	// length check alone does not make a datagram trustworthy. A corrupt or
	// wrongly-endian payload produces values of the right type and the wrong
	// magnitude, and the marker bytes can survive that too.
	all := []float64{
		out.GPStime, out.Latitude, out.Longitude, out.Height,
		out.Vnorth, out.Veast, out.Vdown,
		out.Roll, out.Pitch, out.Heading,
		out.AccBiasX, out.AccBiasY, out.AccBiasZ,
		out.GyroBiasX, out.GyroBiasY, out.GyroBiasZ,
		out.GravityAnomaly,
	}
	for _, v := range all {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return RawFix{}, ErrNotFinite
		}
	}

	// Degrees, and therefore bounded. A value outside these bounds is not a
	// position on Earth. Publishing it is worse than publishing nothing,
	// because the Ground Station has no way to distinguish it from a real fix
	// and the operator would act on it.
	if out.Latitude < -90 || out.Latitude > 90 {
		return RawFix{}, fmt.Errorf("%w: latitude %v", ErrOutOfRange, out.Latitude)
	}
	if out.Longitude < -180 || out.Longitude > 180 {
		return RawFix{}, fmt.Errorf("%w: longitude %v", ErrOutOfRange, out.Longitude)
	}

	return out, nil
}

// GroundSpeed returns the horizontal ground speed in m/s.
//
// Computed here rather than taken from a field, because UDP_message has no
// velocity-norm field: it carries the three components and expects the consumer
// to derive the magnitude. The producer's NavData does carry Vel, which is one
// of the reasons the 420-byte datagram is richer -- see ARCHITECTURE.md 6.1.
func (r RawFix) GroundSpeed() float64 {
	return math.Sqrt(r.Vnorth*r.Vnorth + r.Veast*r.Veast)
}
