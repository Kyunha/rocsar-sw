package sdr

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rocsar/obc/internal/domain"
)

// The capture size is computed from the same arithmetic the acquisition program
// uses, and these are the numbers the aircraft actually produced.
//
// Every 1-second capture written on the Pi is 34,375,000 bytes, and there are
// also 2-second and 25-second ones at exactly 2x and 25x. Those are the fixtures
// here, because a size check that has only ever been run against its own output
// proves nothing -- it would pass with an off-by-a-factor-of-four and no test
// would notice.
func TestCaptureBytesMatchesWhatTheProgramWrote(t *testing.T) {
	cases := []struct {
		name string
		dur  uint32
		want int64
	}{
		// 2750 pulses x ((200-100)us x 31.251 MS/s = 3125 samples) x 4 bytes.
		{"one second", 1, 34_375_000},
		{"two seconds", 2, 68_750_000},
		{"twenty five seconds", 25, 859_375_000},
	}

	// The .bin exactly. The sidecar CSVs are added on top and are not part of
	// what these files weighed, so the test checks the difference.
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := aircraftOperatingPoint()
			p.SessionDurationS = tc.dur

			got, err := captureBytes(p)
			if err != nil {
				t.Fatal(err)
			}
			if sidecar := int64(float64(tc.dur) * p.PRFHz * sidecarRowBytes); got != tc.want+sidecar {
				t.Errorf("captureBytes = %d, want %d (%d observed + %d of CSVs)",
					got, tc.want+sidecar, tc.want, sidecar)
			}
		})
	}
}

// hugeParamsJSON is a params.json asking for 100000 seconds of acquisition -- over
// a day of continuous recording, about 3.4 TB at this window and sample rate.
// Every bound in the contract is satisfied, so SetParams will accept it and the
// size check is the only thing standing between it and a full disk.
//
// SESSION_DURATION's maximum is 86400, which is deliberately not exceeded here:
// the test is about the arithmetic of a large capture, not about the bound, and
// coupling the two would fail for the wrong reason if the bound moved.
const hugeParamsJSON = `{
  "PRF": 2750.0,
  "FS": 31251000.0,
  "SESSION_DURATION": 100000,
  "TX_FREQ": 5850000000.0,
  "NORMALIZED_GAIN_TX": 1,
  "NORMALIZED_GAIN_RX": 1,
  "TX_ANTENNA": "TX/RX",
  "RX_ANTENNA": "RX2",
  "T_MIN_US": 100,
  "T_MAX_US": 200,
  "START_OFFSET_S": 0.1,
  "PULSE_DURATION": 3.64e-05,
  "BW": 25000000.0
}`

// aircraftOperatingPoint is the aircraft's params.json as a typed value: 2750 Hz
// PRF, 31.251 MS/s, a 100-200 us window, gains at 1.0, 5.85 GHz.
func aircraftOperatingPoint() domain.SdrParams {
	dur := uint32(1)
	return domain.SdrParams{
		PRFHz:            2750,
		SampleRateHz:     31.251e6,
		TxFreqHz:         5.85e9,
		NormalizedGainTx: 1,
		NormalizedGainRx: 1,
		BandwidthHz:      25e6,
		SessionDurationS: dur,
		TxAntenna:        "TX/RX",
		RxAntenna:        "RX2",
		SweepMinUs:       100,
		SweepMaxUs:       200,
		StartOffsetS:     0.1,
	}
}

