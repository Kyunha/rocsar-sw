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
	"strconv"
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

	// ErrWrongDevice is returned when the data root is not on the filesystem the
	// configuration named. It is separate from ErrNoRoot because the failure
	// modes are opposite: ErrNoRoot says nothing is there, ErrWrongDevice says
	// something is there and it is the wrong thing. A caller that collapses them
	// into one message tells an operator with a missing mount to go looking for
	// a missing directory.
	ErrWrongDevice = errors.New("storage: data directory is on the wrong filesystem")
)

// Store is the data directory.
type Store struct {
	root string
	// device is the filesystem identity the root is expected to have, as a UUID
	// or a label. Empty asserts nothing. See Check.
	device string
}

// New returns a store rooted at dir.
//
// The directory is NOT created and NOT checked here. Call Check, which is a
// separate step because an unwritable data directory is the one failure that is
// fatal at startup while every other missing device merely degrades -- see
// ARCHITECTURE.md 8.
//
// device is the filesystem the root is expected to be, as a UUID or a label, and
// may be empty to assert nothing. See Check for why that distinction is the
// whole point.
func New(dir, device string) *Store { return &Store{root: dir, device: device} }

// Root returns the data directory.
func (s *Store) Root() string { return s.root }

// Check verifies the directory exists, is writable, and is on the filesystem the
// operator named.
//
// Called once at startup and its failure is fatal. Everything written here is
// flight data: a SAR capture, a photograph, an SDR log. Losing it silently is
// worse than not starting, because the operator finds out at landing.
//
// # Why the device check exists
//
// Writability is not identity. When the SSD drops off the USB bus -- and it did,
// twice, both times ending in "Synchronize Cache failed: hostbyte=0x07", which
// is the drive vanishing mid-flush -- the mount point does not disappear. It
// becomes an ordinary directory on the root filesystem, still there, still
// writable, and every writability probe passes. So the OBC started, served
// telemetry, reported the artefact server healthy, and listed an empty
// directory while the acquisition program wrote 59 captures and 2.8 GB to the
// SD card next to it. The operator's only sign was that the Ground Station showed
// nothing, which reads as a bug in the console.
//
// That failure is now fatal here rather than silent, and it says which
// filesystem it found instead of the one that was wanted.
//
// An empty device asserts nothing. That is the laptop case: a development
// checkout points http.root at a directory under the working tree and there is no
// second filesystem to be on, so asking for one would fail every bench run. The
// aircraft names the device; the bench does not.
func (s *Store) Check() error {
	info, err := os.Stat(s.root)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrNoRoot, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: %s is not a directory", ErrNoRoot, s.root)
	}

	if err := s.checkDevice(); err != nil {
		return err
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

// checkDevice confirms that the filesystem under the root is the one the
// operator named.
//
// # Why not statfs
//
// The first version used statfs, and it could not work: Linux exposes only the
// first 8 bytes of an ext4 UUID through struct statfs (f_type is the superblock
// magic and f_fsid is the high and low halves of the UUID), so a 16-byte
// filesystem UUID is not recoverable from it on a 64-bit kernel. Half a UUID
// would have been a check that could not distinguish the aircraft's disk from
// another disk sharing a UUID prefix, which is precisely the confusion this
// exists to end.
//
// mountinfo is the interface that answers the question properly. It is a
// documented, stable kernel interface, readable without any capability, and it
// states what is mounted where in full -- including the device, which is then
// compared against the symlink the configured UUID resolves to.
//
// The device must be attached AND mounted. A drive that is absent is a different
// failure from one that is present and unmounted, and they are reported
// differently: the first means cable, power or a dead disk, the second means
// fstab did not do its job. Both refuse, because the operator cannot tell them
// apart from the symptom.
func (s *Store) checkDevice() error {
	if s.device == "" {
		return nil
	}

	want, err := filepath.EvalSymlinks(filepath.Join(diskByUUIDDir, s.device))
	if err != nil {
		return fmt.Errorf("%w: the disk %q is not attached to this system (%s is missing).\n"+
			"  The data root cannot be checked because there is nothing to check it against.\n"+
			"  Check the cable, the hub's power, and `lsblk` before anything else.",
			ErrWrongDevice, s.device, filepath.Join(diskByUUIDDir, s.device))
	}

	mounted, err := filesystemAt(s.root)
	if err != nil {
		return fmt.Errorf("%w: cannot read mountinfo: %s", ErrWrongDevice, err)
	}
	if mounted == nil {
		return fmt.Errorf("%w: no filesystem is mounted at %s.\n"+
			"  The directory exists, which is why every writability check passed, but\n"+
			"  it is an ordinary directory on the root filesystem. Anything written there\n"+
			"  would go to the wrong disk. Check `mount` and /etc/fstab.",
			ErrWrongDevice, s.root)
	}

	got, err := filepath.EvalSymlinks(mounted.source)
	if err != nil {
		// A source that is not a path we can resolve -- a network or overlay
		// mount -- is reported as itself rather than being treated as a match.
		got = mounted.source
	}

	if got != want {
		return fmt.Errorf("%w: %s is on %s (%s) but %q was configured, which is %s.\n"+
			"  Either the disk was not mounted, or a different one is. Refusing rather\n"+
			"  than starting: flight data written now would land on the wrong disk and\n"+
			"  be reported as saved.",
			ErrWrongDevice, s.root, mounted.source, mounted.fstype, s.device, want)
	}
	return nil
}

// diskByUUIDDir is where udev publishes filesystem UUIDs as symlinks.
const diskByUUIDDir = "/dev/disk/by-uuid"

// filesystemAt reports the filesystem mounted at or above path, or nil if none is.
//
// The longest matching mount point wins, so a root inside a mounted filesystem
// resolves to that filesystem rather than to "/" -- which is what makes the
// check meaningful, since /mnt/rocsar_ssd is not itself a mount point when the
// SSD is missing but the mount table still lists "/" as covering it.
func filesystemAt(path string) (*mount, error) {
	body, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}

	var best *mount
	for _, line := range strings.Split(string(body), "\n") {
		if line == "" {
			continue
		}
		// id parent maj:min root mountpoint options [optional...] - fstype source superopts
		pre, post, ok := strings.Cut(line, " - ")
		if !ok {
			continue
		}
		fields := strings.Fields(pre)
		if len(fields) < 5 {
			continue
		}
		point := unescapeMount(fields[4])
		// HasPrefix against point+"/" is wrong for point == "/", where it asks
		// whether the path begins "//" and the root mount then matches nothing but
		// itself -- so a data root anywhere on the root filesystem reported no
		// filesystem at all, which is the one case this exists to detect. The
		// root is handled separately and needs no separator.
		under := point == "/" || strings.HasPrefix(abs, point+"/")
		if abs != point && !under {
			continue
		}
		rest := strings.Fields(post)
		if len(rest) < 2 {
			continue
		}
		if best == nil || len(point) > len(best.point) {
			best = &mount{point: point, fstype: rest[0], source: rest[1]}
		}
	}
	return best, nil
}

