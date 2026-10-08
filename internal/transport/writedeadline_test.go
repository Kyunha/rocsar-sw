package transport

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rocsar/obc/internal/storage"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// pacedReader wraps src so reads are granted at bytesPerSec.
//
// Deliberately NOT the production limiter: that one is deleted by this change,
// and a regression test for the deadline must not depend on the code it outlives.
// What matters here is only that the transfer takes real time, so a plain
// time.Sleep between chunks is enough and has no moving parts.
type pacedReader struct {
	src         io.Reader
	bytesPerSec int
	last        time.Time
}

func (p *pacedReader) Read(b []byte) (int, error) {
	n, err := p.src.Read(b)
	if p.last.IsZero() {
		p.last = time.Now()
		return n, err
	}
	elapsed := time.Since(p.last)
	delay := time.Duration(float64(n)/float64(p.bytesPerSec)*float64(time.Second)) - elapsed
	if delay > 0 {
		time.Sleep(delay)
	}
	p.last = time.Now()
	return n, err
}

// A server write deadline truncates a paced transfer.
//
// THE mechanism behind a bug this file's sibling guards against. http.Server's
// WriteTimeout is a deadline on the connection, set when the request header is
// read and NOT reset per chunk, so a response paced slower than
// size/rate simply runs out of time partway through the body.
//
// It matters here because the link is 115 kbit/s and a SAR capture is not a
// web page: from the shipped params.json a burst is 200 us at 31.251 MS/s, so a
// one-second session at PRF 2750 is about 66 MiB, which at 8 KiB/s is hours. The
// production server carried a five-minute WriteTimeout, which at that rate
// truncates at about 2.3 MiB -- and it does so quietly, as a normal-looking
// transfer failure with a plausibly-sized file on disk.
//
// The deadline is set small here so the test runs in milliseconds. What is being
// pinned is the behaviour, not the constant.
func TestAServerWriteDeadlineTruncatesAPacedTransfer(t *testing.T) {
	body := strings.Repeat("x", 8<<10) // 8 KiB
	const bytesPerSec = 4 << 10        // 4 KiB/s: 2 s to send
	const deadline = 200 * time.Millisecond

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Content-Length is set, so a truncated body is detectable as a short
		// read rather than arriving as a clean end-of-stream.
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		_, _ = io.Copy(w, &pacedReader{src: strings.NewReader(body), bytesPerSec: bytesPerSec})
	}))
	defer srv.Close()
	srv.Config.WriteTimeout = deadline

	resp, err := http.Get(srv.URL) //nolint:gosec // loopback test server
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)

	if int64(len(got)) == int64(len(body)) {
		t.Fatalf("the whole %d-byte body arrived despite a %s write deadline; this test "+
			"can no longer demonstrate the truncation it exists to explain", len(body), deadline)
	}
	if err == nil {
		t.Errorf("body arrived short (%d of %d bytes) with no error reported", len(got), len(body))
	}
	t.Logf("truncated at %d of %d bytes after %s, as expected", len(got), len(body), deadline)
}

// The production artefact server must carry no write deadline.
//
// This is the regression guard. The five-minute WriteTimeout it replaces was
// there for a good reason -- an unbounded response body is an invitation -- and
// the reason it became wrong is that the link is rate-limited by the kernel, so
// any transfer big enough to matter takes hours and the deadline cuts it.
//
// What bounds a transfer now is the HTB cap on the interface, which is a rate
// rather than a duration. A client that stops reading holds a goroutine and a
// socket until the operator restarts the OBC, which is the trade and is recorded
// in ARCHITECTURE.md 6.6; IdleTimeout still reaps idle keep-alives.
func TestTheArtefactServerHasNoWriteDeadline(t *testing.T) {
	dir := t.TempDir()
	store := storage.New(dir, "")
	if err := store.WriteFileAtomic("a.bin", []byte("seed"), 0o644); err != nil {
		t.Fatal(err)
	}

	h := NewHTTP("127.0.0.1:0", store, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := h.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Stop()

	if got := h.srv.WriteTimeout; got != 0 {
		t.Errorf("WriteTimeout = %s, want 0 (no deadline)", got)
		t.Errorf("at the default link rate a write deadline truncates a capture long " +
			"before it finishes; the kernel cap is the rate bound, not a wall clock")
	}
	// The two timeouts that ARE about a stalled peer rather than a slow transfer
	// stay, because a slow transfer does not affect either.
	if h.srv.ReadHeaderTimeout == 0 {
		t.Error("ReadHeaderTimeout is unset; a client that opens a connection and says nothing holds a goroutine forever")
	}
	if h.srv.IdleTimeout == 0 {
		t.Error("IdleTimeout is unset; idle keep-alives accumulate")
	}
}

// A Range request still returns exactly the requested slice, with nothing paced.
//
// This and the traversal test below are the properties that outlive the in-server
// limiter, restated so that deleting it does not delete its coverage. Both used to
// be asserted alongside pacing tests; the pacing is gone and these are not.
func TestRangeStillReturnsTheRightSliceUnpaced(t *testing.T) {
	dir := t.TempDir()
	store := storage.New(dir, "")
	body := strings.Repeat("0123456789", 400) // 4000 bytes
	if err := store.WriteFileAtomic("slice.bin", []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(NewFileHandler(store, discardLogger()))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/slice.bin", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", "bytes=1000-1999")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", resp.StatusCode)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if want := body[1000:2000]; string(got) != want {
		t.Errorf("range returned %d bytes, want the %d requested", len(got), len(want))
	}
}

// A path escaping the root is still refused. Not limiter-related, but it lived in
// the same file and is worth keeping explicitly pinned.
func TestTraversalIsStillRefused(t *testing.T) {
	root := t.TempDir()
	store := storage.New(root, "")
	if err := store.WriteFileAtomic("inside.txt", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A real file outside the root, so the refusal cannot be an absent-file 404.
	if err := os.WriteFile(filepath.Join(filepath.Dir(root), "outside.txt"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(filepath.Join(filepath.Dir(root), "outside.txt"))

	srv := httptest.NewServer(NewFileHandler(store, discardLogger()))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/../outside.txt") //nolint:gosec // loopback test server
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusOK && strings.Contains(string(got), "secret") {
		t.Error("a path escaping the data root was served")
	}
}
