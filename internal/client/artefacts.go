package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Entry is one item in a directory listing.
//
// Mirrors internal/storage.Entry, which is what produces it, and is redeclared
// rather than imported: internal/storage is the server's package and it does
// filesystem work this side has no business doing. The JSON tags are the wire
// contract, so they are spelled out here and can be checked against the server.
//
// `size_bytes` is int64 because that is what the server emits. A negative size is
// not a thing a directory contains, and the difference is not worth an
// unsigned type that would have to be checked on every use.
type Entry struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
	Modified  int64  `json:"modified_unix"`
	Kind      string `json:"kind"`
	Directory bool   `json:"directory"`
}

// Listing is a decoded `GET /` response.
//
// `path` is the cleaned request path, echoed by internal/transport/http.go.
type Listing struct {
	Path  string  `json:"path"`
	Files []Entry `json:"files"`
}

// List returns one directory from the OBC's artefact server.
//
// path is relative to the data root; "" or "." is the root itself. The server
// confines every path to that root and answers 403 for anything that escapes it
// after cleaning, so this does not re-implement the check -- see SafeName for the
// one place a name becomes part of a local path before it reaches the wire.
//
// The listing is `{"files": [...]}` with directories included, not a bare array.
// Directories are shown and marked, because an operator looking for where the SDR
// put its output needs to see that `raw/` exists at all.
func (c *Client) List(ctx context.Context, path string) (Listing, error) {
	// The root is "/" and not "/.". "." is the right name for it in a config file
	// and the wrong one in a URL, and asking for "/." gets a 404 from the server --
	// which `gs_cli ls` did on every invocation until a test asked what path it
	// actually sent.
	req := "/" + strings.Trim(path, "/")
	if req == "/." {
		req = "/"
	}

	resp, err := c.httpGet(ctx, req)
	if err != nil {
		return Listing{}, fmt.Errorf("client: listing %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Listing{}, fmt.Errorf("client: listing %s: HTTP %d", path, resp.StatusCode)
	}
	var out Listing
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Listing{}, fmt.Errorf("client: listing %s is not the JSON this client expects: %w", path, err)
	}
	return out, nil
}

// ProgressFunc is called as bytes arrive. total is -1 when the server did not
// say, which is legal: a chunked response carries no Content-Length.
//
// Called on the goroutine doing the copy, so it must not block.
type ProgressFunc func(name string, have, total int64)

// FetchHooks are the optional callbacks a transfer reports through.
//
// Fetch used to print its own progress. It does not any more: a library that
// writes to stdout cannot be used from a window, and "where does this output go"
// is not a question a package should answer on the caller's behalf.
type FetchHooks struct {
	// Progress is called as bytes land. Optional.
	Progress ProgressFunc
	// Note is called for one-off human-readable lines -- resuming, or the server
	// having ignored a Range request. Optional.
	//
	// It exists rather than folding those into Progress because they are events,
	// not measurements, and a progress bar that occasionally prints a sentence
	// reads as a bug in the bar.
	Note func(string)
}

func (h FetchHooks) note(s string) {
	if h.Note != nil {
		h.Note(s)
	}
}

// Transfer describes one completed artefact transfer.
//
// Returned rather than reported through a hook, because it happens once and
// because of where it has to go. `gs_cli fetch x.jpg > out.jpg` pipes the bytes
// to stdout, so the one-line summary belongs on stderr -- and a package that
// decides that for its caller is a package that will eventually decide it wrong.
type Transfer struct {
	// Name is the artefact as named on the OBC.
	Name string
	// Bytes is the whole file, including anything resumed from a previous attempt.
	Bytes int64
	// Elapsed is this transfer's wall time, not the file's.
	Elapsed time.Duration
}

// Rate is bytes per second over Elapsed.
func (t Transfer) Rate() float64 {
	return float64(t.Bytes) / 1024 / maxf(t.Elapsed.Seconds(), 0.001)
}

