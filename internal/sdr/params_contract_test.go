package sdr

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rocsar/obc/internal/domain"
)

// The operating point the aircraft actually flies must satisfy every constraint
// the contract table states, and contain every key config.hpp reads.
//
// # Why this is a literal and not a file
//
// It used to read third_party/sdr-ettus-b200mini/parameters/params.json, and
// skip when that tree was absent -- which it always was, because third_party/ is
// gitignored as vendored upstream and this repository has never carried the
// program. The four tests built on that file were permanently skipped and had
// stopped checking anything without saying so.
//
// So the aircraft's parameters are written out here instead: the exact contents
// of /root/rocsar-rpi/sdr-ettus-b200mini/parameters/params.json as of the
// bistatic 5.85 GHz operating point. That keeps the guarantee the file-reading
// test gave -- the shipped values are checked against the real table, so an
// invented bound fails here -- without a vendored tree to go missing.
//
// What this cannot do is notice that the aircraft's file has been edited since.
// That is a deliberate trade: a check that always runs and pins known-good
// values is worth more than one that would run only where the program happens to
// be installed, which is nowhere in CI. Anyone changing the aircraft's
// parameters.json should update this literal in the same commit, and
// TestOperatingPointIsValid below is the place to notice.
//
// This is the test that stops a bound from being invented. The first version of
// this file asserted values against numbers nobody had checked; three of the
// seven turned out to be wrong against the Ettus datasheet (FS was 16x the
// hardware maximum, TX_FREQ's floor was 70x too low, BW was 18x too high).
const aircraftParamsJSON = `{
  "AUTO_CONNECT_ENABLED": 1,
  "AUTO_CONNECT_INTERVAL": 300,
  "TEMP_FETCH_INTERVAL": 1,
  "SERVER_LOG_FILE_ENABLED": 1,
  "PRF": 2750.0,
  "FS": 31251000.0,
  "SESSION_DURATION": 1,
  "TX_FREQ": 5850000000.0,
  "NORMALIZED_GAIN_TX": 1,
  "NORMALIZED_GAIN_RX": 1,
  "TX_ANTENNA": "TX/RX",
  "RX_ANTENNA": "RX2",
  "T_MIN_US": 100,
  "T_MAX_US": 200,
  "START_OFFSET_S": 0.1,
  "CLOCK_SOURCE": "internal",
  "TIME_SOURCE": "internal",
  "SYNC_TO_PPS": false,
  "PULSE_DURATION": 3.64e-05,
  "BW": 25000000.0
}`

func TestShippedParamsSatisfyTheContract(t *testing.T) {
	raw := map[string]any{}
	if err := json.Unmarshal([]byte(aircraftParamsJSON), &raw); err != nil {
		t.Fatalf("the aircraft's params.json does not parse: %v", err)
	}

	// 1. Every required key present. j.at() throws on a missing one, from inside
	//    main(), before any radio is initialised.
	for _, name := range RequiredNames() {
		if _, ok := raw[name]; !ok {
			t.Errorf("the aircraft's params.json is missing %q, which config.hpp reads "+
				"with j.at(); connect.cpp will throw at startup", name)
		}
	}

	// 2. Every numeric value inside its documented range.
	for _, k := range Keys {
		v, ok := raw[k.Name].(float64)
		if !ok {
			continue // a string parameter, or absent (covered above)
		}
		if err := ValidateValue(k.Name, v); err != nil {
			t.Errorf("the aircraft's params.json violates the contract: %v\n"+
				"  provenance: %s", err, k.Provenance)
		}
	}
}

// PULSE_DURATION is read by nobody: config.hpp has that line commented out, so
// exposing it as a control would be a knob that does nothing.
//
// The C++ side of this check -- reading config.hpp to see whether the line has
// been uncommented upstream -- is gone along with the vendored tree. What remains
// is the half that is still ours to enforce: the key must not be in the contract,
// because nothing reads it. If a future firmware revision uncomments it, this is
// the test that has to be revisited, and its failure message says so.
func TestPulseDurationIsReadByNobody(t *testing.T) {
	if _, ok := Lookup("PULSE_DURATION"); ok {
		t.Error("PULSE_DURATION is in the contract; config.hpp may now read it. " +
			"If it does, re-add it to domain.SdrParamsPatch and the SdrParams proto message.")
	}
}

// Every bound states where it came from. A bound with an empty Provenance is a
// guess wearing the costume of a fact, which is what this table replaced.
func TestEveryBoundHasProvenance(t *testing.T) {
	for _, k := range Keys {
		if k.Min == 0 && k.Max == 0 {
			continue // not a bounded numeric parameter
		}
		if strings.TrimSpace(k.Provenance) == "" {
			t.Errorf("key %q has bounds %g..%g and no stated provenance", k.Name, k.Min, k.Max)
		}
		if k.Min > k.Max {
			t.Errorf("key %q has Min %g above Max %g", k.Name, k.Min, k.Max)
		}
	}
}

