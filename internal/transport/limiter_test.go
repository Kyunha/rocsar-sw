package transport

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rocsar/obc/internal/storage"
)

// The limiter exists because the kernel could not do this job: a tc flower filter
// was shipped, asserted by a test, and never classified a single packet (three
// separate faults, ARCHITECTURE.md 6.6). So the bound has to be asserted here, or
// it is not asserted at all.

// A rate that is actually applied.
//
// Without a fake clock this would have to transfer a megabyte to measure, so the
// clock is injected and the arithmetic is checked directly.
func TestBulkLimiterGrantsAtTheConfiguredRate(t *testing.T) {
	// 1000 B/s. Burst and initial fill are both 1000, and the quantum is the
	// 512 floor (1000/20 would be 50).
	l := newBulkLimiter(1000)
	quantum := l.quantum()
	if quantum != 512 {
		t.Fatalf("quantum = %d, want 512; this test's arithmetic assumes it", quantum)
	}

	now := time.Unix(0, 0)
	l.now = func() time.Time { return now }
	var slept []time.Duration
	l.sleep = func(d time.Duration) { slept = append(slept, d) }

	// Start full: one quantum, then nothing until time passes.
	if got := l.take(1 << 20); got != quantum {
		t.Fatalf("first grant = %d, want one quantum (%d)", got, quantum)
	}
	if got := l.take(1 << 20); got != 0 {
		t.Errorf("a drained bucket granted %d bytes; it should grant nothing until it refills", got)
	}

	// Measure the refill from empty. Draining by calling take() does not work:
	// take() returns 0 while the bucket is below the quantum, so it cannot consume
	// the remainder. Zero it directly instead -- this is a white-box test and a
	// deterministic starting state is worth more here than exercising the drain.
	l.mu.Lock()
	l.tokens = 0
	l.last = now
	l.mu.Unlock()

	// Short of a quantum: 400 ms refills 400 of the 512 needed.
	now = now.Add(400 * time.Millisecond)
	if got := l.take(1 << 20); got != 0 {
		t.Errorf("at 400 tokens the grant was %d, want 0 -- the quantum is %d", got, quantum)
	}

	// Another 200 ms crosses it.
	now = now.Add(200 * time.Millisecond)
	if got := l.take(1 << 20); got != quantum {
		t.Errorf("at 600 tokens the grant was %d, want one quantum (%d)", got, quantum)
	}

	// It must never grant more than the caller asked for, or a small remaining
	// range read would be padded. Refill past the quantum first.
	now = now.Add(time.Second)
	if got := l.take(10); got != 10 {
		t.Errorf("a request for 10 bytes was granted %d, want 10", got)
	}
	_ = slept // asserted at the reader level, where the loop that would spin lives
}

// The reader must not busy-loop when the bucket is empty.
//
// io.Copy treats a zero-length read with no error as "call me again", so a reader
// that returned (0, nil) instead of waiting would spin the CPU at 100% -- on the
// flight computer, while holding a socket the radio link depends on. This
// exercises the loop that actually runs.
func TestBoundedReaderWaitsInsteadOfSpinning(t *testing.T) {
	l := newBulkLimiter(1000)

	now := time.Unix(0, 0)
	l.now = func() time.Time { return now }

	var sleeps []time.Duration
	l.sleep = func(d time.Duration) {
		sleeps = append(sleeps, d)
		// Advance the clock so the loop terminates; real time would do this.
		now = now.Add(d)
	}

	// Empty the bucket so the first Read has to wait.
	l.mu.Lock()
	l.tokens = 0
	l.last = now
	l.mu.Unlock()

	src := strings.NewReader("0123456789")
	buf := make([]byte, 4)

	n, err := l.reader(src).Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Fatalf("read %d bytes, want 4", n)
	}
	if len(sleeps) == 0 {
		t.Fatal("the reader returned without sleeping; a drained bucket would spin")
	}
	// A quantum at 1000 B/s is 512 ms. Sleeping one byte-time instead would mean
	// 8192 syscalls a second at the configured 8 KiB/s.
	if sleeps[0] != 512*time.Millisecond {
		t.Errorf("waited %s, want 512ms -- one quantum at 1000 B/s", sleeps[0])
	}
}