// Fetch downloads an artefact.
//
// Two things it does that a bare GET does not:
//
//   - It resumes. The link is 115 kbit/s and the files are tens of megabytes, so
//     an interrupted download is the normal case rather than the exceptional one.
//     The server answers a Range request with 206, and this asks for the remainder.
//   - It writes to a temporary file and renames, so an interrupted transfer never
//     leaves a truncated file that looks complete. A partial photograph that looks
//     whole is worse than no photograph.
//
// out is the final path; the transfer goes to out + ".part" and is renamed on
// success. An empty out writes to stdout, which is what `gs_cli fetch x.jpg`
// does with a pipe.
func (c *Client) Fetch(ctx context.Context, name, out string, resume bool, hooks FetchHooks) (Transfer, error) {
	if err := SafeName(name); err != nil {
		return Transfer{}, err
	}

	url := c.cfg.HTTP + "/" + name

	// Resume: find out how much of the partial file is already there.
	var have int64
	partial := out + ".part"
	if resume {
		if st, err := os.Stat(partial); err == nil {
			have = st.Size()
			hooks.note(fmt.Sprintf("resuming at %d bytes", have))
		}
	}
	if !resume {
		_ = os.Remove(partial)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Transfer{}, err
	}
	if have > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", have))
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Transfer{}, fmt.Errorf("client: fetch %s: %w", name, err)
	}
	defer resp.Body.Close()

	// A server that ignored the Range answers 200 with the whole body. Appending
	// that to the partial file would silently corrupt it.
	truncated := have > 0 && resp.StatusCode != http.StatusPartialContent
	if truncated {
		hooks.note(fmt.Sprintf("server ignored the Range (HTTP %d); starting over", resp.StatusCode))
		have = 0
	}

	switch resp.StatusCode {
	case http.StatusOK, http.StatusPartialContent:
	default:
		return Transfer{}, fmt.Errorf("client: fetch %s: HTTP %d", name, resp.StatusCode)
	}

	if resp.ContentLength > 0 && resp.StatusCode != http.StatusPartialContent {
		hooks.note(fmt.Sprintf("%s: %d bytes", name, resp.ContentLength))
	}

	dst := io.Writer(os.Stdout)
	var f *os.File
	if out != "" {
		flags := os.O_CREATE | os.O_WRONLY
		if have > 0 {
			flags |= os.O_APPEND
		} else {
			flags |= os.O_TRUNC
		}
		f, err = os.OpenFile(partial, flags, 0o644)
		if err != nil {
			return Transfer{}, err
		}
		defer f.Close()
		dst = f
	}

	var body io.Reader = resp.Body
	if hooks.Progress != nil {
		body = &progressReader{
			src:   resp.Body,
			hook:  hooks.Progress,
			name:  name,
			done:  have,
			last:  time.Now(),
			every: progressInterval,
		}
	}

	start := time.Now()
	written, err := io.Copy(dst, body)
	if err != nil {
		return Transfer{}, fmt.Errorf("client: %s: interrupted after %d bytes: %w", name, written, err)
	}
	if f != nil {
		if err := f.Close(); err != nil {
			return Transfer{}, err
		}
		if err := os.Rename(partial, out); err != nil {
			return Transfer{}, err
		}
	}

	return Transfer{Name: name, Bytes: have + written, Elapsed: time.Since(start)}, nil
}

// SafeName reports whether name is an artefact name inside the data root.
//
// Subdirectories are ALLOWED, and they have to be: the listing reports names like
// `photos/camera-20261004-223347.jpg`, so a stricter rule makes `gs_cli ls` output
// impossible to paste straight into a fetch. The first version of this rejected
// every "/" and so refused the exact names the tool had printed one line earlier.
//
// Refused is anything that could leave the artefact root: a parent traversal, a
// leading slash, a backslash, or an empty component. The server validates this
// properly too; refusing here means the client says so itself rather than relying
// on the far side.
//
// It matters more on the client than it looks. A name from a listing becomes part
// of a local filesystem path here, and a window is a more likely source of a
// pasted path than a shell is.
func SafeName(name string) error {
	if name == "" {
		return errors.New("client: no artefact name given")
	}
	if strings.Contains(name, "..") || strings.HasPrefix(name, "/") ||
		strings.Contains(name, "\\") || strings.Contains(name, "//") {
		return fmt.Errorf("client: %q is not an artefact name inside the data root", name)
	}
	return nil
}

// progressReader reports as bytes pass through it.
//
// Rate-limited to 10 Hz. The link is 8 KiB/s by default (qos.bulk_rate_bps), so a
// console cannot usefully display more, and an event per Read is one more thing
// competing with telemetry for a 115 kbit/s pipe.
type progressReader struct {
	src  io.Reader
	hook ProgressFunc
	name string

	done int64 // already on disk before this transfer started
	last time.Time
	// every is the floor between callbacks. A field rather than a constant so a
	// test can drive it to zero instead of sleeping 100ms to observe a call --
	// a timing assertion in a unit test is a flake waiting for a busy machine.
	every time.Duration
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.src.Read(b)
	p.done += int64(n)
	if n > 0 && (p.every <= 0 || time.Since(p.last) >= p.every) {
		p.last = time.Now()
		p.hook(p.name, p.done, -1)
	}
	return n, err
}

// progressInterval is the floor between ProgressFunc calls.
const progressInterval = 100 * time.Millisecond

func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
