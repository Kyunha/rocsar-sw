package transport

import (
	"fmt"
	"io"
	"sync"
	"time"
)

// bulkLimiter bounds artefact downloads by back-pressure.
//
// WHY IT EXISTS
// -------------
// The link is a shared 115 kbit/s radio. The kernel cannot tell an artefact
// download from telemetry on this interface -- see ARCHITECTURE.md 6.6 for the
// three separate faults that stopped the tc flower filter from ever classifying
// anything -- so nothing was stopping a photo fetch from consuming the whole link
// while telemetry starved.
//
// This bounds the traffic this process produces, which is the traffic that was
// actually causing it: artefact bytes, served by the HTTP handler below.
//
// WHAT IT IS NOT
// --------------
// It is not link shaping. It cannot constrain the SDR, a system service, the
// Ground Station's own uploads, or anything else on the box. It back-pressures
// rather than prevents: the kernel socket buffer fills, TCP stops reading from
// the file, and the download slows. Bytes already in flight are not recalled.
//
// The honest summary is that this bounds the one producer we can see, and the
// deferred link module is what actually bounds the link.
//
// A TOKEN BUCKET, not a fixed sleep
// ---------------------------------
// A token bucket lets a short burst through and then settles to the configured
// rate, so the first packet of a small file is not delayed and a burst of small
// artefact requests is not punished. Sleeping a fixed interval per chunk would
// add latency to every response and would still be wrong at the boundaries.
//
// Per-goroutine state, shared bucket. Two concurrent downloads share the rate
// rather than each getting it, because the constraint is the link and not the
// request.
type bulkLimiter struct {
	bytesPerSec int
	burst       int

	mu     sync.Mutex
	tokens float64
	last   time.Time

	// now is swapped in tests so a rate can be asserted without waiting for it.
	now func() time.Time
	// sleep is swapped in tests for the same reason.
	sleep func(time.Duration)
}

// newBulkLimiter returns a limiter, or nil when bytesPerSec is not positive.
//
// nil is a valid limiter: it means unbounded, and it is what a caller that has
// not been told a rate gets. Every method below tolerates a nil receiver, so
// there is no "is it configured" branch at each call site.
func newBulkLimiter(bytesPerSec int) *bulkLimiter {
	if bytesPerSec <= 0 {
		return nil
	}
	return &bulkLimiter{
		bytesPerSec: bytesPerSec,
		// One second's worth of burst. Enough that a small file completes without
		// being shaped, small enough that a bulk transfer cannot open a gap wide
		// enough to matter at 115 kbit/s.
		burst:  bytesPerSec,
		tokens: float64(bytesPerSec),
		last:   time.Now(),
		now:    time.Now,
		sleep:  time.Sleep,
	}
}

// reader wraps src so reads are paced by the bucket.
func (l *bulkLimiter) reader(src io.Reader) io.Reader {
	if l == nil {
		return src
	}
	return &limitedReader{lim: l, r: src}
}

type limitedReader struct {
	lim *bulkLimiter
	r   io.Reader
}

func (lr *limitedReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	// Loop until the bucket yields something. Returning (0, nil) instead would
	// make io.Copy call straight back in -- it treats a zero-length read with no
	// error as "try again" -- so a drained bucket would spin the CPU at 100%
	// rather than sleeping.
	for {
		if n := lr.lim.take(len(p)); n > 0 {
			return lr.r.Read(p[:n])
		}
		lr.lim.wait()
	}
}

// quantum is the smallest grant.
//
// Without it the pacing degenerates. The bucket refills continuously, so the
// first moment it holds a single byte it grants exactly one byte, and the next
// Read finds an empty bucket again: 8 KiB/s becomes 8192 one-byte reads and 8192
// syscalls a second, which is slower than the unshaped path it was added to
// improve. A grant of roughly 50 ms worth keeps the syscall rate around 20/s at
// any configured rate, and the floor stops a very low rate from regressing to
// byte-at-a-time.
func (l *bulkLimiter) quantum() int {
	q := l.bytesPerSec / 20
	if q < 512 {
		q = 512
	}
	return q
}

// take consumes up to want bytes from the bucket and returns how many were
// granted: a whole quantum, or less if that is all the caller asked for, or 0
// when the bucket has not accumulated enough yet.
func (l *bulkLimiter) take(want int) int {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	if elapsed := now.Sub(l.last); elapsed > 0 {
		l.tokens += elapsed.Seconds() * float64(l.bytesPerSec)
		if l.tokens > float64(l.burst) {
			l.tokens = float64(l.burst)
		}
	}
	l.last = now

	q := l.quantum()
	if l.tokens < float64(q) {
		return 0
	}
	granted := q
	if want < granted {
		granted = want
	}
	l.tokens -= float64(granted)
	return granted
}

// wait sleeps for the time it takes the bucket to refill one quantum.
//
// Derived from the configured rate rather than hardcoded, so it stays correct if
// the rate changes, and bounded below so a pathologically small rate cannot turn
// this into a spin.
func (l *bulkLimiter) wait() {
	l.mu.Lock()
	perQuantum := time.Duration(int64(time.Second) * int64(l.quantum()) / int64(l.bytesPerSec))
	l.mu.Unlock()
	if perQuantum < time.Millisecond {
		perQuantum = time.Millisecond
	}
	l.sleep(perQuantum)
}

// String describes the limit for logs and telemetry.
func (l *bulkLimiter) String() string {
	if l == nil {
		return "unbounded"
	}
	return fmt.Sprintf("%d B/s (burst %d B)", l.bytesPerSec, l.burst)
}
