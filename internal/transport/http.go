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
// resumed: a SAR capture that dies at 80% over a marginal link is resumable over
// HTTP with Range and is not resumable at all over a datagram socket.
type HTTP struct {
	log   *slog.Logger
	store *storage.Store
	srv   *http.Server
	addr  string
}

// NewHTTP returns a file server rooted at the given data directory.
func NewHTTP(addr string, store *storage.Store, log *slog.Logger) *HTTP {
	if log == nil {
		log = slog.Default()
	}
	return &HTTP{log: log, store: store, addr: addr}
}

// Start binds the address and serves in the background.
func (h *HTTP) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", h.handle)

	h.srv = &http.Server{
		Addr:    h.addr,
		Handler: mux,
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
		"note", "the port here is what the tc flower filter classifies on; it must equal config.BulkPort")
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

func (h *HTTP) handle(w http.ResponseWriter, r *http.Request) {
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
	// the second line of defence -- the first is that Resolve is the only way
	// to get a path out of the store.
	abs, err := h.store.Resolve(rel)
	if err != nil {
		h.log.Warn("refused a path outside the data directory",
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
		h.serveList(w, r, abs)
		return
	}
	h.serveFile(w, r, abs, info)
}

// serveList returns a directory as JSON.
//
// JSON rather than HTML: the only client is the Ground Station, and an operator
// who wants to look with curl gets something readable without a browser.
func (h *HTTP) serveList(w http.ResponseWriter, r *http.Request, abs string) {
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name == "" {
		name = "."
	}
	// List takes a path relative to the root, so strip the root prefix that
	// Resolve already validated.
	entries, err := h.store.List(relativeToRoot(h.store.Root(), abs))
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
		h.log.Debug("encoding a listing failed", "err", err)
	}
}

// serveFile returns bytes, honouring Range.
func (h *HTTP) serveFile(w http.ResponseWriter, r *http.Request, abs string, info os.FileInfo) {
	f, err := os.Open(abs)
	if err != nil {
		http.Error(w, "cannot open", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", contentType(abs))
	// The name is derived from a sanitised path inside the root, so it cannot
	// contain a quote or a newline that would split the header.
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("inline; filename=%q", info.Name()))
	w.Header().Set("Accept-Ranges", "bytes")

	rangeHdr := r.Header.Get("Range")
	if rangeHdr == "" {
		w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
		if r.Method == http.MethodHead {
			return
		}
		if _, err := io.Copy(w, f); err != nil {
			// The client went away mid-transfer, which on a marginal link is
			// routine and not an error worth a stack trace.
			h.log.Debug("transfer interrupted", "file", info.Name(), "err", err)
		}
		return
	}

	start, end, ok := parseRange(rangeHdr, info.Size())
	if !ok {
		// An unsatisfiable range gets a 416 with the real size, which is what
		// lets a resuming client work out where to restart.
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
	if _, err := io.CopyN(w, f, end-start+1); err != nil {
		h.log.Debug("range transfer interrupted", "file", info.Name(), "err", err)
	}
}

// parseRange handles a single byte range, which is all a resuming client sends.
func parseRange(header string, size int64) (start, end int64, ok bool) {
	const prefix = "bytes="
	if !strings.HasPrefix(header, prefix) {
		return 0, 0, false
	}
	spec := strings.TrimPrefix(header, prefix)
	// Multiple ranges are not supported: they are a client being clever, and
	// answering with the whole file is simpler and still correct for a resumer.
	if strings.Contains(spec, ",") {
		return 0, 0, false
	}

	dash := strings.Index(spec, "-")
	if dash < 0 {
		return 0, 0, false
	}
	from, to := spec[:dash], spec[dash+1:]

	if from == "" {
		// Suffix range: the last N bytes.
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
	case strings.HasSuffix(name, ".bin"), strings.HasSuffix(name, ".dat"):
		return "application/octet-stream"
	default:
		return "application/octet-stream"
	}
}

func relativeToRoot(root, abs string) string {
	if abs == root {
		return "."
	}
	rel := strings.TrimPrefix(abs, root)
	return strings.TrimPrefix(rel, string(os.PathSeparator))
}
