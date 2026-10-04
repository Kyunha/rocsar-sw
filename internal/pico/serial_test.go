package pico

import (
	"strings"
	"testing"
)

// The transport appears in the message an operator reads when the flight
// controller is missing, so that message has to be readable.
//
// It was not: with no String method, %s reflected the struct and printed the
// port handle and a format complaint next to the path.
func TestTransportStringIsReadable(t *testing.T) {
	s := NewSerialTransport("/dev/ttyACM0", DefaultBaudrate)

	// Unopened is the state an operator is most likely to see it in, since the
	// port is opened during startup.
	got := s.String()
	if strings.Contains(got, "%!") {
		t.Errorf("String() = %q, which contains a format complaint", got)
	}
	if !strings.Contains(got, "/dev/ttyACM0") {
		t.Errorf("String() = %q, want it to name the device", got)
	}
	if !strings.Contains(got, "unopened") {
		t.Errorf("String() = %q, want it to say the port is not open", got)
	}

	// The baud rate is kept deliberately: the ST3215 servo bus is also 115200,
	// and telling those two apart is why the constant has a comment.
	if !strings.Contains(got, "115200") {
		t.Errorf("String() = %q, want the baud rate", got)
	}
}
