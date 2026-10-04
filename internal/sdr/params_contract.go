package sdr

import (
	"fmt"
	"strconv"
)

// The parameters.json contract, in one place.
//
// # Why this file exists
//
// parameters.json is a frozen contract with C++ we do not maintain
// (third_party/sdr-ettus-b200mini). Two facts about it dominate everything here,
// and neither is obvious from reading our own code:
//
//  1. THE PROGRAM DOES NO VALIDATION OF ITS OWN. config.hpp's load_config() is
//     twelve `j.at("KEY").get<T>()` lines and not one range check. There is no
//     `if`. Every invalid value is handed straight to UHD, which throws with a
//     message about a USRP setting and not about our JSON.
//
//  2. j.at() THROWS IF THE KEY IS ABSENT. Not "returns zero" -- throws, from
//     inside main(), before any radio is initialised. A params.json missing one
//     key is a bricked SDR with no way back except editing the file by hand on
//     the aircraft.
//
// So the required/optional split below is the load-bearing part. The bounds come
// second: they exist to catch an operator typo before it reaches a device that
// will fail in a way that does not name the cause.

// Key is one entry of parameters.json.
type Key struct {
	// Name is the JSON key, exactly as the C++ spells it.
	Name string

	// Required is true if load_config() calls j.at() on it. A required key that
	// is missing throws at startup; removing one from this table without
	// changing the C++ is a way to brick the SDR.
	Required bool

	// Min and Max bound the value. Max is always a hardware ceiling, never a
	// preference: exceeding it fails in UHD. Min is a typo guard, not a mission
	// constraint, and is deliberately loose.
	//
	// A zero Min or Max means "not bounded here". Every entry states why in
	// Provenance.
	Min, Max float64

	// Provenance says where Min and Max come from. Every bound has one. A bound
	// with no stated source is a guess wearing the costume of a fact -- which is
	// what the first version of this file was.
	Provenance string
}

// Keys is the contract.
var Keys = []Key{
	{
		Name:       "PRF",
		Required:   true,
		Min:        1e3,
		Max:        3e4,
		Provenance: "Typo guard, NOT a mission constraint. The full-sweep figure is PRF >= 1/(T_MAX_US - T_MIN_US) = 5 kHz for the shipped 200 us window, and that is deliberately NOT enforced: the operating point is 2750 Hz (PRI 364 us), which is below it, because the burst window is intentionally shorter than the PRI. T_MIN_US = 0 is intentional. Max is the unambiguous-range limit c/(2*5km) = 30 kHz; the operating point of 2750 Hz implies 54.5 km, so Max is very conservative and will not reject it. Both are single constants and are safe to change together with the mission.",
	},
	{
		Name:       "FS",
		Required:   true,
		Min:        1e5,
		Max:        61.44e6,
		Provenance: "Datasheet: 'ADC Sample Rate (max) 61.44 MS/s' and 'DAC Sample Rate (max) 61.44 MS/s' (docs/b200-b210_spec_sheet.pdf). An earlier bound of 1e9 was 16x the hardware maximum. Min is a typo guard only; the B200 will coerce a lower rate.",
	},
	{
		Name:       "TX_FREQ",
		Required:   true,
		Min:        70e6,
		Max:        6e9,
		Provenance: "Datasheet: 'RF coverage from 70 MHz to 6 GHz'. An earlier Min of 1e6 was below the hardware floor by a factor of 70.",
	},
	{
		Name:       "BW",
		Required:   true,
		Min:        1e5,
		Max:        56e6,
		Provenance: "Datasheet: 'up to 56 MHz of instantaneous bandwidth'. An earlier Max of 1e9 was roughly 18x the hardware maximum.",
	},
	{
		Name:       "NORMALIZED_GAIN_TX",
		Required:   true,
		Min:        0,
		Max:        1,
		Provenance: "The datasheet names a 'Spartan6 FPGA PGA' but gives no dB figure. 0..1 is what the key's name means and is what UHD's normalised gain takes. Not datasheet-derived and does not pretend to be.",
	},
	{
		Name:       "NORMALIZED_GAIN_RX",
		Required:   true,
		Min:        0,
		Max:        1,
		Provenance: "As NORMALIZED_GAIN_TX. The datasheet gives no RX gain range.",
	},
	{
		Name:       "SESSION_DURATION",
		Required:   true,
		Min:        1,
		Max:        86400,
		Provenance: "A policy bound, not a hardware one: 1 second to 24 hours of acquisition. The upper limit exists so an operator cannot start a run that fills the SSD.",
	},
	{
		Name:       "T_MIN_US",
		Required:   true,
		Provenance: "Required by j.at() and NOT modelled in domain.SdrParams. A partial update must not drop it; test/RequiredKeysArePresent asserts it survives.",
	},
	{
		Name:       "T_MAX_US",
		Required:   true,
		Provenance: "Required by j.at() and NOT modelled. The sweep window's upper edge, in microseconds. T_MIN_US = 0 is intentional.",
	},
	{
		Name:       "START_OFFSET_S",
		Required:   true,
		Provenance: "Required by j.at() and NOT modelled. Seconds to wait before the first burst.",
	},
	{
		Name:       "TX_ANTENNA",
		Required:   true,
		Provenance: "Required by j.at() and NOT modelled. A string, not a number, so it has no bounds. 'TX/RX' in the shipped file.",
	},
	{
		Name:       "RX_ANTENNA",
		Required:   true,
		Provenance: "Required by j.at() and NOT modelled. A string. connect.cpp compares TX and RX antennas and warns when they are the same port.",
	},
}

