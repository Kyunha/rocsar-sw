package transport

import (
	"context"
	"fmt"
	"io"

	"golang.org/x/time/rate"
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
// Per-request state, shared bucket. Two concurrent downloads share the rate
// rather than each getting it, because the constraint is the link and not the
// request. That is rate.Limiter's concurrency behaviour, which is why it is the
// library's and not ours.
//
// This used to be a hand-rolled bucket with an injected clock. x/time/rate is the
// same algorithm with the accounting already reviewed, so what is left here is
// the part that is specific to this link: the quantum.
type bulkLimiter struct {
	bytesPerSec int
	burst       int
	quantum     int
	lim         *rate.Limiter
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
	// One second's worth of burst. Enough that a small file completes without
	// being shaped, small enough that a bulk transfer cannot open a gap wide
	// enough to matter at 115 kbit/s.
	burst := bytesPerSec
	return &bulkLimiter{
		bytesPerSec: bytesPerSec,
		burst:       burst,
		quantum:     quantumFor(bytesPerSec),
		lim:         rate.NewLimiter(rate.Limit(bytesPerSec), burst),
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
	// Take a quantum, not len(p). io.Copy hands over a 32 KiB buffer, and waiting
	// for all of it at the configured 8 KiB/s would mean a four-second stall
	// before the client's first bytes, on a link where the client is waiting on
	// us.
	//
	// The quantum must also not exceed the burst. WaitN rejects n > burst rather
	// than granting it, and at a rate below the quantum floor -- a rate the
	// configuration allows -- the burst IS the smaller number, so capping here is
	// what keeps a slow configured rate from becoming an error.
	n := min(len(p), lr.lim.quantum, lr.lim.burst)
	if err := lr.lim.lim.WaitN(context.Background(), n); err != nil {
		// Unreachable: n is capped at the burst above. Returning the error beats
		// silently truncating a download, and a caller that reaches this line
		// wants to know that the cap stopped being true.
		return 0, fmt.Errorf("transport: wait for %d bytes: %w", n, err)
	}
	return lr.r.Read(p[:n])
}

// quantumFor is the smallest grant, in bytes.
//
// Without a floor the pacing degenerates. The bucket refills continuously, so
// the first moment it holds a single byte it grants exactly one byte, and the
// next Read finds an empty bucket again: 8 KiB/s becomes 8192 one-byte reads and
// 8192 syscalls a second, which is slower than the unshaped path it was added to
// improve. A grant of roughly 50 ms worth keeps the syscall rate around 20/s at
// any configured rate, and the 512 floor stops a very low rate from regressing to
// byte-at-a-time.
func quantumFor(bytesPerSec int) int {
	return max(bytesPerSec/20, 512)
}

// String describes the limit for logs and telemetry.
func (l *bulkLimiter) String() string {
	if l == nil {
		return "unbounded"
	}
	return fmt.Sprintf("%d B/s (burst %d B)", l.bytesPerSec, l.burst)
}