// mount is one line of mountinfo, reduced to what the check compares.
type mount struct {
	point  string
	fstype string
	source string
}

// unescapeMount decodes the octal escapes the kernel writes into mount paths.
//
// The interface allows space and three others in a mount point, and the kernel
// escapes them as \040 and friends. Without this, a mount point containing a
// space would never compare equal to its own path and the check would report a
// mismatch that does not exist. mountinfo's escaping is octal with exactly three
// digits.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 4
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
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
	// `cam` or `camera`: NameFor(storage.KindCamera, ...) produces
	// "camera-20261003-142530.jpg", which the narrower `^cam-` did NOT match --
	// "cam" followed by "era", not by "-". Every photograph the system took was
	// therefore classified `unknown` and listed as such.
	cameraName = regexp.MustCompile(`^cam(era)?-.*\.(jpg|jpeg)$`)
	// The acquisition program writes `rx_data_<localtime>.bin` into its SSD_PATH,
	// so the timestamp is part of the stem and the pattern has to allow for it. An
	// earlier version anchored `rx_data` directly against the extension and
	// matched nothing the program actually produces.
	//
	// The directory does not appear in the pattern because a listing is relative
	// to the root. The program's own second copy goes to
	// <programDir>/Data/raw_data/, which is outside the root and so never listed
	// here -- deliberately: it is a duplicate of a capture already served, and
	// showing both would have the operator download the same 34 MB twice.
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

// NameFor builds a sortable artefact name: "<kind>-<timestamp>.<ext>".
//
// The timestamp is local and formatted for lexical ordering, which is why it is
// not RFC3339. `camera-20261003-142530.jpg` sorts correctly as a plain string
// comparison; `camera-2026-10-03T14:25:30Z.jpg` does not compare usefully
// against anything.
//
// UNIQUE only to the second. Two captures inside one second produce the same
// name and the second overwrites the first. That is accepted: the alternative is
// sub-second precision, which reads badly and sorts no better, and an operator
// taking two photographs of the same thing a second apart has almost certainly
// taken the same photograph twice. A caller that needs distinct names must add
// its own discriminator -- see camera.Mock, which does.
func NameFor(kind, ext string) string {
	return fmt.Sprintf("%s-%s.%s", kind, time.Now().Format("20060102-150405"), ext)
}

// UniqueNameFor is NameFor with a caller-supplied discriminator, for the case
// where two artefacts of the same kind in the same second really are different
// things.
func UniqueNameFor(kind, discriminator, ext string) string {
	return fmt.Sprintf("%s-%s-%s.%s", kind, discriminator, time.Now().Format("20060102-150405"), ext)
}