// The bucket is capped at one second's worth, so a long idle period does not
// bank credit that lets the next download run wide open.
func TestBulkLimiterCapsTheBurst(t *testing.T) {
	l := newBulkLimiter(1000)

	now := time.Unix(0, 0)
	l.now = func() time.Time { return now }
	l.sleep = func(time.Duration) {}

	// An hour of idleness.
	now = now.Add(time.Hour)

	// Burst is 1000, so at most 1000 bytes are available however long we waited.
	total := 0
	for {
		n := l.take(1 << 20)
		if n == 0 {
			break
		}
		total += n
	}
	if total > 1000 {
		t.Errorf("an hour of idleness banked %d bytes; the burst cap is 1000", total)
	}
}

// Two downloads share one rate, because the constraint is the link and not the
// request. Separate buckets would let N concurrent fetches through at N times the
// configured rate, which is the failure this was added to prevent.
func TestTwoConcurrentDownloadsShareOneRate(t *testing.T) {
	l := newBulkLimiter(1000)

	now := time.Unix(0, 0)
	l.now = func() time.Time { return now }
	l.sleep = func(time.Duration) {}

	if a, b := l.take(1<<20), l.take(1<<20); a+b > 1000 {
		t.Errorf("two readers were granted %d+%d = %d bytes at once; the burst is 1000",
			a, b, a+b)
	}
}

// zero means unbounded, and must be usable without a nil check at every call
// site -- a laptop on wifi is the normal case for this tool, not an edge case.
func TestZeroBytesPerSecondIsUnbounded(t *testing.T) {
	for _, rate := range []int{0, -1} {
		l := newBulkLimiter(rate)
		if l != nil {
			t.Errorf("newBulkLimiter(%d) returned a limiter; it should return nil", rate)
		}
		if got := l.String(); got != "unbounded" {
			t.Errorf("String() on a nil limiter = %q, want %q", got, "unbounded")
		}
		// The reader must pass the source straight through.
		src := strings.NewReader("hello")
		buf := make([]byte, 5)
		n, err := l.reader(src).Read(buf)
		if err != nil || n != 5 {
			t.Errorf("nil limiter did not pass the read through: n=%d err=%v", n, err)
		}
	}
}

// The quantum exists because byte-granular pacing is worse than no limiter at
// all: 8 KiB/s becomes 8192 one-byte reads and 8192 syscalls a second.
func TestQuantumKeepsTheSyscallRateSane(t *testing.T) {
	for _, rate := range []int{1, 100, 1000, 8192, 1024 * 1024} {
		l := newBulkLimiter(rate)
		q := l.quantum()
		if q < 512 {
			t.Errorf("rate %d: quantum is %d, below the 512 floor", rate, q)
		}
		readsPerSecond := rate / q
		if readsPerSecond > 64 {
			t.Errorf("rate %d: quantum %d means %d reads/second, which is too many",
				rate, q, readsPerSecond)
		}
	}
}

// End to end through the handler: a bounded server must still deliver every
// byte, unchanged. A limiter that corrupted or truncated the stream would be far
// worse than the problem it solves.
func TestBoundedArtefactIsDeliveredIntact(t *testing.T) {
	dir := t.TempDir()
	store := storage.New(dir)

	body := make([]byte, 300_000)
	for i := range body {
		body[i] = byte(i % 251)
	}
	if err := store.WriteFileAtomic("big.bin", body, 0o644); err != nil {
		t.Fatal(err)
	}

	// A rate high enough to finish quickly but low enough to actually engage the
	// bucket mid-transfer.
	srv := httptest.NewServer(NewFileHandler(store, nil, 200_000))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/" + filepath.Base("big.bin"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != len(body) {
		t.Fatalf("got %d bytes, want %d", len(got), len(body))
	}
	for i := range got {
		if got[i] != body[i] {
			t.Fatalf("byte %d differs: got %02x want %02x", i, got[i], body[i])
		}
	}
}

// A Range request goes through the same bounded path and must return exactly the
// requested slice. This is the path the Ground Station uses to resume a download.
func TestBoundedRangeRequestReturnsTheRightSlice(t *testing.T) {
	dir := t.TempDir()
	store := storage.New(dir)

	body := make([]byte, 100_000)
	for i := range body {
		body[i] = byte(i % 97)
	}
	if err := store.WriteFileAtomic("r.bin", body, 0o644); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(NewFileHandler(store, nil, 50_000))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/r.bin", nil)
	req.Header.Set("Range", "bytes=1000-1999")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status %d, want 206", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if len(got) != 1000 {
		t.Fatalf("got %d bytes, want 1000", len(got))
	}
	for i := range got {
		if got[i] != body[1000+i] {
			t.Fatalf("slice differs at %d: got %02x want %02x", i, got[i], body[1000+i])
		}
	}
}
