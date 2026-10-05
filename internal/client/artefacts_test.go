package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The artefact tests exist because this is the one path where a mistake is
// invisible. A photograph that is 90% there and looks whole decodes as a
// truncated JPEG in some viewers and as a valid image in others; a file with a
// resume appended to it that should not have been decodes as nothing at all. And
// a one-line progress message landing in the middle of piped bytes produces a
// file that is not a JPEG, with no error anywhere to explain it.

func testClient(t *testing.T, h http.HandlerFunc) (*Client, func()) {
	t.Helper()
	srv := httptest.NewServer(h)
	return New(Config{HTTP: srv.URL}), srv.Close
}

// ---------------------------------------------------------------------------
// SafeName
// ---------------------------------------------------------------------------

// A listing name becomes a local filesystem path here, and a window is a more
// likely source of a pasted path than a shell is. The server refuses these too;
// refusing here means the client says so itself.
func TestSafeNameRefusesEscapingTheRoot(t *testing.T) {
	for _, tc := range []struct{ name, why string }{
		{"../etc/passwd", "parent traversal"},
		{"photos/../../etc/passwd", "traversal after a valid prefix"},
		{"/etc/passwd", "absolute"},
		{`photos\x.jpg`, "backslash"},
		{"photos//x.jpg", "empty component"},
		{"", "empty"},
	} {
		if err := SafeName(tc.name); err == nil {
			t.Errorf("%q was accepted (%s)", tc.name, tc.why)
		}
	}
}

// The subdirectory case is the one a stricter check gets wrong. The listing
// reports `photos/camera-20261004-223347.jpg`, so refusing every "/" makes
// `gs_cli ls` output impossible to paste into a fetch.
func TestSafeNameAcceptsSubdirectories(t *testing.T) {
	for _, name := range []string{
		"photo.jpg",
		"photos/camera-20261004-223347.jpg",
		"raw/rx_data_001.bin",
	} {
		if err := SafeName(name); err != nil {
			t.Errorf("%q was refused: %v", name, err)
		}
	}
}

// ---------------------------------------------------------------------------
// List
// ---------------------------------------------------------------------------

