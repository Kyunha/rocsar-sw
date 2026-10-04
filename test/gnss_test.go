package test

import (
	"math"
	"testing"
	"time"

	"github.com/rocsar/obc/internal/domain"
	"github.com/rocsar/obc/internal/gnss"
)

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
		mustEncode(t, gnss.RawFix{
			Latitude: latDeg, Longitude: lonDeg, Height: 120.0, Heading: 275.5,
		}), 1, time.Now())
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
//
// Hand-built rather than encoded, because gnss.Encode refuses to produce it --
// it shares the decoder's validator, which is the point of the previous test.
// A consumer that somehow received this datagram must reject it, and that is a
// separate property from the encoder declining to send it.
func TestGNSSRejectsRadiansDoubledIntoDegrees(t *testing.T) {
	if _, err := gnss.Encode(gnss.RawFix{Latitude: 38.7223 * 57.2957795}); err == nil {
		t.Fatal("the encoder produced a latitude of 2218; it should have refused")
	}

	doubled := make([]byte, gnss.DatagramSize)
	doubled[0] = gnss.StartMarker
	doubled[len(doubled)-1] = gnss.EndMarker
	putF64(doubled, 13, 38.7223*57.2957795)

	if _, err := gnss.Decode(doubled, 1, time.Now()); err == nil {
		t.Fatal("the decoder accepted a latitude of 2218; out-of-range coordinates must be rejected")
	}
}

// Every field, at its verified offset. A field-order change in the Go decoder
// would otherwise reinterpret the whole struct silently.
// Every field must survive a round trip at its own offset. A field-order change
// in either direction would reinterpret the struct silently, and this asserts
// the value that comes out, not just the size.
func TestEveryFieldRoundTrips(t *testing.T) {
	orig := gnss.RawFix{
		GPStime: 1.5, Latitude: 10.0, Longitude: 20.0, Height: 30.0,
		Vnorth: 1.0, Veast: 2.0, Vdown: 3.0,
		Roll: 4.0, Pitch: 5.0, Heading: 6.0,
		AccBiasX: 7.0, AccBiasY: 8.0, AccBiasZ: 9.0,
		GyroBiasX: 10.0, GyroBiasY: 11.0, GyroBiasZ: 12.0,
		GravityAnomaly: 13.0,
	}

	raw, err := gnss.Decode(mustEncode(t, orig), 1, time.Now())
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	checks := []struct {
		name string
		got  float64
		want float64
	}{
		{"GPStime", raw.GPStime, orig.GPStime},
		{"Latitude", raw.Latitude, orig.Latitude},
		{"Longitude", raw.Longitude, orig.Longitude},
		{"Height", raw.Height, orig.Height},
		{"Vnorth", raw.Vnorth, orig.Vnorth},
		{"Veast", raw.Veast, orig.Veast},
		{"Vdown", raw.Vdown, orig.Vdown},
		{"Roll", raw.Roll, orig.Roll},
		{"Pitch", raw.Pitch, orig.Pitch},
		{"Heading", raw.Heading, orig.Heading},
		{"AccBiasX", raw.AccBiasX, orig.AccBiasX},
		{"AccBiasY", raw.AccBiasY, orig.AccBiasY},
		{"AccBiasZ", raw.AccBiasZ, orig.AccBiasZ},
		{"GyroBiasX", raw.GyroBiasX, orig.GyroBiasX},
		{"GyroBiasY", raw.GyroBiasY, orig.GyroBiasY},
		{"GyroBiasZ", raw.GyroBiasZ, orig.GyroBiasZ},
		{"GravityAnomaly", raw.GravityAnomaly, orig.GravityAnomaly},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v -- field offset or width is wrong", c.name, c.got, c.want)
		}
	}
}

// The encoder must produce exactly DatagramSize bytes with the markers in place,
// or a receiver built against it will bind a socket and never be framed.
func TestEncodeShape(t *testing.T) {
	b, err := gnss.Encode(gnss.RawFix{Latitude: 1, Longitude: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != gnss.DatagramSize {
		t.Fatalf("Encode produced %d bytes, want %d", len(b), gnss.DatagramSize)
	}
	if b[0] != gnss.StartMarker {
		t.Errorf("start marker = 0x%02x, want 0x%02x", b[0], gnss.StartMarker)
	}
	if b[len(b)-1] != gnss.EndMarker {
		t.Errorf("end marker = 0x%02x, want 0x%02x", b[len(b)-1], gnss.EndMarker)
	}
}

// Encode and Decode must agree on validity, or a fix the encoder refuses is one
// the decoder would have accepted -- and a rule neither can state.
func TestEncodeRefusesWhatDecodeRefuses(t *testing.T) {
	bad := []gnss.RawFix{
		{Latitude: 91},
		{Latitude: -91},
		{Longitude: 181},
		{Longitude: -181},
		{Latitude: math.NaN()},
		{Height: math.Inf(1)},
	}
	for _, f := range bad {
		if _, err := gnss.Encode(f); err == nil {
			t.Errorf("Encode accepted %+v", f)
		}
	}
	if _, err := gnss.Encode(gnss.RawFix{Latitude: 38.7, Longitude: -9.1}); err != nil {
		t.Errorf("Encode rejected a valid fix: %v", err)
	}
}

func TestGroundSpeedDerivedFromComponents(t *testing.T) {
	// 3-4-5 triangle. Ground speed is derived from the components, not carried:
	// UDP_message has no velocity-norm field and expects the consumer to compute it.
	raw, err := gnss.Decode(
		mustEncode(t, gnss.RawFix{Latitude: 1, Longitude: 1, Vnorth: 3, Veast: 4}),
		1, time.Now())
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
	// Built by hand because every case here is malformed on purpose and must not
	// go through the encoder, which refuses to produce them.
	valid := func() []byte {
		b := make([]byte, gnss.DatagramSize)
		b[0] = gnss.StartMarker
		b[len(b)-1] = gnss.EndMarker
		putF64(b, 13, 38.7)
		putF64(b, 21, -9.1)
		return b
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

// mustEncode is the only datagram builder the tests need: the encoder IS the
// definition of the layout, and a second one here could disagree with it.
func mustEncode(t *testing.T, f gnss.RawFix) []byte {
	t.Helper()
	b, err := gnss.Encode(f)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	return b
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