// The datasheet's ceilings, asserted directly so a wrong number in the table is
// caught even if the shipped file happens to satisfy the wrong bound.
func TestDatasheetCeilings(t *testing.T) {
	cases := []struct {
		key     string
		wantMax float64
		cite    string
	}{
		{"FS", 61.44e6, "datasheet: ADC/DAC Sample Rate (max) 61.44 MS/s"},
		{"TX_FREQ", 6e9, "datasheet: RF coverage 70 MHz to 6 GHz"},
		{"BW", 56e6, "datasheet: up to 56 MHz instantaneous bandwidth"},
	}
	for _, c := range cases {
		k, ok := Lookup(c.key)
		if !ok {
			t.Errorf("%q is not in the contract", c.key)
			continue
		}
		if k.Max != c.wantMax {
			t.Errorf("%s Max = %g, want %g -- %s", c.key, k.Max, c.wantMax, c.cite)
		}
	}

	// The B200's floor, which the first version got wrong by a factor of 70.
	tx, _ := Lookup("TX_FREQ")
	if tx.Min != 70e6 {
		t.Errorf("TX_FREQ Min = %g, want 70e6 -- datasheet: RF coverage from 70 MHz", tx.Min)
	}
}

// The operating point must remain settable. A validator that rejects the
// system's own shipped configuration is worse than no validator.
func TestOperatingPointIsValid(t *testing.T) {
	// PRF 2750 Hz is the agreed operating point and is not expected to change.
	if err := ValidateValue("PRF", 2750); err != nil {
		t.Errorf("the operating point PRF = 2750 Hz is rejected: %v", err)
	}
	// And the other shipped values.
	for _, c := range []struct {
		key string
		v   float64
	}{
		{"FS", 31251000},
		{"TX_FREQ", 5800000000},
		{"NORMALIZED_GAIN_TX", 0.5},
		{"NORMALIZED_GAIN_RX", 0.5},
		{"BW", 25000000},
		{"SESSION_DURATION", 1},
	} {
		if err := ValidateValue(c.key, c.v); err != nil {
			t.Errorf("the operating point %s = %g is rejected: %v", c.key, c.v, err)
		}
	}
}

// Out-of-range and gross typos must be caught.
func TestBoundsRejectTypos(t *testing.T) {
	cases := []struct {
		key string
		v   float64
		why string
	}{
		{"FS", 312.51e6, "5x the hardware maximum: a transposed exponent"},
		{"FS", 1e9, "the bound this file replaced"},
		{"TX_FREQ", 50e6, "below the 70 MHz floor"},
		{"TX_FREQ", 6.5e9, "above the 6 GHz ceiling"},
		{"BW", 200e6, "far above the 56 MHz instantaneous bandwidth"},
		// 27 500 Hz -- a misplaced decimal, 10x the operating point -- is
		// deliberately NOT a case here: it is inside the agreed 1 kHz..30 kHz
		// range, and the bound is a typo guard rather than a mission
		// constraint. Asserting it would be rejected would mean tightening a
		// limit the operator chose. Above the ceiling instead.
		{"PRF", 275000, "a misplaced decimal, 100x the operating point"},
		{"PRF", 50, "a misplaced decimal, 1/55th of the operating point"},
		{"NORMALIZED_GAIN_TX", 1.5, "outside 0..1"},
		{"NORMALIZED_GAIN_RX", -0.1, "outside 0..1"},
		{"SESSION_DURATION", 0, "zero is not an acquisition"},
	}
	for _, c := range cases {
		t.Run(c.key, func(t *testing.T) {
			if err := ValidateValue(c.key, c.v); err == nil {
				t.Errorf("accepted %s = %g (%s)", c.key, c.v, c.why)
			}
		})
	}
}

// An unknown key is not an error. The shipped file may carry keys the C++ reads
// and we do not model, and this package is not the arbiter.
func TestUnknownKeyIsNotValidated(t *testing.T) {
	if err := ValidateValue("SOMETHING_NEW", 1e300); err != nil {
		t.Errorf("an unknown key was rejected: %v", err)
	}
}

// programConfigRelPath is a copy of a line of C++ we do not maintain:
//
//	const Config cfg = load_config("./../sdr-ettus-b200mini/parameters/params.json");
//
// It had a test that pinned both sides by reading that C++ file, and the test
// had been skipping for some time -- third_party/ is gitignored, this repository
// does not carry the program, so there was never a file there to read. A skipped
// pin is not a pin.
//
// What is checkable without the program is the half that is ours: the constant
// must resolve, relative to the program directory, to the file the Service edits.
// If it does not, programParamsPath's comparison is against a path the program
// never opens, and it would pass while checking nothing -- which is exactly the
// silent failure programParamsPath exists to prevent.
//
// The rest -- that connect.cpp still says what it said -- is a fact about the
// aircraft, not about this repository. When the program is upgraded there, read
// the line in connect.cpp and confirm it matches. The refusal is in place either
// way: programParamsPath compares this constant against paramsPath() on every
// start, so a program that changed its mind is caught in the air rather than
// trusted from here.
func TestTheConfigPathConstantResolvesToTheFileWeEdit(t *testing.T) {
	s := &Service{programDir: "/opt/vendor/sdr-ettus-b200mini"}

	got := filepath.Clean(filepath.Join(s.programDir, programConfigRelPath))
	want := s.paramsPath()
	if got != want {
		t.Errorf("programConfigRelPath resolves to %q but the Service edits %q.\n"+
			"  programParamsPath compares these on every start, so this mismatch means\n"+
			"  every check it performs is validating a file the program never reads.",
			got, want)
	}
}

