package test

import (
	"strings"
	"testing"

	"github.com/rocsar/obc/internal/domain"
)

// The ST3215's status byte is the servo's own vocabulary, and it used to be
// rendered through domain.ErrorCode, whose names are all about commands. A servo
// reporting a bit was rendered as, for example, "ERROR_INVALID_COMMAND": the servo
// saying "I am in trouble" showed up as "you sent a bad command", which is the
// opposite of the right advice on an aircraft.
func TestServoFaultsAreNotNamedWithCommandErrors(t *testing.T) {
	for _, status := range []int32{0x01, 0x02, 0x04, 0x08, 0x10, 0x20, 0x40, 0x80, 0xFF} {
		for _, name := range domain.ServoFaults(status) {
			if strings.Contains(name, "COMMAND") || strings.HasPrefix(name, "ERROR_") {
				t.Errorf("status %#02x rendered as a command error: %q", status, name)
			}
		}
	}
}

// The hex is the part that is certainly true, and it must be there. It is the
// servo's own reply and this firmware does not read register 0x41, so the byte is
// reported rather than interpreted.
func TestServoFaultsReportTheRawByte(t *testing.T) {
	faults := domain.ServoFaults(0x21)
	if len(faults) == 0 || faults[0] != "0x21" {
		t.Fatalf("ServoFaults(0x21) = %v, want it to lead with the raw byte", faults)
	}
	// Bits 0 and 5 are set, so both must be listed.
	joined := strings.Join(faults, " ")
	if !strings.Contains(joined, "bit0") || !strings.Contains(joined, "bit5") {
		t.Errorf("ServoFaults(0x21) = %v, want bit0 and bit5 named", faults)
	}
}

// The bit names that appear are the vendor's, from register 0x41 -- and they carry
// a "?" because they are that register's meanings, not this byte's. An earlier
// version of this function invented six names (OVERHEATED, OVERLOADED,
// UNDERVOLTAGE, OVERVOLTAGE, ENCODER_ERROR, COOLING_DOWN), all six wrong against
// the vendor's list of Voltage/Sensor/Temperature/Current/Angle/Overload, and all
// six plausible enough to be believed. This asserts the list rather than the
// mapping so a future "improvement" back to guesses fails here.
func TestTheBitNamesAreTheVendorsAndAreMarkedAsUnconfirmed(t *testing.T) {
	want := []string{"VOLTAGE", "SENSOR", "TEMPERATURE", "CURRENT", "ANGLE", "OVERLOAD"}
	if len(domain.ServoFaultBitNames) != len(want) {
		t.Fatalf("ServoFaultBitNames has %d entries, want %d",
			len(domain.ServoFaultBitNames), len(want))
	}
	for i, name := range want {
		if domain.ServoFaultBitNames[i] != name {
			t.Errorf("ServoFaultBitNames[%d] = %q, want %q", i,
				domain.ServoFaultBitNames[i], name)
		}
	}
	faults := strings.Join(domain.ServoFaults(0x01), " ")
	if !strings.Contains(faults, "?") {
		t.Errorf("a named bit is presented as certain: %q", faults)
	}
}

// A fault the operator cannot see is the failure this path exists to avoid:
// common.proto describes this byte as the servo reporting overheat or an
// overloaded regulator "while answering a perfectly well-formed frame".
func TestAnyFaultBitStillSurfaces(t *testing.T) {
	for _, status := range []int32{0x20, 0x40, 0x80, 0xE0, 0xFF} {
		if faults := domain.ServoFaults(status); len(faults) == 0 {
			t.Errorf("ServoFaults(%#02x) reported nothing", status)
		}
	}
}

// Zero is "no fault" and is the only value that yields nothing.
func TestNoServoFaultReportsNothing(t *testing.T) {
	if faults := domain.ServoFaults(0); faults != nil {
		t.Errorf("ServoFaults(0) = %v, want nil", faults)
	}
}
