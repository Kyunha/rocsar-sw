// Package storage owns the data directory on the SSD.
//
// Deliberately minimal: a directory, atomic file placement, and a name that is
// unique and sortable. There is no artefact catalogue, no index.json, no
// checksums and no paging. ARCHITECTURE.md 6.7 records what that costs.
package storage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// Errors.
var (
	// ErrNoRoot is returned when the data directory does not exist.
	ErrNoRoot = errors.New("storage: data directory is not available")

	// ErrEscapesRoot is returned when a caller tries to reach outside the data
	// directory. It is a refusal, not a normalisation: a path that resolves
	// outside the root after cleaning is a bug or an attack, and quietly
	// clamping it to the root would turn either into a wrong-but-successful read.
	ErrEscapesRoot = errors.New("storage: path escapes the data directory")
)

// Store is the data directory.
type Store struct {
	root string
}

// New returns a store rooted at dir.
//
// The directory is NOT created and NOT checked here. Call Check, which is a
// separate step because an unwritable data directory is the one failure that is
// fatal at startup while every other missing device merely degrades -- see
// ARCHITECTURE.md 8.
func New(dir string) *Store { return &Store{root: dir} }

// Root returns the data directory.
func (s *Store) Root() string { return s.root }

// Check verifies the directory exists and is writable.
//
// Called once at startup and its failure is fatal. Everything written here is
// flight data: a SAR capture, a photograph, an SDR log. Losing it silently is
// worse than not starting, because the operator finds out at landing.
func (s *Store) Check() error {
	info, err := os.Stat(s.root)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrNoRoot, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: %s is not a directory", ErrNoRoot, s.root)
	}

	// Writability is probed rather than inferred from the mode bits, because the
	// interesting case is a filesystem that is mounted read-only or full, and
	// both report as a successful open for reading.
	probe, err := os.CreateTemp(s.root, ".rocsar-probe-*")
	if err != nil {
		return fmt.Errorf("%w: not writable: %s", ErrNoRoot, err)
	}
	name := probe.Name()
	_ = probe.Close()
	if err := os.Remove(name); err != nil {
		return fmt.Errorf("%w: could not clean up the probe file: %s", ErrNoRoot, err)
	}
	return nil
}

// Resolve turns a caller-supplied relative name into an absolute path inside the
// root, or refuses.
//
// The cleaning is done before the check, not after: `../../etc/passwd` and
// `photos/../../etc/passwd` both clean to a path outside the root, and checking
// the un-cleaned string would let the second through if the first were blocked.
func (s *Store) Resolve(name string) (string, error) {
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("%w: %s is absolute", ErrEscapesRoot, name)
	}
	clean := filepath.Clean(name)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %s", ErrEscapesRoot, name)
	}

	abs := filepath.Join(s.root, clean)

	// Belt and braces: confirm the joined path really is inside, in case Clean
	// and Join ever disagree about a separator.
	rootAbs, err := filepath.Abs(s.root)
	if err != nil {
		return "", err
	}
	absPath, err := filepath.Abs(abs)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(rootAbs, absPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %s", ErrEscapesRoot, name)
	}

	return absPath, nil
}

// Sub returns a store rooted at a subdirectory, creating it.
func (s *Store) Sub(dir string) (*Store, error) {
	abs, err := s.Resolve(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("storage: create %s: %w", abs, err)
	}
	return &Store{root: abs}, nil
}