// PULSE_DURATION must survive a partial update untouched, along with every other
// unmodelled key. Encoding the typed struct instead of the map would drop all
// five and the next ./connect would throw.
func TestPartialUpdatePreservesUnmodelledKeys(t *testing.T) {
	dir := t.TempDir()
	prog := filepath.Join(dir, "sdr-ettus-b200mini")
	if err := os.MkdirAll(filepath.Join(prog, "parameters"), 0o755); err != nil {
		t.Fatal(err)
	}

	// A file with every required key, two of them strings, plus the inert
	// PULSE_DURATION.
	seed := `{
  "PRF": 2750.0,
  "FS": 31251000.0,
  "TX_FREQ": 5800000000.0,
  "NORMALIZED_GAIN_TX": 0.5,
  "NORMALIZED_GAIN_RX": 0.5,
  "BW": 25000000.0,
  "SESSION_DURATION": 1,
  "T_MIN_US": 0,
  "T_MAX_US": 200,
  "START_OFFSET_S": 0.1,
  "TX_ANTENNA": "TX/RX",
  "RX_ANTENNA": "TX/RX",
  "PULSE_DURATION": 3.64e-05,
  "AUTO_CONNECT_ENABLED": 1,
  "TEMP_FETCH_INTERVAL": 20
}`
	path := filepath.Join(prog, "parameters", "params.json")
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}

	s := NewService(prog, dir, dir, discardLogger())
	prf := 3000.0
	if err := s.SetParams(context.Background(), domain.SdrParamsPatch{PRFHz: &prf}); err != nil {
		t.Fatalf("SetParams: %v", err)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw := map[string]any{}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}

	if got := raw["PRF"]; got != 3000.0 {
		t.Errorf("PRF = %v, want 3000 -- the patch was not applied", got)
	}

	// Everything the patch did not name must be byte-identical in value.
	for _, k := range []struct {
		key  string
		want any
	}{
		{"FS", 31251000.0},
		{"TX_FREQ", 5800000000.0},
		{"NORMALIZED_GAIN_TX", 0.5},
		{"BW", 25000000.0},
		{"SESSION_DURATION", 1.0}, // JSON round trip makes this a float
		{"T_MIN_US", 0.0},
		{"T_MAX_US", 200.0},
		{"START_OFFSET_S", 0.1},
		{"TX_ANTENNA", "TX/RX"},
		{"RX_ANTENNA", "TX/RX"},
		{"PULSE_DURATION", 3.64e-05},
		{"AUTO_CONNECT_ENABLED", 1.0},
		{"TEMP_FETCH_INTERVAL", 20.0},
	} {
		if got, ok := raw[k.key]; !ok {
			t.Errorf("%s was DROPPED by a partial update", k.key)
		} else if got != k.want {
			t.Errorf("%s = %v (%T), want %v (%T) -- a partial update must leave unmentioned keys alone",
				k.key, got, got, k.want, k.want)
		}
	}
}

// A file missing a required key must be refused, with the missing names, rather
// than being written back looking healthy.
func TestMissingRequiredKeyIsRefused(t *testing.T) {
	dir := t.TempDir()
	prog := filepath.Join(dir, "sdr-ettus-b200mini")
	if err := os.MkdirAll(filepath.Join(prog, "parameters"), 0o755); err != nil {
		t.Fatal(err)
	}
	// T_MAX_US and TX_ANTENNA absent.
	seed := `{"PRF": 2750.0, "FS": 31251000.0, "TX_FREQ": 5800000000.0,
  "NORMALIZED_GAIN_TX": 0.5, "NORMALIZED_GAIN_RX": 0.5, "BW": 25000000.0,
  "SESSION_DURATION": 1, "T_MIN_US": 0, "START_OFFSET_S": 0.1, "RX_ANTENNA": "TX/RX"}`
	if err := os.WriteFile(filepath.Join(prog, "parameters", "params.json"), []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}

	s := NewService(prog, dir, dir, discardLogger())
	prf := 3000.0
	err := s.SetParams(context.Background(), domain.SdrParamsPatch{PRFHz: &prf})
	if err == nil {
		t.Fatal("SetParams accepted a params.json missing keys config.hpp requires")
	}
	if !strings.Contains(err.Error(), "T_MAX_US") || !strings.Contains(err.Error(), "TX_ANTENNA") {
		t.Errorf("the error does not name the missing keys: %v", err)
	}
}

// Helpers kept here so the contract tests read as tests and not as setup.

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}
