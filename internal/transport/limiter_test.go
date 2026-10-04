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
//
// What is NOT tested here is the token arithmetic. It belongs to x/time/rate and
// has more users than this project. What is tested is the part specific to this
// link: the quantum, and the fact that a paced reader blocks and returns whole
// bytes.

// A rate that is actually applied.
//
// Measured rather than computed, because the bucket is now the library's and its
// internals are not ours to assert. What matters is that a reader which runs past
// the burst takes real time to get the rest, and returns the right bytes.
func TestBoundedReaderWaitsInsteadOfSpinning(t *testing.T) {
	// Burst 4096, quantum 512. The first 4096 bytes are banked; the 1536 after
	// them have to be earned at 4096 B/s, which is 375 ms.
	l := newBulkLimiter(4096)
	if l.quantum != 512 {
		t.Fatalf("quantum = %d, want 512; this test's arithmetic assumes it", l.quantum)
	}

	body := strings.Repeat("x", 4096+1536)
	src := strings.NewReader(body)

	start := time.Now()
	got, err := io.ReadAll(l.reader(src))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Fatalf("read %d bytes, want %d intact", len(got), len(body))
	}

	// Half the expected wait, to stay honest about how long this takes. The point
	// is that it is not instant: io.Copy treats (0, nil) as "call me again", so a
	// reader that returned without blocking would spin the CPU at 100% on the
	// flight computer while holding a socket the radio link depends on.
	if want := 375 * time.Millisecond / 2; elapsed < want {
		t.Errorf("read %d bytes past a 4096 B/s burst in %s, want at least %s -- "+
			"the reader is not waiting", len(got), elapsed, want)
	}
}

// A grant must fit inside the burst, or WaitN rejects it.
//
// This is a regression test, not a restatement of the code. The quantum has a 512
// byte floor and the burst is one second's worth, so any rate below 512 B/s has a
// quantum larger than its burst. The previous hand-rolled bucket could never
// accumulate a full quantum at such a rate -- tokens capped at the burst, take()
// returned 0, and the reader looped forever. Capping the grant at the burst is
// what makes a slow configured rate work at all.
func TestGrantIsCappedAtTheBurstAtLowRates(t *testing.T) {
	for _, rate := range []int{1, 100, 512} {
		l := newBulkLimiter(rate)
		if l.quantum <= l.burst {
			continue // no capping needed at this rate; nothing to prove
		}

		// A reader that reports the size of the buffer it was handed, so the grant
		// is observed rather than inferred.
		sized := &sizeReportingReader{r: strings.NewReader("some bytes to read")}

		start := time.Now()
		n, err := l.reader(sized).Read(make([]byte, 4096))
		if err != nil {
			t.Fatalf("rate %d: %v", rate, err)
		}
		elapsed := time.Since(start)

		if sized.got > l.burst {
			t.Errorf("rate %d: granted a read of %d bytes, above the burst of %d",
				rate, sized.got, l.burst)
		}
		if n == 0 {
			t.Errorf("rate %d: the reader returned 0 bytes rather than blocking until it could", rate)
		}
		// A rate of 1 B/s and a burst of 1 means this genuinely waits. Only assert
		// it moved at all, to keep the test quick.
		if elapsed <= 0 {
			t.Errorf("rate %d: the read took no time at all", rate)
		}
	}
}

type sizeReportingReader struct {
	r   io.Reader
	got int
}

func (s *sizeReportingReader) Read(p []byte) (int, error) {
	s.got = len(p)
	return s.r.Read(p)
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
		q := quantumFor(rate)
		if q < 512 {
			t.Errorf("rate %d: quantum is %d, below the 512 floor", rate, q)
		}
		// The syscall rate is governed by min(quantum, burst), because that is what
		// is actually granted.
		perRead := min(q, rate)
		readsPerSecond := rate / perRead
		if readsPerSecond > 64 {
			t.Errorf("rate %d: quantum %d means %d reads/second, which is too many",
				rate, perRead, readsPerSecond)
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
