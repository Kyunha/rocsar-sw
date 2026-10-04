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

	"github.com/rocsar/obc/internal/domain"
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

// Coordinate bounds come from domain, because the decoder and Fix.Valid() are
// two answers to the same question and must not disagree. Geophysical rather than
// a choice: there is no latitude 91, so a value past the pole means a byte was
// mis-parsed, not that somewhere unusual was measured.

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

	// payloadSize is sizeof(GUI_buffer) from Read_uB.h, verified with offsetof().
	payloadSize = 136

	// headerSize is Start (1 byte) + Epochs (4 bytes), verified with offsetof().
	headerSize = 5

	// epochsSize is the uint32 epoch counter. Named because it is the reason
	// headerSize is 5 and not 1.
	epochsSize = 4
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
	payload := datagram[headerSize : headerSize+payloadSize]

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

	// A NaN or an infinity survives a float64 round trip perfectly, so the length
	// check alone does not make a datagram trustworthy. A corrupt or
	// wrongly-endian payload produces values of the right type and the wrong
	// magnitude, and the marker bytes can survive that too.
	//
	// Shared with Encode so the two directions cannot disagree about what a valid
	// fix is -- a decoder that accepts something its own encoder refuses to
	// produce is a decoder with a rule nobody wrote down.
	if err := validate(out); err != nil {
		return RawFix{}, err
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

// Encode builds a 142-byte UDP_message from a RawFix.
//
// The inverse of Decode, in the same package and beside the offsets it uses, for
// three reasons:
//
//   - tools/gnss_bench needs to produce a valid datagram to prove the decoder
//     works, and a test that builds its own datagram by hand is a second
//     definition of the layout that can drift from this one.
//   - It makes the wire format symmetrical to read: one file says where every
//     byte goes in both directions.
//   - Anything that needs a receiver to produce something -- a bench, a
//     simulator, a test -- can do so through the same code that has to stay
//     compatible with it.
//
// It writes DEGREES, because that is what the wire carries. See the note on
// RawFix.Latitude: the bug this project shipped was a consumer converting a
// second time, and an encoder that took radians would invite it.
func Encode(f RawFix) ([]byte, error) {
	out := make([]byte, DatagramSize)
	out[0] = StartMarker
	// Epochs counts epochs since the receiver started; 1 is the only value the
	// shipped producer ever writes.
	out[1], out[2], out[3], out[4] = 1, 0, 0, 0
	out[DatagramSize-1] = EndMarker

	base := headerSize
	le := binary.LittleEndian
	put := func(off int, v float64) {
		le.PutUint64(out[base+off:base+off+8], math.Float64bits(v))
	}

	if err := validate(f); err != nil {
		return nil, err
	}

	put(offGPStime, f.GPStime)
	put(offLat, f.Latitude)
	put(offLon, f.Longitude)
	put(offHeight, f.Height)
	put(offVn, f.Vnorth)
	put(offVe, f.Veast)
	put(offVd, f.Vdown)
	put(offRoll, f.Roll)
	put(offPitch, f.Pitch)
	put(offHeading, f.Heading)
	put(offAccX, f.AccBiasX)
	put(offAccY, f.AccBiasY)
	put(offAccZ, f.AccBiasZ)
	put(offGyroX, f.GyroBiasX)
	put(offGyroY, f.GyroBiasY)
	put(offGyroZ, f.GyroBiasZ)
	put(offAnomaly, f.GravityAnomaly)

	return out, nil
}

// validate is the encode-side counterpart of Decode's checks, so that Encode and
// Decode cannot disagree about what a valid fix is.
func validate(f RawFix) error {
	all := []float64{
		f.GPStime, f.Latitude, f.Longitude, f.Height,
		f.Vnorth, f.Veast, f.Vdown,
		f.Roll, f.Pitch, f.Heading,
		f.AccBiasX, f.AccBiasY, f.AccBiasZ,
		f.GyroBiasX, f.GyroBiasY, f.GyroBiasZ,
		f.GravityAnomaly,
	}
	for _, v := range all {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return ErrNotFinite
		}
	}
	if f.Latitude < -domain.MaxLatitudeDeg || f.Latitude > domain.MaxLatitudeDeg {
		return fmt.Errorf("%w: latitude %v", ErrOutOfRange, f.Latitude)
	}
	if f.Longitude < -domain.MaxLongitudeDeg || f.Longitude > domain.MaxLongitudeDeg {
		return fmt.Errorf("%w: longitude %v", ErrOutOfRange, f.Longitude)
	}
	return nil
}