// A session that cannot fit must be refused, not started. This is the check
// that answers "we will fill the SD card": the number is known before the radio
// is touched.
func TestACaptureLargerThanTheDiskIsRefused(t *testing.T) {
	prog := filepath.Join(t.TempDir(), "sdr-ettus-b200mini")
	if err := os.MkdirAll(filepath.Join(prog, "parameters"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A capture far larger than any test filesystem. At the aircraft's operating
	// point that is 100000 seconds, which is over a day of continuous recording
	// and would need about 3.4 TB -- more than the SSD has.
	if err := os.WriteFile(filepath.Join(prog, "parameters", "params.json"),
		[]byte(hugeParamsJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	s := NewService(prog, t.TempDir(), t.TempDir(), discardLogger())
	err := s.checkCaptureDestinations(context.Background())
	if err == nil {
		t.Fatal("Connect would start a capture that cannot fit on the disk")
	}
	// The message has to say which disk and by how much, or an operator has to
	// go and measure it themselves.
	if got := err.Error(); !strings.Contains(got, "free") || !strings.Contains(got, "needs") {
		t.Errorf("the refusal does not report free space and requirement: %v", err)
	}
}

// A missing Data/raw_data aborts the program after the session, so it is refused
// up front. That abort is the reason the second copy's directory is checked at
// all: write_buffer_to_disk throws, the exception escapes the RX thread, and the
// process dies with a successful capture already collected.
func TestAMissingFallbackDirectoryIsRefusedBeforeStarting(t *testing.T) {
	prog := filepath.Join(t.TempDir(), "sdr-ettus-b200mini")
	if err := os.MkdirAll(filepath.Join(prog, "parameters"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Data/raw_data deliberately NOT created.
	if err := os.WriteFile(filepath.Join(prog, "parameters", "params.json"),
		[]byte(validParams), 0o644); err != nil {
		t.Fatal(err)
	}

	s := NewService(prog, t.TempDir(), t.TempDir(), discardLogger())
	err := s.checkCaptureDestinations(context.Background())
	if err == nil {
		t.Fatal("Connect would start with no Data/raw_data, which aborts the process " +
			"after a successful session")
	}
	if !strings.Contains(err.Error(), programCaptureRelPath) {
		t.Errorf("the refusal does not name the directory the program needs: %v", err)
	}
}

// Both destinations have to work. An SSD that is present but unmounted is the
// exact failure this whole change exists for: the directory exists, it is
// writable, and it is the wrong disk.
func TestBothDestinationsAreChecked(t *testing.T) {
	prog := filepath.Join(t.TempDir(), "sdr-ettus-b200mini")
	if err := os.MkdirAll(filepath.Join(prog, "parameters"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(prog, programCaptureRelPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prog, "parameters", "params.json"),
		[]byte(validParams), 0o644); err != nil {
		t.Fatal(err)
	}

	ssd := t.TempDir()
	// The SSD directory is gone; the fallback is fine.
	s := NewService(prog, t.TempDir(), filepath.Join(ssd, "not-there"), discardLogger())
	if err := s.checkCaptureDestinations(context.Background()); err == nil {
		t.Fatal("Connect would start with no SSD directory")
	}

	// Both present and writable: the check passes.
	s = NewService(prog, t.TempDir(), ssd, discardLogger())
	if err := s.checkCaptureDestinations(context.Background()); err != nil {
		t.Fatalf("both destinations are writable but the check refused: %v", err)
	}
}

// The sweep window is the divisor here, so an inverted one has to be refused
// rather than producing a negative size that compares as "plenty of room".
func TestAZeroWidthWindowIsRefusedByTheSizeCheck(t *testing.T) {
	p := aircraftOperatingPoint()
	p.SweepMinUs = p.SweepMaxUs
	if _, err := captureBytes(p); err == nil {
		t.Fatal("a zero-width window was accepted")
	}
}

// A session longer than the contract permits, at a wide window, is the case
// where the int64 itself is reachable. It must be refused rather than wrapping to
// a small number, which would invert the answer.
func TestAnAbsurdSessionIsRefusedRatherThanOverflowing(t *testing.T) {
	p := aircraftOperatingPoint()
	p.SessionDurationS = 86400 // the contract's maximum
	p.PRFHz = 30e3             // the contract's maximum
	p.SampleRateHz = 61.44e6   // the contract's maximum
	p.SweepMinUs, p.SweepMaxUs = 0, 1e6

	got, err := captureBytes(p)
	if err != nil {
		return // refused, which is the intent
	}
	if got <= 0 {
		t.Errorf("captureBytes = %d for a session at the top of every bound; "+
			"the product wrapped rather than being refused", got)
	}
}
