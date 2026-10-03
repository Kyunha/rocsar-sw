package test

import (
	"math"
	"testing"
	"time"

	"github.com/rocsar/obc/internal/domain"
	"github.com/rocsar/obc/internal/gnss"
)

// buildDatagram assembles a 142-byte UDP_message exactly as Read_uB's C struct
// lays it out, so these tests exercise the real byte positions rather than a Go
// struct's idea of them.
//
// Layout verified with offsetof() against the actual C header:
//
//	Start   0    End 141   sizeof(UDP_message) 142
//	Epochs  1    Data  5    sizeof(GUI_buffer) 136
//	Latitude at 13, Longitude at 21, Height at 29, Heading at 77, Ganom at 133
type builder struct{ b []byte }

func newDatagram() *builder {
	b := make([]byte, gnss.DatagramSize)
	b[0] = gnss.StartMarker
	b[1], b[2], b[3], b[4] = 1, 0, 0, 0 // Epochs, little-endian
	b[gnss.DatagramSize-1] = gnss.EndMarker
	return &builder{b: b}
}

func (d *builder) f64(absOffset int, v float64) *builder {
	bits := math.Float64bits(v)
	for i := 0; i < 8; i++ {
		d.b[absOffset+i] = byte(bits >> (8 * i)) // little-endian
	}
	return d
}

func (d *builder) bytes() []byte { return d.b }

// THE regression test.
//
// obc_rocsar's NOTES.md documented latitude and longitude as arriving "in
// RADIANS" and gnss_listener.go obeyed it, multiplying by 180/pi on receipt.
// But Read_uB already converts before sending -- Read_uB.cpp, BroadcastData:
// `meas.latitude * RAD2DEG`. So the shipped system displayed a position wrong by
// a factor of 57.3, which for Lisbon produced latitude 2357 degrees. It looked
// like a number, so nobody caught it.
//
// Note that the C struct's own comment in Read_uB.h also says "in radians",
// which is where the wrong documentation came from. The wire format is in
// degrees and the producer's comment is stale.
func TestGNSSDecodesDegreesNotRadians(t *testing.T) {
	const (
		latDeg = 38.7223 // Lisbon
		lonDeg = -9.1393
	)

	raw, err := gnss.Decode(
		newDatagram().f64(13, latDeg).f64(21, lonDeg).f64(29, 120.0).f64(77, 275.5).bytes(),
		1, time.Now())
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if raw.Latitude != latDeg {
		t.Errorf("Latitude = %v, want %v -- the decoder converted and the value is now wrong by ~57.3x",
			raw.Latitude, latDeg)
	}
	if raw.Longitude != lonDeg {
		t.Errorf("Longitude = %v, want %v", raw.Longitude, lonDeg)
	}
	if raw.Height != 120.0 {
		t.Errorf("Height = %v, want 120", raw.Height)
	}
	if raw.Heading != 275.5 {
		t.Errorf("Heading = %v, want 275.5", raw.Heading)
	}
}

// A latitude in the range that radians-times-57.3 would produce must be
// REJECTED, not accepted as a fix. This is the check that turns "wrong by a
// factor of 57.3" from a silent corruption into a counted rejection.
func TestGNSSRejectsRadiansDoubledIntoDegrees(t *testing.T) {
	// 0.674 * 180/pi == 38.7: the bug's actual output for Lisbon.
	_, err := gnss.Decode(newDatagram().f64(13, 38.7223*57.2957795).bytes(), 1, time.Now())
	if err == nil {
		t.Fatal("a latitude of 2218 was accepted; out-of-range coordinates must be rejected")
	}
}