// The wire shape is {"path","files"} with directories included -- not a bare
// array. internal/transport/http.go encodes a map with both keys, and a client
// that expects an array gets an empty listing and reports "no artefacts" on a
// data directory full of them.
func TestListDecodesTheServersShape(t *testing.T) {
	c, done := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			t.Errorf("asked for %q, want /", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
		  "path": ".",
		  "files": [
		    {"name":"photos","size_bytes":0,"modified_unix":1,"kind":"unknown","directory":true},
		    {"name":"rx_data_001.bin","size_bytes":31457280,"modified_unix":2,"kind":"sdr","directory":false}
		  ]}`)
	})
	defer done()

	got, err := c.List(context.Background(), "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got.Path != "." {
		t.Errorf("path = %q, want .", got.Path)
	}
	if len(got.Files) != 2 {
		t.Fatalf("got %d entries, want 2", len(got.Files))
	}
	if !got.Files[0].Directory {
		t.Error("photos should be marked as a directory")
	}
	if got.Files[1].SizeBytes != 31457280 || got.Files[1].Kind != "sdr" {
		t.Errorf("rx_data_001.bin decoded as %+v", got.Files[1])
	}
}

// A subdirectory is the normal case for an operator looking for the SDR's output,
// and the leading slash has to be optional because a listing shows bare names.
func TestListAsksForASubdirectoryWithoutDoublingTheSlash(t *testing.T) {
	var asked string
	c, done := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Path
		fmt.Fprint(w, `{"path":"photos","files":[]}`)
	})
	defer done()

	for _, given := range []string{"photos", "/photos"} {
		if _, err := c.List(context.Background(), given); err != nil {
			t.Fatalf("List(%q): %v", given, err)
		}
		if asked != "/photos" {
			t.Errorf("List(%q) asked for %q, want /photos", given, asked)
		}
	}
}

// A 403 is the server refusing a path, and it must reach the operator as a 403
// rather than as an empty listing. "No artefacts" and "you may not look there"
// are different sentences and only one of them is true.
func TestListReportsAnHTTPError(t *testing.T) {
	c, done := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	})
	defer done()

	if _, err := c.List(context.Background(), "../etc"); err == nil {
		t.Fatal("a 403 was reported as success")
	} else if !strings.Contains(err.Error(), "403") {
		t.Errorf("error %q does not mention the status", err)
	}
}

// ---------------------------------------------------------------------------
// Fetch
// ---------------------------------------------------------------------------

func TestFetchWritesTheFileAndLeavesNoPartial(t *testing.T) {
	body := strings.Repeat("jpeg-bytes-", 100)
	c, done := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, body)
	})
	defer done()

	dir := t.TempDir()
	out := filepath.Join(dir, "photo.jpg")

	x, err := c.Fetch(context.Background(), "photo.jpg", out, true, FetchHooks{})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if x.Bytes != int64(len(body)) {
		t.Errorf("reported %d bytes, want %d", x.Bytes, len(body))
	}

	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading the result: %v", err)
	}
	if string(got) != body {
		t.Errorf("the file does not match what the server sent (%d vs %d bytes)", len(got), len(body))
	}
	// The rename is the point: a file is either absent or complete.
	if _, err := os.Stat(out + ".part"); !os.IsNotExist(err) {
		t.Error("a .part file was left behind")
	}
}

// Resuming is the normal case, not the exception: the link is 115 kbit/s and the
// files are tens of megabytes.
func TestFetchResumesFromAPartialFile(t *testing.T) {
	full := strings.Repeat("abcdefgh", 50)
	const have = 200 // 25 whole "abcdefgh" groups

	c, done := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		want := fmt.Sprintf("bytes=%d-", have)
		if got := r.Header.Get("Range"); got != want {
			t.Errorf("Range = %q, want %q", got, want)
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", have, len(full)-1, len(full)))
		w.WriteHeader(http.StatusPartialContent)
		fmt.Fprint(w, full[have:])
	})
	defer done()

	dir := t.TempDir()
	out := filepath.Join(dir, "rx_data_001.bin")
	if err := os.WriteFile(out+".part", []byte(full[:have]), 0o644); err != nil {
		t.Fatal(err)
	}

	var notes []string
	x, err := c.Fetch(context.Background(), "rx_data_001.bin", out, true, FetchHooks{
		Note: func(s string) { notes = append(notes, s) },
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if x.Bytes != int64(len(full)) {
		t.Errorf("reported %d bytes for the whole file, want %d", x.Bytes, len(full))
	}

	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != full {
		t.Errorf("resumed file is %d bytes and does not match the %d-byte original", len(got), len(full))
	}

	// The resume is announced. A transfer that silently continues where it left
	// off is indistinguishable, to the operator, from one that restarted.
	if len(notes) == 0 || !strings.Contains(notes[0], "resuming at 200") {
		t.Errorf("notes = %v, want a line saying it resumed at 200", notes)
	}
}

// A server that ignores the Range answers 200 with the whole body. Appending that
// to the partial file produces a file that is the right length, wrong
// throughout, and opens without complaint. This is the single most dangerous
// thing this function could do.
func TestFetchDoesNotCorruptWhenTheServerIgnoresRange(t *testing.T) {
	full := strings.Repeat("Z", 300)

	c, done := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") == "" {
			t.Error("no Range header was sent, so this test is not exercising the resume path")
		}
		w.WriteHeader(http.StatusOK) // the whole body, Range ignored
		fmt.Fprint(w, full)
	})
	defer done()

	dir := t.TempDir()
	out := filepath.Join(dir, "rx_data_001.bin")
	if err := os.WriteFile(out+".part", []byte(strings.Repeat("Y", 100)), 0o644); err != nil {
		t.Fatal(err)
	}

	var notes []string
	if _, err := c.Fetch(context.Background(), "rx_data_001.bin", out, true, FetchHooks{
		Note: func(s string) { notes = append(notes, s) },
	}); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != full {
		t.Errorf("the file is %d bytes; a Range-ignoring server must not have been appended to", len(got))
	}

	joined := strings.Join(notes, " | ")
	if !strings.Contains(joined, "ignored the Range") {
		t.Errorf("notes = %v, want a line saying the Range was ignored", notes)
	}
}

// resume=false must start over rather than continue, and must remove the stale
// partial first -- otherwise the second attempt appends to the first.
func TestFetchWithoutResumeStartsOver(t *testing.T) {
	full := strings.Repeat("Q", 120)
	c, done := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			t.Errorf("Range = %q, want none when resume is off", r.Header.Get("Range"))
		}
		fmt.Fprint(w, full)
	})
	defer done()

	dir := t.TempDir()
	out := filepath.Join(dir, "rx_data_001.bin")
	if err := os.WriteFile(out+".part", []byte(strings.Repeat("P", 300)), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := c.Fetch(context.Background(), "rx_data_001.bin", out, false, FetchHooks{}); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != full {
		t.Errorf("the file is %d bytes, want %d -- the stale partial was not discarded", len(got), len(full))
	}
}

// The transfer body and the progress narration must be separable, because
// `gs_cli fetch x.jpg > out.jpg` pipes one and prints the other. If a note went
// to stdout it would land inside the artefact.
func TestProgressIsCoalescedNotPerRead(t *testing.T) {
	c, done := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, strings.Repeat("x", 4096))
	})
	defer done()

	dir := t.TempDir()
	calls := 0
	if _, err := c.Fetch(context.Background(), "x.bin", filepath.Join(dir, "x.bin"), true, FetchHooks{
		Progress: func(string, int64, int64) { calls++ },
	}); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	// 4096 bytes at 8 KiB/s is well under the 100ms floor, so a per-Read
	// implementation would have called this several times.
	if calls > 1 {
		t.Errorf("progress fired %d times for a sub-interval transfer; it is meant to be rate-limited", calls)
	}
}

// When out is empty the artefact goes to stdout, so narration must be
// separable from it. This test is the reason Fetch returns a Transfer instead of
// printing a summary: `gs_cli fetch photo.jpg > out.jpg` is the obvious
// invocation, and a status line on stdout produces a file that is not a JPEG with
// no error anywhere. It was that bug -- the tool printed "<name>: <n> bytes" to
// stdout while fetching to stdout.
//
// What this can check is that the narration arrives only through the hooks, and
// that nothing is written to the process's own stdout by the library.
func TestNarrationIsDeliveredThroughHooksNotPrinted(t *testing.T) {
	c, done := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "0123456789")
	})
	defer done()

	dir := t.TempDir()
	var notes []string
	x, err := c.Fetch(context.Background(), "x.bin", filepath.Join(dir, "x.bin"), true, FetchHooks{
		Note: func(s string) { notes = append(notes, s) },
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if x.Bytes != 10 {
		t.Errorf("Bytes = %d, want 10", x.Bytes)
	}
	// The size line is a note, so it can be routed to stderr by the caller.
	if !containsAny(notes, "10 bytes") {
		t.Errorf("notes = %v, want the size announced so the caller can send it to stderr", notes)
	}
}

func containsAny(haystack []string, want string) bool {
	for _, s := range haystack {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}

// Config crosses the Wails bridge as JSON in both directions: the window
// sends endpoints to Connect, and Endpoints returns them for display. The tags
// are the contract -- without them the generator emits Go field names and the
// TypeScript side silently sends fields Go ignores, which reads as "the
// connect button does nothing".
func TestConfigJSONRoundTrip(t *testing.T) {
	in := Config{
		Control:   "tcp://192.168.1.50:5555",
		Telemetry: "tcp://192.168.1.50:5556",
		HTTP:      "http://192.168.1.50:5557",
		Topic:     "telemetry",
	}
	body, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Config
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Errorf("config round trip changed %+v to %+v", in, out)
	}
	for _, key := range []string{`"control_endpoint"`, `"telemetry_endpoint"`, `"http_endpoint"`, `"topic"`} {
		if !strings.Contains(string(body), key) {
			t.Errorf("config JSON lacks %s; the frontend sends these names", key)
		}
	}
}

// ---------------------------------------------------------------------------
// Names
// ---------------------------------------------------------------------------

// Names feeds gs_cli's `commands` listing and cmd/gs's command palette. If it
// drifts from BuildRequests, a console offers a command that does not exist --
// or, worse, one that exists and is not offered.
//
// What matters is not that every command builds without arguments; several
// legitimately require some. It is that every listed name is *recognised*, and
// that nothing is recognised without being listed.
func TestNamesAndBuildRequestsAgree(t *testing.T) {
	listed := map[string]bool{}
	for _, name := range Names() {
		listed[name] = true
	}

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"query", nil},
		{"photo", nil},
		{"gnss", []string{"1"}},
		{"gnss-rotate", nil},
		{"heading", []string{"275.5"}},
		{"jog", []string{"1", "3000"}},
		{"zero", nil},
		{"mount", []string{"1", "0.5"}},
		{"dir", []string{"1", "+1"}},
		{"heater", []string{"1", "on"}},
		{"stop", []string{"all"}},
		{"pico-status", nil},
		{"sdr-probe", nil},
		{"sdr-get-params", nil},
		{"sdr-connect", nil},
		{"sdr-reset-usb", nil},
		{"link", []string{"115"}},
		{"reboot", nil},
	} {
		if _, err := BuildRequests(tc.name, tc.args); err != nil {
			t.Errorf("%q is not usable: %v", tc.name, err)
		}
		if !listed[tc.name] {
			t.Errorf("%q builds but Names() does not list it, so no console offers it", tc.name)
		}
	}

	// And nothing is listed that does not build.
	for name := range listed {
		if _, err := BuildRequests(name, validArgsFor(name)); err != nil {
			t.Errorf("Names lists %q but it does not build: %v", name, err)
		}
	}
}

// validArgsFor supplies arguments for the commands that require them, so the
// drift check above is about recognition rather than about arity.
func validArgsFor(name string) []string {
	switch name {
	case "gnss", "heading", "stop", "link":
		return []string{"1"}
	case "jog":
		return []string{"1", "1"}
	case "mount":
		return []string{"1", "0"}
	case "dir":
		return []string{"1", "+1"}
	case "heater":
		return []string{"1", "on"}
	}
	return nil
}