// WriteFileAtomic writes data to name inside the root, atomically.
//
// Write to a temporary file in the SAME directory, fsync, then rename.
//
// The same-directory requirement is not pedantry: rename is only atomic within a
// filesystem, and a temp file in /tmp would make this a copy-then-rename whose
// reader can observe a half-written file. The fsync before the rename is what
// makes it durable -- without it a power loss can leave the rename recorded and
// the contents not, which on an aircraft is a real sequence of events.
func (s *Store) WriteFileAtomic(name string, data []byte, perm os.FileMode) error {
	abs, err := s.Resolve(name)
	if err != nil {
		return err
	}
	dir := filepath.Dir(abs)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("storage: create %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".partial-*")
	if err != nil {
		return fmt.Errorf("storage: temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()

	// Any failure past this point leaves a stray temp file unless it is removed.
	// Cleaned up on every path, including the error ones.
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()

	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("storage: write %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("storage: sync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("storage: close %s: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return fmt.Errorf("storage: chmod %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, abs); err != nil {
		return fmt.Errorf("storage: rename into %s: %w", abs, err)
	}
	return nil
}

// Entry is one item in a directory listing.
type Entry struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
	Modified  int64  `json:"modified_unix"`
	Kind      string `json:"kind"`
	Directory bool   `json:"directory"`
}

// Artefact kinds. Deliberately a closed set: the HTTP listing tells the Ground
// Station what a file is, and a free-text kind would put that decision in the
// hands of whatever wrote the file.
const (
	KindCamera  = "camera"
	KindSDR     = "sdr"
	KindLog     = "log"
	KindUnknown = "unknown"
)

// List returns the contents of a directory inside the root.
func (s *Store) List(name string) ([]Entry, error) {
	abs, err := s.Resolve(name)
	if err != nil {
		return nil, err
	}
	items, err := os.ReadDir(abs)
	if err != nil {
		return nil, fmt.Errorf("storage: list %s: %w", abs, err)
	}

	out := make([]Entry, 0, len(items))
	for _, it := range items {
		// Temporary files from an interrupted atomic write are not artefacts.
		// Listing them would show the operator half-written files that no
		// command can produce and that will never be complete.
		if strings.HasPrefix(it.Name(), ".partial-") || strings.HasPrefix(it.Name(), ".rocsar-probe-") {
			continue
		}

		e := Entry{Name: it.Name(), Kind: KindUnknown}
		if info, err := it.Info(); err == nil {
			e.SizeBytes = info.Size()
			e.Modified = info.ModTime().Unix()
			e.Directory = info.IsDir()
		}
		e.Kind = KindOf(e.Name, e.Directory)
		out = append(out, e)
	}

	// Sorted by name. os.ReadDir already sorts, but the listing is part of the
	// HTTP contract and a client that has to re-sort to diff two of them is a
	// client doing our job for us.
	sortEntries(out)
	return out, nil
}

func sortEntries(e []Entry) {
	for i := 1; i < len(e); i++ {
		for j := i; j > 0 && e[j].Name < e[j-1].Name; j-- {
			e[j], e[j-1] = e[j-1], e[j]
		}
	}
}

var (
	cameraName = regexp.MustCompile(`^cam-.*\.(jpg|jpeg)$`)
	// The vendored program writes `Data/rx_data_<localtime>.bin`, so the
	// timestamp is part of the stem and the pattern has to allow for it. An
	// earlier version anchored `rx_data` directly against the extension and
	// matched nothing the program actually produces.
	sdrName = regexp.MustCompile(`^(rx_data.*|.*sar.*)\.(bin|dat)$`)
	logName = regexp.MustCompile(`.*\.(log|txt)$`)
)

// KindOf classifies a filename by its convention.
//
// By convention rather than by a recorded field, because there is no catalogue.
// The naming is the contract: `cam-*.jpg` is a photograph, `rx_data_*.bin` is a
// SAR capture. That is a real reduction in rigour -- a file renamed by hand
// becomes unclassifiable -- and it is the price of not keeping an index.
func KindOf(name string, isDir bool) string {
	if isDir {
		return KindUnknown
	}
	lower := strings.ToLower(name)
	switch {
	case cameraName.MatchString(lower):
		return KindCamera
	case sdrName.MatchString(lower):
		return KindSDR
	case logName.MatchString(lower):
		return KindLog
	default:
		return KindUnknown
	}
}

// FreeSpace reports the filesystem's free bytes for the data directory.
//
// Bavail, not Bfree: Bfree includes blocks reserved for root, and on a mounted
// SSD the operator is not root. Reporting Bfree would overstate the space
// available to the OBC by whatever fraction the filesystem reserves, which is
// exactly the number that matters when a SAR capture is about to fill it.
func (s *Store) FreeSpace() (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(s.root, &st); err != nil {
		return 0, fmt.Errorf("storage: statfs %s: %w", s.root, err)
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}

// NameFor builds a sortable, unique artefact name.
//
// The timestamp is local and formatted for lexical ordering, which is why it is
// not RFC3339: `20261003-142530.jpg` sorts correctly as a string and
// `2026-10-03T14:25:30Z.jpg` does not compare usefully against anything.
func NameFor(kind, ext string, prefix string) string {
	ts := time.Now().Format("20060102-150405")
	if prefix == "" {
		return fmt.Sprintf("%s-%s.%s", kind, ts, ext)
	}
	return fmt.Sprintf("%s-%s-%s.%s", prefix, kind, ts, ext)
}