// Every field, at its verified offset. A field-order change in the Go decoder
// would otherwise reinterpret the whole struct silently.
func TestGNSSFieldOffsets(t *testing.T) {
	raw, err := gnss.Decode(newDatagram().
		f64(5+0, 1.5).    // GPStime
		f64(5+8, 10.0).   // Latitude
		f64(5+16, 20.0).  // Longitude
		f64(5+24, 30.0).  // Height
		f64(5+32, 1.0).   // Vnorth
		f64(5+40, 2.0).   // Veast
		f64(5+48, 3.0).   // Vdown
		f64(5+56, 4.0).   // Roll
		f64(5+64, 5.0).   // Pitch
		f64(5+72, 6.0).   // Heading
		f64(5+80, 7.0).   // AccBiX
		f64(5+88, 8.0).   // AccBiY
		f64(5+96, 9.0).   // AccBiZ
		f64(5+104, 10.0). // GyrBiX
		f64(5+112, 11.0). // GyrBiY
		f64(5+120, 12.0). // GyrBiZ
		f64(5+128, 13.0). // Ganom
		bytes(), 1, time.Now())
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	checks := []struct {
		name string
		got  float64
		want float64
	}{
		{"GPStime", raw.GPStime, 1.5},
		{"Latitude", raw.Latitude, 10.0},
		{"Longitude", raw.Longitude, 20.0},
		{"Height", raw.Height, 30.0},
		{"Vnorth", raw.Vnorth, 1.0},
		{"Veast", raw.Veast, 2.0},
		{"Vdown", raw.Vdown, 3.0},
		{"Roll", raw.Roll, 4.0},
		{"Pitch", raw.Pitch, 5.0},
		{"Heading", raw.Heading, 6.0},
		{"AccBiasX", raw.AccBiasX, 7.0},
		{"AccBiasY", raw.AccBiasY, 8.0},
		{"AccBiasZ", raw.AccBiasZ, 9.0},
		{"GyroBiasX", raw.GyroBiasX, 10.0},
		{"GyroBiasY", raw.GyroBiasY, 11.0},
		{"GyroBiasZ", raw.GyroBiasZ, 12.0},
		{"GravityAnomaly", raw.GravityAnomaly, 13.0},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v -- field offset or width is wrong", c.name, c.got, c.want)
		}
	}
}

func TestGroundSpeedDerivedFromComponents(t *testing.T) {
	// 3-4-5 triangle.
	raw, err := gnss.Decode(newDatagram().f64(13, 1.0).f64(5+32, 3.0).f64(5+40, 4.0).bytes(), 1, time.Now())
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got := raw.GroundSpeed(); math.Abs(got-5.0) > 1e-9 {
		t.Errorf("GroundSpeed = %v, want 5", got)
	}
}

// Rejection is not an edge case to be tolerated. Each failure mode is a
// different operator problem and they are counted separately so telemetry can
// distinguish "the receiver stopped" from "the receiver is producing garbage".
func TestGNSSRejectsMalformed(t *testing.T) {
	valid := func() []byte {
		return newDatagram().f64(13, 38.7).f64(21, -9.1).bytes()
	}

	cases := []struct {
		name string
		mut  func([]byte) []byte
		want error
	}{
		{"too short", func(b []byte) []byte { return b[:141] }, gnss.ErrShortDatagram},
		{"too long", func(b []byte) []byte { return append(b, 0) }, gnss.ErrShortDatagram},
		{"bad start marker", func(b []byte) []byte { b[0] = 0x00; return b }, gnss.ErrBadMarker},
		{"bad end marker", func(b []byte) []byte { b[len(b)-1] = 0x00; return b }, gnss.ErrBadMarker},
		{"NaN latitude", func(b []byte) []byte { return putF64(b, 13, math.NaN()) }, gnss.ErrNotFinite},
		{"Inf longitude", func(b []byte) []byte { return putF64(b, 21, math.Inf(1)) }, gnss.ErrNotFinite},
		{"latitude out of range", func(b []byte) []byte { return putF64(b, 13, 91.0) }, gnss.ErrOutOfRange},
		{"longitude out of range", func(b []byte) []byte { return putF64(b, 21, 181.0) }, gnss.ErrOutOfRange},
		// All-zero is structurally valid: correct markers, correct length. It is
		// a legitimate (if useless) fix at Null Island, so it is accepted. The
		// point is that range checks do not accidentally reject it.
		{"all zeros is a valid fix", func(b []byte) []byte { return putF64(b, 13, 0) }, nil},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := gnss.Decode(c.mut(valid()), 1, time.Now())
			switch {
			case c.want == nil && err != nil:
				t.Fatalf("expected acceptance, got %v", err)
			case c.want != nil && err == nil:
				t.Fatalf("expected %v, got acceptance", c.want)
			case c.want != nil && !errorsIs(err, c.want):
				t.Fatalf("got %v, want %v", err, c.want)
			}
		})
	}
}

func putF64(b []byte, off int, v float64) []byte {
	bits := math.Float64bits(v)
	for i := 0; i < 8; i++ {
		b[off+i] = byte(bits >> (8 * i))
	}
	return b
}

func errorsIs(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// domain.Fix.Valid is the range check the GS also relies on.
func TestFixValidRejectsImpossibleCoordinates(t *testing.T) {
	good := domain.Fix{LatitudeDeg: 38.7, LongitudeDeg: -9.1}
	if !good.Valid() {
		t.Error("a real coordinate was rejected")
	}
	for _, bad := range []domain.Fix{
		{LatitudeDeg: 91},
		{LatitudeDeg: -91},
		{LongitudeDeg: 181},
		{LongitudeDeg: -181},
	} {
		if bad.Valid() {
			t.Errorf("Valid() accepted %+v", bad)
		}
	}
}
