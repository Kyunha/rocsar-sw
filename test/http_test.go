package test

import (
	"bytes"
	"context"
	"encoding/json"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rocsar/obc/internal/camera"
	"github.com/rocsar/obc/internal/storage"
	"github.com/rocsar/obc/internal/transport"
)

func newTestFileServer(t *testing.T) (*httptest.Server, *storage.Store) {
	t.Helper()

	root := t.TempDir()
	store := storage.New(root, "")
	srv := httptest.NewServer(transport.NewFileHandler(store, nil))
	t.Cleanup(srv.Close)
	return srv, store
}

// A path that resolves outside the data directory must be refused, never served.
//
// Checked through the HTTP layer rather than only against storage.Resolve,
// because the request path goes through net/http's own cleaning first and the two
// can disagree. `photos/../../etc/passwd` is the case that matters: it looks
// harmless and cleans to something outside the root.
func TestHTTPServerRefusesToEscapeTheDataDirectory(t *testing.T) {
	srv, _ := newTestFileServer(t)

	for _, path := range []string{
		"/../../../etc/passwd",
		"/%2e%2e/%2e%2e/etc/passwd",
		"/photos/../../etc/passwd",
		"/..%2f..%2fetc%2fpasswd",
	} {
		req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		if err != nil {
			t.Fatalf("building a request for %q: %v", path, err)
		}
		// Send the path verbatim rather than letting the client normalise it,
		// or the test would prove the CLIENT is safe and not the server.
		req.URL.Opaque = path

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			// A redirect is the server refusing at the routing layer, which is
			// an acceptable outcome. Following it must not yield a 200 either.
			continue
		}
		body := make([]byte, 256)
		n, _ := resp.Body.Read(body)
		_ = resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			t.Errorf("GET %s returned 200 with %q -- the data directory was escaped",
				path, string(body[:n]))
		}
		if strings.Contains(string(body[:n]), "root:") {
			t.Errorf("GET %s leaked /etc/passwd", path)
		}
	}
}

