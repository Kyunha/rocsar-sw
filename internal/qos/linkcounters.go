package qos

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// sysfsNetPath is where the kernel publishes per-interface byte counters. It is
// a documented, stable interface, readable without any capability, which is why
// the measurement is done here rather than through rtnetlink: the netlink route
// would add an attribute-parse path for no benefit, and this file is the only
// thing that reads it.
const sysfsNetPath = "/sys/class/net/"

// minSampleInterval is the shortest gap between two counter reads that becomes a
// rate.
//
// Status() is called from more than the telemetry tick. The command dispatcher
// reads it for the device name, and Verify reads it while checking the qdisc. A
// rate differenced over the ~10 ms between two such calls is a spike with no
// meaning -- one packet over 10 ms reads as a busy link. Below this interval the
// previous rate is returned unchanged, so an extra caller is free and cannot
// distort the number. The telemetry interval is 1 s, so the tick always clears
// it.
const minSampleInterval = 500 * time.Millisecond

// LinkCounters turns an interface's byte counters into a throughput rate.
//
// It differences consecutive reads of
// /sys/class/net/<device>/statistics/{tx,rx}_bytes against the wall clock. The
// counters are monotonic since the interface came up, so a decrease means the
// interface was reset (or, in principle, the counter wrapped); that is reported
// as no measurement rather than as a negative rate.
//
// The zero value is usable. A nil device, an unreadable counter, a counter that
// went backwards, or the very first sample all report nil -- absent, not zero.
// A genuine zero is a real reading: an idle link. The distinction is the whole
// point (GUI_ARCHITECTURE.md 7.1), and the console draws absent as "no reading"
// rather than as a bar of length zero.
type LinkCounters struct {
	mu     sync.Mutex
	device string
	prevTx uint64
	prevRx uint64
	prevAt time.Time
	// last is what was returned the last time a rate was computed, so a caller
	// inside minSampleInterval gets the same number rather than a fresh spike.
	lastTx *uint32
	lastRx *uint32
	// started is false until a first sample has established a baseline.
	started bool

	// read and now are seams for tests. Nil means the real sysfs read and the
	// real clock; production never sets them.
	read func(device, stat string) (uint64, bool)
	now  func() time.Time
}

// Sample returns the measured rate in kbit/s, or (nil, nil) when there is no
// measurement to report.
func (c *LinkCounters) Sample(device string) (*uint32, *uint32) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// A different interface invalidates the baseline: differencing one device's
	// counters against another's is meaningless.
	if device != c.device {
		c.device = device
		c.started = false
		c.lastTx, c.lastRx = nil, nil
	}
	if c.device == "" {
		return nil, nil
	}

	read := c.read
	if read == nil {
		read = readCounter
	}
	now := c.clock()

	tx, okTx := read(c.device, "tx_bytes")
	rx, okRx := read(c.device, "rx_bytes")
	if !okTx || !okRx {
		// Unreadable is not zero. Drop the baseline so the next successful read
		// starts fresh instead of differencing across the gap.
		c.started = false
		c.lastTx, c.lastRx = nil, nil
		return nil, nil
	}

	if !c.started {
		c.prevTx, c.prevRx, c.prevAt = tx, rx, now
		c.started = true
		return nil, nil
	}

	if now.Sub(c.prevAt) < minSampleInterval {
		return c.lastTx, c.lastRx
	}

	if tx < c.prevTx || rx < c.prevRx {
		// The interface was reset. Re-baseline and report nothing: the delta is
		// not a rate, and a negative one would be worse than an absent one.
		c.prevTx, c.prevRx, c.prevAt = tx, rx, now
		c.lastTx, c.lastRx = nil, nil
		return nil, nil
	}

	txRate := rateKbps(tx-c.prevTx, now.Sub(c.prevAt))
	rxRate := rateKbps(rx-c.prevRx, now.Sub(c.prevAt))
	c.prevTx, c.prevRx, c.prevAt = tx, rx, now
	c.lastTx, c.lastRx = &txRate, &rxRate
	return c.lastTx, c.lastRx
}

func (c *LinkCounters) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// rateKbps converts a byte count over an elapsed duration to kbit/s, rounding to
// the nearest. The unit is kbit/s because that is the unit of the cap it is
// compared against (link.rate_kbps) and the unit the operator sets; converting
// anywhere else is where an 8x error comes from.
func rateKbps(bytes uint64, elapsed time.Duration) uint32 {
	if elapsed <= 0 {
		return 0
	}
	kbps := (float64(bytes) * 8) / elapsed.Seconds() / 1000
	if kbps <= 0 {
		return 0
	}
	if kbps >= float64(^uint32(0)) {
		return ^uint32(0)
	}
	return uint32(kbps + 0.5)
}

// readCounter reads one sysfs counter. A missing file (no such interface) and a
// malformed value are both "no reading".
func readCounter(device, stat string) (uint64, bool) {
	b, err := os.ReadFile(sysfsNetPath + device + "/statistics/" + stat)
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}
