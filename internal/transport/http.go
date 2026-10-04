package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/rocsar/obc/internal/storage"
)

// HTTP serves the data directory on the bulk port.
//
// Artefact bytes go over HTTP, never over ZeroMQ. Two reasons. The air link is
// 115 kbit/s, and a multi-megabyte SAR capture pushed through the PUB socket
// starves the telemetry and command traffic sharing it. And the transfer can be
// resumed: a capture that dies at 80% over a marginal link is resumable over
// HTTP with Range, and is not resumable at all over a datagram socket.
type HTTP struct {
	log   *slog.Logger
	store *storage.Store
	srv   *http.Server
	addr  string

	// limiter bounds artefact downloads. Built once here rather than per request
	// so that two concurrent downloads share one rate: the constraint is the link,
	// not the request.
	limiter *bulkLimiter
	// bulkRate is kept so the handler can be rebuilt (tests, restarts) from the
	// same configuration rather than from a rate already rounded through a bucket.
	bulkRate int
}

// NewHTTP returns a file server rooted at the given data directory.
//
// bytesPerSec bounds artefact downloads; zero or negative means unbounded.
func NewHTTP(addr string, store *storage.Store, log *slog.Logger, bytesPerSec int) *HTTP {
	if log == nil {
		log = slog.Default()
	}
	h := &HTTP{log: log, store: store, addr: addr, bulkRate: bytesPerSec}
	if lim := newBulkLimiter(bytesPerSec); lim != nil {
		h.limiter = lim
		h.log.Info("artefact downloads bounded", "rate", lim.String())
	}
	return h
}

// Start binds the address and serves in the background.
func (h *HTTP) Start(ctx context.Context) error {
	h.srv = &http.Server{
		Addr:    h.addr,
		Handler: NewFileHandler(h.store, h.log, h.bulkRate),
		// A slow operator on a marginal link must not tie up a goroutine
		// indefinitely, and an unbounded request body is an invitation.
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       60 * time.Second,
	}

	ln, err := net.Listen("tcp", h.addr)
	if err != nil {
		return fmt.Errorf("transport: listen on %s: %w", h.addr, err)
	}

	go func() {
		if err := h.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			h.log.Error("HTTP server stopped", "err", err)
		}
	}()

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = h.srv.Shutdown(shutdownCtx)
	}()

	h.log.Info("HTTP artefact server bound", "addr", h.addr, "root", h.store.Root(),
		"note", "artefact bytes are served here, and this is the bulk path the in-process limiter bounds")
	return nil
}

// Stop shuts the server down.
func (h *HTTP) Stop() {
	if h.srv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = h.srv.Shutdown(ctx)
}

// NewFileHandler returns the artefact handler for a data directory.
//
// Exported because mounting it on your own ServeMux is a legitimate thing to
// want, and because the traversal, Range and content-type behaviour is worth
// testing directly. It is not exported *for* the tests -- the tests use it
// because it is the same function the server runs.
// bytesPerSec bounds artefact downloads. Zero or negative means unbounded, which
// is what a caller that has not been told a rate gets -- a laptop on wifi, a test,
// a developer who wants to see what the link really does.
func NewFileHandler(store *storage.Store, log *slog.Logger, bytesPerSec int) http.HandlerFunc {
	if log == nil {
		log = slog.Default()
	}
	lim := newBulkLimiter(bytesPerSec)
	return func(w http.ResponseWriter, r *http.Request) { serve(w, r, store, log, lim) }
}

func serve(w http.ResponseWriter, r *http.Request, store *storage.Store, log *slog.Logger, lim *bulkLimiter) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "only GET and HEAD", http.StatusMethodNotAllowed)
		return
	}

	rel := strings.TrimPrefix(r.URL.Path, "/")
	if rel == "" {
		rel = "."
	}

	// Resolve refuses anything that escapes the root, after cleaning. This is
	// the second line of defence; the first is that Resolve is the only way to
	// get a path out of the store.
	abs, err := store.Resolve(rel)
	if err != nil {
		log.Warn("refused a path outside the data directory",
			"path", r.URL.Path, "remote", r.RemoteAddr)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	info, err := os.Stat(abs)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	if info.IsDir() {
		serveList(w, r, store, abs)
		return
	}
	serveFile(w, r, store, log, abs, info, lim)
}