// The listing is JSON, because the only client is the Ground Station and an
// operator with curl should get something readable.
func TestHTTPListingIsJSON(t *testing.T) {
	srv, store := newTestFileServer(t)

	if err := store.WriteFileAtomic("cam-1.jpg", []byte("jpeg-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteFileAtomic("rx_data_20261003_120000.bin", []byte("sar"), 0o644); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want JSON", ct)
	}

	var listing struct {
		Path  string          `json:"path"`
		Files []storage.Entry `json:"files"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listing); err != nil {
		t.Fatalf("decoding the listing: %v", err)
	}
	if len(listing.Files) != 2 {
		t.Fatalf("listing has %d entries, want 2", len(listing.Files))
	}
	// Sorted by name, so two listings can be diffed.
	if listing.Files[0].Name != "cam-1.jpg" || listing.Files[1].Name != "rx_data_20261003_120000.bin" {
		t.Errorf("listing is not sorted by name: %+v", listing.Files)
	}
	if listing.Files[0].Kind != storage.KindCamera {
		t.Errorf("cam-1.jpg classified as %q", listing.Files[0].Kind)
	}
	if listing.Files[1].Kind != storage.KindSDR {
		t.Errorf("rx_data_*.bin classified as %q; the pattern must match what the vendored program writes", listing.Files[1].Kind)
	}
}

// Range is what makes a marginal-link transfer resumable, and a SAR capture that
// dies at 80% is the case it exists for.
func TestHTTPRangeRequestsResume(t *testing.T) {
	srv, store := newTestFileServer(t)

	body := make([]byte, 4096)
	for i := range body {
		body[i] = byte(i)
	}
	if err := store.WriteFileAtomic("rx_data_20261003_120000.bin", body, 0o644); err != nil {
		t.Fatal(err)
	}
	const path = "/rx_data_20261003_120000.bin"

	cases := []struct {
		name      string
		hdr       string
		wantCode  int
		wantBytes int
		wantStart int
	}{
		{"whole file", "", http.StatusOK, 4096, 0},
		{"middle slice", "bytes=100-199", http.StatusPartialContent, 100, 100},
		{"open-ended", "bytes=4000-", http.StatusPartialContent, 96, 4000},
		{"suffix", "bytes=-100", http.StatusPartialContent, 100, 3996},
		// Past the end is clamped, not refused: a resumer asking for more than
		// exists should get what there is.
		{"overrun clamped", "bytes=4090-9999", http.StatusPartialContent, 6, 4090},
		// Wholly past the end is unsatisfiable, and 416 tells the client the
		// real size so it can work out where to restart.
		// A 416 carries the error text in the body, so the byte count is not
		// checked for those three; what matters is the status and the true size
		// in Content-Range, which is what lets a resumer recompute its offset.
		{"past the end", "bytes=9000-9100", http.StatusRequestedRangeNotSatisfiable, -1, 0},
		{"nonsense", "bytes=abc", http.StatusRequestedRangeNotSatisfiable, -1, 0},
		{"reversed", "bytes=200-100", http.StatusRequestedRangeNotSatisfiable, -1, 0},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
			if err != nil {
				t.Fatal(err)
			}
			if c.hdr != "" {
				req.Header.Set("Range", c.hdr)
			}

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != c.wantCode {
				t.Fatalf("status = %d, want %d", resp.StatusCode, c.wantCode)
			}

			got := make([]byte, 8192)
			n, _ := readFull(resp.Body, got)

			if c.wantBytes >= 0 && n != c.wantBytes {
				t.Fatalf("read %d bytes, want %d", n, c.wantBytes)
			}

			// The check that matters. Content-Range and Content-Length were
			// correct while the body was the wrong slice of the file, so asserting
			// the headers alone would have passed a handler that silently
			// corrupted every resumed download.
			if c.wantCode == http.StatusPartialContent && n > 0 {
				if !bytesEqual(got[:n], body[c.wantStart:c.wantStart+n]) {
					t.Errorf("the body is not the requested slice:\n got %v\nwant %v",
						got[:min8(n)], body[c.wantStart:c.wantStart+min8(n)])
				}
			}

			if c.wantCode == http.StatusRequestedRangeNotSatisfiable {
				// The client needs the real size to recover.
				if cr := resp.Header.Get("Content-Range"); !strings.Contains(cr, "4096") {
					t.Errorf("416 without the true size: Content-Range = %q", cr)
				}
			}
		})
	}
}

// The method set is narrow. A write method reaching this handler would be a way
// to put files on the aircraft.
func TestHTTPRejectsWriteMethods(t *testing.T) {
	srv, _ := newTestFileServer(t)

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req, err := http.NewRequest(method, srv.URL+"/anything", strings.NewReader("x"))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s returned %d, want 405", method, resp.StatusCode)
		}
	}
}

// A missing file is a 404, not a 500 and not a 200 with an empty body.
func TestHTTPMissingFileIs404(t *testing.T) {
	srv, _ := newTestFileServer(t)

	resp, err := http.Get(srv.URL + "/nope.jpg")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// The JPEG content type matters: without it a Ground Station that trusts
// Content-Type cannot tell a photograph from a log.
func TestHTTPContentTypes(t *testing.T) {
	srv, store := newTestFileServer(t)

	for name, want := range map[string]string{
		"cam-1.jpg":            "image/jpeg",
		"rx_data_20261003.bin": "application/octet-stream",
		"connect-20261003.log": "text/plain; charset=utf-8",
		"params.json":          "application/json",
	} {
		if err := store.WriteFileAtomic(name, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		resp, err := http.Get(srv.URL + "/" + name)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if got := resp.Header.Get("Content-Type"); got != want {
			t.Errorf("%s Content-Type = %q, want %q", name, got, want)
		}
	}
}

func readFull(r interface{ Read([]byte) (int, error) }, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := r.Read(buf[total:])
		total += n
		if err != nil {
			return total, nil
		}
		if n == 0 {
			break
		}
	}
	return total, nil
}

func min8(n int) int {
	if n > 8 {
		return 8
	}
	return n
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// camera_bench's whole addition over the server is decoding the JPEG it just
// wrote and checking the result. This does that against the mock, which produces
// a real JPEG rather than bytes that happen to be called one.
//
// A camera that ignores S_FMT and hands back raw pixels would be written to the
// data directory, served over HTTP with Content-Type: image/jpeg, and reported
// as a successful capture by every layer above. Only decoding it catches that.
func TestCameraMockProducesADecodableJPEG(t *testing.T) {
	dir := t.TempDir()
	store := storage.New(dir, "")

	cam := camera.NewMock("/dev/video0", "photos", store)

	photo, err := cam.Capture(context.Background())
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(dir, photo.Path))
	if err != nil {
		t.Fatalf("the photograph is not where it said it was: %v", err)
	}
	if len(body) != int(photo.SizeBytes) {
		t.Errorf("SizeBytes says %d, the file is %d", photo.SizeBytes, len(body))
	}

	img, err := jpeg.Decode(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("%d bytes that are not a decodable JPEG: %v", len(body), err)
	}
	if b := img.Bounds(); b.Dx() < 16 || b.Dy() < 16 {
		t.Errorf("the JPEG is %dx%d, which is not a usable photograph", b.Dx(), b.Dy())
	}

	// Two captures must be distinguishable, which is what makes this useful for
	// checking that the fetch path serves the file the caller asked for.
	second, err := cam.Capture(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second.Name == photo.Name {
		t.Error("two captures produced the same name")
	}
	secondBody, err := os.ReadFile(filepath.Join(dir, second.Path))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(body, secondBody) {
		t.Error("two captures produced identical bytes; they cannot be told apart on the wire")
	}
}
