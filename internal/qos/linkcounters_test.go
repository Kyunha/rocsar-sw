package qos

import (
	"testing"
	"time"
)

// fakeCounters drives a LinkCounters from a settable byte count and clock, so
// the arithmetic can be tested without a real interface. The seams are the
// unexported read/now fields; production never sets them.
func fakeCounters(tx, rx *uint64, now *time.Time) *LinkCounters {
	c := &LinkCounters{}
	c.now = func() time.Time { return *now }
	c.read = func(_, stat string) (uint64, bool) {
		if stat == "tx_bytes" {
			return *tx, true
		}
		return *rx, true
	}
	return c
}

func TestRateKbpsIsBitsNotBytes(t *testing.T) {
	// 1000 bytes in one second is 8000 bit/s, which is 8 kbit/s. Getting this
	// wrong by a factor of 8 is the documented way this unit has been broken.
	if got := rateKbps(1000, time.Second); got != 8 {
		t.Fatalf("1000 B/s = 8 kbit/s, got %d", got)
	}
	// Half a second doubles the rate for the same byte count.
	if got := rateKbps(1000, 500*time.Millisecond); got != 16 {
		t.Fatalf("1000 B in 0.5 s = 16 kbit/s, got %d", got)
	}
	// Zero elapsed is not a division.
	if got := rateKbps(1000, 0); got != 0 {
		t.Fatalf("zero elapsed should be 0, got %d", got)
	}
}

func TestFirstSampleIsAbsentNotZero(t *testing.T) {
	var tx, rx uint64 = 5000, 4000
	now := time.Unix(1_700_000_000, 0)
	c := fakeCounters(&tx, &rx, &now)

	gotTx, gotRx := c.Sample("eth0")
	if gotTx != nil || gotRx != nil {
		t.Fatalf("first sample has nothing to difference against; want absent, got %v/%v", gotTx, gotRx)
	}
}

func TestSampleDeltasOverTheInterval(t *testing.T) {
	var tx, rx uint64
	now := time.Unix(1_700_000_000, 0)
	c := fakeCounters(&tx, &rx, &now)

	c.Sample("eth0") // baseline

	tx, rx = 125_000, 0 // 125 000 B/s = 1000 kbit/s; rx idle
	now = now.Add(time.Second)

	gotTx, gotRx := c.Sample("eth0")
	if gotTx == nil || *gotTx != 1000 {
		t.Fatalf("want tx 1000 kbit/s, got %v", gotTx)
	}
	// A measured zero is a real reading -- an idle link -- and must be reported,
	// not turned into absence.
	if gotRx == nil || *gotRx != 0 {
		t.Fatalf("want rx 0 kbit/s reported, got %v", gotRx)
	}
}

func TestSampleWithinTheMinimumIntervalIsCached(t *testing.T) {
	var tx, rx uint64
	now := time.Unix(1_700_000_000, 0)
	c := fakeCounters(&tx, &rx, &now)

	c.Sample("eth0")
	tx = 125_000
	now = now.Add(time.Second)
	first, _ := c.Sample("eth0")

	// A second caller 100 ms later -- the dispatcher reading the device name, or
	// Verify reading the qdisc -- must not manufacture a spike from the tiny
	// interval.
	tx += 125_000
	now = now.Add(100 * time.Millisecond)
	second, _ := c.Sample("eth0")
	if second == nil || first == nil || *second != *first {
		t.Fatalf("a call inside the minimum interval should return the cached rate %v, got %v", first, second)
	}
}

func TestCounterDecreaseIsAbsentNotNegative(t *testing.T) {
	var tx, rx uint64
	now := time.Unix(1_700_000_000, 0)
	c := fakeCounters(&tx, &rx, &now)

	tx = 1_000_000
	c.Sample("eth0")
	now = now.Add(time.Second)
	c.Sample("eth0")

	// Interface reset: the counters start again from zero.
	tx, rx = 5, 5
	now = now.Add(time.Second)
	gotTx, gotRx := c.Sample("eth0")
	if gotTx != nil || gotRx != nil {
		t.Fatalf("a reset counter is not a rate; want absent, got %v/%v", gotTx, gotRx)
	}
}

func TestUnreadableCountersAreAbsent(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c := &LinkCounters{}
	c.now = func() time.Time { return now }
	c.read = func(_, _ string) (uint64, bool) { return 0, false }

	if tx, rx := c.Sample("eth0"); tx != nil || rx != nil {
		t.Fatalf("unreadable counters must be absent, got %v/%v", tx, rx)
	}
}

func TestEmptyDeviceIsAbsent(t *testing.T) {
	var tx, rx uint64
	now := time.Unix(1_700_000_000, 0)
	c := fakeCounters(&tx, &rx, &now)
	if gotTx, gotRx := c.Sample(""); gotTx != nil || gotRx != nil {
		t.Fatalf("no device is no measurement, got %v/%v", gotTx, gotRx)
	}
}

func TestChangingDeviceDiscardsTheBaseline(t *testing.T) {
	var tx, rx uint64
	now := time.Unix(1_700_000_000, 0)
	c := fakeCounters(&tx, &rx, &now)

	tx = 1000
	c.Sample("eth0")
	now = now.Add(time.Second)

	// A different interface's counters cannot be differenced against eth0's.
	if gotTx, _ := c.Sample("wlan0"); gotTx != nil {
		t.Fatalf("device change should re-baseline; want absent, got %v", gotTx)
	}
}

// TestNullShaperStillMeasures is the regression this design exists to prevent:
// with shaping OFF the shaper installs nothing, but the interface can still be
// measured, and an unshaped link is exactly where throughput matters most.
func TestNullShaperStillMeasures(t *testing.T) {
	n := NewNullShaper("link shaping disabled by configuration")
	n.Configure("eth0", 115)

	var tx, rx uint64
	now := time.Unix(1_700_000_000, 0)
	n.counters.now = func() time.Time { return now }
	n.counters.read = func(_, stat string) (uint64, bool) {
		if stat == "tx_bytes" {
			return tx, true
		}
		return rx, true
	}

	n.Status() // baseline
	tx = 125_000
	now = now.Add(time.Second)

	st := n.Status()
	if st.MeasuredTxKbps == nil || *st.MeasuredTxKbps != 1000 {
		t.Fatalf("NullShaper must report measured throughput; got %v", st.MeasuredTxKbps)
	}
	if st.RateKbps != 115 || st.ShapingActive {
		t.Fatalf("NullShaper must still report the intended cap and that it is inactive: %+v", st)
	}
}