// PULSE_DURATION is absent from this table on purpose.
//
// third_party/sdr-ettus-b200mini/parameters/config.hpp has:
//
//	// c.pulse_duration = j.at("PULSE_DURATION").get<double>();
//
// Commented out. The program does not read it, so a key we expose as settable is
// a control that does nothing. It was removed from domain.SdrParamsPatch and from
// the SdrParams proto message. The key itself stays in the shipped params.json
// -- harmless, and removing it would be an edit to a vendored file for no gain.

// keysByName indexes Keys.
var keysByName = func() map[string]Key {
	m := make(map[string]Key, len(Keys))
	for _, k := range Keys {
		m[k.Name] = k
	}
	return m
}()

// Lookup returns the contract entry for a JSON key.
func Lookup(name string) (Key, bool) {
	k, ok := keysByName[name]
	return k, ok
}

// RequiredNames lists every key load_config() will throw without, in the order
// they appear in config.hpp. Used by tools/sdr_bench validate and by
// test/RequiredKeysArePresent.
func RequiredNames() []string {
	var out []string
	for _, k := range Keys {
		if k.Required {
			out = append(out, k.Name)
		}
	}
	return out
}

// ValidateValue checks one key against its bounds.
//
// An unknown key is not an error: the shipped params.json may carry keys the C++
// ignores, and this package is not the arbiter of what the program may read.
// Whether a key is *required* is answered by Lookup's Required flag, and whether
// it is *bounds-checkable* by Min/Max being non-zero.
func ValidateValue(name string, v float64) error {
	k, ok := Lookup(name)
	if !ok {
		return nil
	}
	if k.Min == 0 && k.Max == 0 {
		return nil // not a numeric parameter, or deliberately unbounded
	}
	if v < k.Min || v > k.Max {
		return &BoundsError{Key: k, Value: v}
	}
	return nil
}

// BoundsError is a value outside its documented range.
type BoundsError struct {
	Key   Key
	Value float64
}

// Error names the key, the value, and the range, plus where the range came
// from. The provenance is included because "TX_FREQ = 50 MHz is outside
// 70 MHz..6 GHz" is actionable and "TX_FREQ = 50 MHz is invalid" is not.
func (e *BoundsError) Error() string {
	return fmt.Sprintf("sdr: %s = %s Hz is outside %g..%g (%s)",
		e.Key.Name,
		strconv.FormatFloat(e.Value, 'g', -1, 64),
		e.Key.Min, e.Key.Max,
		e.Key.Provenance)
}