// serveList returns a directory as JSON.
//
// JSON rather than HTML: the only client is the Ground Station, and an operator
// who wants to look with curl gets something readable without a browser.
func serveList(w http.ResponseWriter, r *http.Request, store *storage.Store, abs string) {
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name == "" {
		name = "."
	}
	entries, err := store.List(relativeToRoot(store.Root(), abs))
	if err != nil {
		http.Error(w, "cannot list", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(map[string]any{
		"path":  path.Clean(name),
		"files": entries,
	}); err != nil {
		// The header is already sent, so there is nothing to report to the
		// client. The connection simply ends short, which a JSON consumer
		// notices as a parse error rather than as a valid empty listing.
		return
	}
}

// serveFile returns bytes, honouring Range.
func serveFile(w http.ResponseWriter, r *http.Request, store *storage.Store, log *slog.Logger,
	abs string, info os.FileInfo, lim *bulkLimiter) {

	f, err := os.Open(abs)
	if err != nil {
		http.Error(w, "cannot open", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", contentType(info.Name()))
	// The name comes from a path already validated as inside the root, so it
	// cannot contain a quote or a newline that would split the header.
	w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", info.Name()))
	w.Header().Set("Accept-Ranges", "bytes")

	rangeHdr := r.Header.Get("Range")
	if rangeHdr == "" {
		w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
		if r.Method == http.MethodHead {
			return
		}
		if _, err := copyStream(w, lim.reader(f)); err != nil {
			// The client went away mid-transfer, which on a marginal link is
			// routine and not an error worth a stack trace.
			log.Debug("transfer interrupted", "file", info.Name(), "err", err)
		}
		return
	}

	start, end, ok := parseRange(rangeHdr, info.Size())
	if !ok {
		// An unsatisfiable range gets a 416 carrying the real size, which is
		// what lets a resuming client work out where to restart.
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", info.Size()))
		http.Error(w, "range not satisfiable", http.StatusRequestedRangeNotSatisfiable)
		return
	}

	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, info.Size()))
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	w.WriteHeader(http.StatusPartialContent)

	if r.Method == http.MethodHead {
		return
	}

	// SEEK to the start of the range before reading.
	//
	// This was missing, and the failure mode is the worst kind: the response had
	// a correct Content-Range and Content-Length and the wrong bytes. A client
	// resuming a 3 MB SAR capture at 80% would have been handed the file's
	// first 20% and appended it to what it already had, producing a file of the
	// right size containing nothing but zeroes from 80% onwards. It looks like
	// a flaky transfer, not a bug.
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		log.Warn("could not seek to the start of a range", "start", start, "err", err)
		return
	}

	if _, err := copyN(w, lim.reader(f), end-start+1); err != nil {
		log.Debug("range transfer interrupted", "file", info.Name(), "err", err)
	}
}

// parseRange handles a single byte range, which is all a resuming client sends.
//
// The cases that are easy to get wrong and are each pinned by a test:
//
//   - Past the end of the file is CLAMPED, not refused. A resumer asking for more
//     than exists should get what there is, not a 416 and a retry loop.
//   - Wholly past the end is unsatisfiable, and 416 with the true size lets the
//     client recompute where to restart.
//   - Multiple ranges are not supported: a client being clever gets the whole
//     file, which is simpler and still correct for a resumer.
func parseRange(header string, size int64) (start, end int64, ok bool) {
	const prefix = "bytes="
	if !strings.HasPrefix(header, prefix) {
		return 0, 0, false
	}
	spec := strings.TrimPrefix(header, prefix)
	if strings.Contains(spec, ",") {
		return 0, 0, false
	}

	dash := strings.Index(spec, "-")
	if dash < 0 {
		return 0, 0, false
	}
	from, to := spec[:dash], spec[dash+1:]

	if from == "" {
		n, err := strconv.ParseInt(to, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		if n > size {
			n = size
		}
		return size - n, size - 1, true
	}

	s, err := strconv.ParseInt(from, 10, 64)
	if err != nil || s < 0 || s >= size {
		return 0, 0, false
	}
	if to == "" {
		return s, size - 1, true
	}
	e, err := strconv.ParseInt(to, 10, 64)
	if err != nil || e < s {
		return 0, 0, false
	}
	if e >= size {
		e = size - 1
	}
	return s, e, true
}

func contentType(name string) string {
	switch {
	case strings.HasSuffix(name, ".jpg"), strings.HasSuffix(name, ".jpeg"):
		return "image/jpeg"
	case strings.HasSuffix(name, ".json"), strings.HasSuffix(name, ".toml"):
		return "application/json"
	case strings.HasSuffix(name, ".log"), strings.HasSuffix(name, ".txt"):
		return "text/plain; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}

func relativeToRoot(root, abs string) string {
	if abs == root {
		return "."
	}
	rel := strings.TrimPrefix(abs, root)
	return strings.TrimPrefix(rel, string(filepath.Separator))
}

// copyStream and copyN exist so the handler does not import io directly at three
// call sites, and so the two paths -- whole file and range -- can be swapped for
// an instrumented reader in a test without changing the handler.
func copyStream(dst io.Writer, src io.Reader) (int64, error) { return io.Copy(dst, src) }

func copyN(dst io.Writer, src io.Reader, n int64) (int64, error) { return io.CopyN(dst, src, n) }
