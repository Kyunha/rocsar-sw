package test

import (
	"strconv"
	"testing"

	"github.com/rocsar/obc/api/rocsar/v1"
	"github.com/rocsar/obc/internal/domain"
)

// domain.ErrorCode is a hand-maintained mirror of rocsar.v1.ErrorCode. The
// conversion between them is a bare integer cast (internal/pico/convert.go), so
// nothing about adding a value to the schema forces anything on this side: a new
// code simply arrives as an unnamed ErrorCode and String() falls through to a
// default arm.
//
// That is how ERROR_CALIBRATION_FAILED reached an operator as "ERROR_INTERNAL"
// while the console showed ERROR_CALIBRATION_FAILED in the same breath -- two
// names for one fault, in the message that matters most. So the mirror is pinned
// here instead.
func TestErrorCodesMirrorTheSchema(t *testing.T) {
	// Every wire value must render under its own name. An unnamed one means the
	// value still arrives correctly but is reported as something else.
	for value := 0; value <= 9; value++ {
		wire := rocsarv1.ErrorCode(value)
		want := wire.String()

		mirrored := domain.ErrorCode(value)
		if got := mirrored.String(); got != want {
			t.Errorf("ErrorCode(%d) renders as %q, schema says %q", value, got, want)
		}
	}

	// And the specific value that slipped through, named so the failure is
	// recognisable rather than a diff in a list.
	if got := domain.ErrCalibrationFailed.String(); got != "ERROR_CALIBRATION_FAILED" {
		t.Errorf("ErrCalibrationFailed = %q", got)
	}
	if got := domain.ErrCalibrationFailed; int(got) != int(rocsarv1.ErrorCode_ERROR_CALIBRATION_FAILED) {
		t.Errorf("ErrCalibrationFailed is %d, schema says %d", got,
			rocsarv1.ErrorCode_ERROR_CALIBRATION_FAILED)
	}
}

// A value from a firmware newer than this host must not be reported as one of the
// codes we know. Silence here would tell an operator the servo is uncalibrated
// when it said something else entirely.
func TestAnUnknownErrorCodeSaysSoRatherThanGuessing(t *testing.T) {
	got := domain.ErrorCode(200).String()
	if got == "ERROR_INTERNAL" || got == "ERROR_NONE" {
		t.Errorf("an unknown code renders as a known one: %q", got)
	}
	if got != "ERROR_UNKNOWN("+strconv.Itoa(200)+")" {
		t.Errorf("ErrorCode(200) = %q, want it to name the value", got)
	}
}

// The two codes that mean different things to an operator, distinguished by the
// firmware on purpose: ERROR_HARDWARE_FAULT means nothing was written, and
// ERROR_CALIBRATION_FAILED means the servo may now hold a change we cannot vouch
// for. Collapsing them loses the only fact that says whether to re-run `zero`.
func TestTheTwoTeachFailuresAreNotTheSameCode(t *testing.T) {
	if domain.ErrHardwareFault == domain.ErrCalibrationFailed {
		t.Fatal("a dead servo and a teach that did not take share an error code")
	}
	if domain.ErrHardwareFault.String() == domain.ErrCalibrationFailed.String() {
		t.Fatal("and they render identically")
	}
}
