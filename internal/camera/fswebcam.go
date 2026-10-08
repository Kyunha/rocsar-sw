// Package camera captures stills from a USB camera.
//
// Snapshot only. There is no live stream: it consumed bandwidth that constraint
// H1 does not have, in exchange for a picture nobody needed continuously. One
// photograph on command, written to the SSD, fetched over HTTP.
//
// The capture mechanism is fswebcam. A hand-written V4L2 path was removed: it
// negotiated the camera correctly and then never received a frame on the target
// hardware (every ioctl succeeded, poll() returned POLLERR after STREAMON).
// fswebcam photographs the same node at the same moment and works.
//
// What this package will and will not do is set out in params_contract.go. The
// short version: every setting here is a fswebcam option, every bound is cited
// to the fswebcam(1) man page, and a requested resolution is a REQUEST -- what
// a photograph really is comes back decoded out of the JPEG, because fswebcam's
// man page says outright that "the actual resolution used may differ if the
// source or device cannot capture at the specified resolution".
package camera

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // registered so DecodeConfig reads a JPEG's header
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/rocsar/obc/internal/domain"
	"github.com/rocsar/obc/internal/storage"
)

// fswebcamBinary is the capture program, found on PATH.
const fswebcamBinary = "fswebcam"

// ErrBusy is returned when a capture is asked for while one is running.
var ErrBusy = errors.New("camera: a capture is already in progress")

// Capture is a snapshot camera that shells out to fswebcam.
type Capture struct {
	device string
	store  *storage.Store
	dir    string
	log    *slog.Logger

	// bin is fswebcam's path, a field rather than a constant so a test can
	// point it at a script that writes a known photograph. That substitution is
	// the only way to test a subprocess wrapper without a camera, and testing it
	// is not optional: the bytes fswebcam writes are the whole output of this
	// program, and everything above here reports success without ever seeing
	// them.
	bin string

	mu     sync.Mutex
	state  domain.SubsystemState
	shots  uint64
	last   *domain.Photo
	params domain.CameraParams
}

var _ domain.Camera = (*Capture)(nil)

// NewCapture returns a camera writing into the given subdirectory of the store.
//
// `initial` is the configuration to use before anything has been set: it wins
// only when there is no stored settings file, and it is validated here so that a
// bad rocsar.toml is a startup error rather than a discovery made by an operator
// in the air. A stored file always wins over it, and the reason is logged when
// the two disagree, because "the file says 640x480 and the config says
// 1920x1080" is a fact somebody will need.
func NewCapture(device, dir string, store *storage.Store, initial domain.CameraParams, log *slog.Logger) *Capture {
	if log == nil {
		log = slog.Default()
	}
	params := initial
	if err := Validate(params); err != nil {
		log.Warn("the configured camera settings are not usable; using the defaults",
			"err", err, "defaults", Defaults())
		params = Defaults()
	}

	stored, err := LoadParams(store)
	switch {
	case err == nil:
		if stored != params {
			// Not a warning: this is the designed precedence, and saying so is
			// how an operator finds out their rocsar.toml is not what the
			// camera is using.
			log.Info("stored camera settings are in force; the configured ones are only a default",
				"file", ParamsFileName, "in_force", stored, "configured", params)
		}
		params = stored
	case errors.Is(err, ErrNoParamsFile):
		// No file is the normal first-flight case, so this is not worth a
		// warning -- but the settings ARE now the ones in force and the file
		// does not exist to say so, so the initial value is written once. That
		// makes the file's contents and the settings in force agree from the
		// first photograph, rather than diverging the first time somebody
		// changes one.
		if serr := SaveParams(store, params); serr != nil {
			// A read-only SSD must not stop the camera: every other layer here
			// degrades and reports, per ARCHITECTURE.md 8. The settings stay
			// correct in memory and are lost on restart, and the log says so.
			log.Warn("could not write the initial camera settings; they will not survive a restart",
				"file", ParamsFileName, "err", serr)
		}
	default:
		log.Warn("stored camera settings are unusable; using the configured ones", "err", err)
	}

	return &Capture{
		device: device,
		store:  store,
		dir:    dir,
		log:    log,
		bin:    fswebcamBinary,
		params: params,
		state:  domain.SubsystemDisconnected,
	}
}

func (c *Capture) Device() string { return c.device }

func (c *Capture) State() domain.SubsystemState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

func (c *Capture) PhotosTaken() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.shots
}

// LastPhoto is the most recent photograph, or nil if none has been taken.
//
// It is a copy, so a caller cannot reach into the camera's state and it can
// never change under the caller mid-read.
func (c *Capture) LastPhoto() *domain.Photo {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.last == nil {
		return nil
	}
	out := *c.last
	return &out
}

// Params returns the settings in force.
func (c *Capture) Params(ctx context.Context) (domain.CameraParams, error) {
	if err := ctx.Err(); err != nil {
		return domain.CameraParams{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.params, nil
}

// SetParams validates, stores and persists a partial update.
//
// The order is load-bearing: the merged settings are validated BEFORE anything
// is written, and the in-memory copy is only replaced after the file write has
// succeeded. A failed save therefore leaves the camera running the settings it
// was running, rather than running settings that a restart would throw away --
// which would be a discrepancy between what telemetry reports and what the next
// boot does, and that is the class of lie this system works hardest to avoid.
//
// The lock is held across the write on purpose. It is a few hundred bytes to a
// temp file plus a rename, and the alternative -- releasing it to write -- opens
// a window where a concurrent capture reads settings the file does not yet
// hold.
func (c *Capture) SetParams(ctx context.Context, patch domain.CameraParamsPatch) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if Empty(patch) {
		return fmt.Errorf(
			"camera: no settings were given; a partial update must name at least one. " +
				"Use a separate 'reset to defaults' if that is what you meant")
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	next, err := Merge(c.params, patch)
	if err != nil {
		return err
	}
	if err := SaveParams(c.store, next); err != nil {
		return err
	}
	prev := c.params
	c.params = next
	c.log.Info("camera settings updated",
		"changed", describeChanges(prev, next), "in_force", next)
	return nil
}

// describeChanges names only the fields that moved.
//
// The changed ones and no others: a settings change is a thing an operator
// reads in a log to confirm what they typed, and a line listing five fields they
// did not touch is a line they have to check each one of.
func describeChanges(prev, next domain.CameraParams) string {
	var parts []string
	changed := func(name string, was, is any) {
		if fmt.Sprint(was) != fmt.Sprint(is) {
			parts = append(parts, fmt.Sprintf("%s %v -> %v", name, was, is))
		}
	}
	changed("quality", qualityName(prev.JPEGQuality), qualityName(next.JPEGQuality))
	changed("resolution", prev.Resolution, next.Resolution)
	changed("frames", prev.Frames, next.Frames)
	changed("skip", prev.Skip, next.Skip)
	changed("delay_ms", prev.DelayMs, next.DelayMs)
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}

// qualityName spells the automatic case out rather than printing a zero.
//
// Zero here means fswebcam's -1, and an operator reading "quality 85 -> 0" in a
// log has been told their picture quality was turned down to nothing.
func qualityName(q uint32) string {
	if q == DefaultQuality {
		return "auto"
	}
	return strconv.FormatUint(uint64(q), 10)
}

// Capture grabs one JPEG at the settings in force and writes it to the data
// directory atomically.
func (c *Capture) Capture(ctx context.Context) (*domain.Photo, error) {
	c.mu.Lock()
	if c.state == domain.SubsystemBusy {
		c.mu.Unlock()
		return nil, ErrBusy
	}
	c.state = domain.SubsystemBusy
	// Snapshotted here rather than read inside captureOnce, so a settings
	// change landing mid-capture cannot produce a photograph whose settings are
	// not the ones that were asked for.
	params := c.params
	c.mu.Unlock()

	photo, err := c.captureOnce(ctx, params)

	c.mu.Lock()
	if err != nil {
		c.state = domain.SubsystemDisconnected
	} else {
		c.state = domain.SubsystemReady
		c.shots++
		c.last = photo
	}
	c.mu.Unlock()

	return photo, err
}

func (c *Capture) captureOnce(ctx context.Context, params domain.CameraParams) (*domain.Photo, error) {
	if _, err := os.Stat(c.device); err != nil {
		return nil, fmt.Errorf("camera: %s is not present", c.device)
	}

	body, err := c.runFswebcam(ctx, params)
	if err != nil {
		return nil, err
	}

	return c.savePhoto(body)
}

// runFswebcam writes one photograph to a scratch file and returns its bytes.
//
// The file is removed before returning on every path, including failure. A
// scratch JPEG left in the data directory after a failed capture is exactly the
// sort of thing an operator later finds and believes.
func (c *Capture) runFswebcam(ctx context.Context, params domain.CameraParams) ([]byte, error) {
	// Scratch space under the data root rather than /tmp so that a full or
	// read-only /tmp cannot silently become a capture failure, and so the
	// scratch file is on the same filesystem as the store, which is what makes
	// WriteFileAtomic's rename atomic.
	tmpDir := filepath.Join(c.store.Root(), ".camera-scratch")
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return nil, fmt.Errorf("camera: prepare scratch directory: %w", err)
	}
	out := filepath.Join(tmpDir, "fswebcam-scratch.jpg")
	_ = os.Remove(out)
	defer os.Remove(out)

	args := fswebcamArgs(c.device, out, params)

	// Separate arguments, never a shell string: `device` comes from a config
	// file and must not be able to become a command.
	cmd := exec.CommandContext(ctx, c.bin, args...)

	// fswebcam prints its banner and progress to stderr even with --no-banner.
	// Keep it: when this fails, "Illegal" or a V4L2 error on stderr is the only
	// diagnostic there is, and discarding it is how this took as long as it did.
	if o, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("camera: fswebcam failed: %w: %s", err, firstLine(o))
	}

	body, err := os.ReadFile(out)
	if err != nil {
		return nil, fmt.Errorf("camera: fswebcam reported success but wrote nothing readable: %w", err)
	}
	if len(body) < 2 || body[0] != 0xFF || body[1] != 0xD8 {
		return nil, fmt.Errorf("camera: fswebcam wrote %d bytes that are not a JPEG; "+
			"a capture that fails must not look like a capture that worked", len(body))
	}
	return body, nil
}

// fswebcamArgs builds the argument list for one capture.
//
// # Why most options are omitted rather than passed at their default value
//
// An option this code is unsure of is only passed when an operator asked for
// the setting it controls. That matters because the bound on fswebcam here is a
// man page: it is installed by apt on the aircraft, is archived upstream, and
// nobody has written down its version. If the installed build does not know
// `--jpeg`, then passing `--jpeg` unconditionally turns a no-op field into a
// total loss of the camera, and the field being no-op is the state we found it
// in. Omitting an option that equals its default means the default capture path
// is byte-for-byte the invocation that has been working, and an unsupported flag
// can only break a capture whose operator specifically asked for that feature --
// with fswebcam's own complaint on stderr naming the flag.
//
// # The exception, and why -r is passed even at its default
//
// The resolution IS always passed. Omitting it would inherit whatever fswebcam's
// default happens to be on the installed version, which is a resolution nobody
// in this project chose and which this code could not report. `-r` is also the
// one option that cannot be missing from any fswebcam that works at all, so
// passing it costs nothing in the risk the rest of this function manages.
//
// # Test note
//
// This is a pure function of three values, deliberately, so that the exact
// command line is assertable without a camera, a PATH or a subprocess. That is
// most of what fswebcam_test.go checks.
func fswebcamArgs(device, out string, p domain.CameraParams) []string {
	args := []string{"-d", device, "--no-banner"}

	// -r, always: see above.
	args = append(args, "-r", p.Resolution)

	if p.JPEGQuality != DefaultQuality {
		args = append(args, "--jpeg", strconv.FormatUint(uint64(p.JPEGQuality), 10))
	}
	if p.Frames != 1 {
		args = append(args, "-F", strconv.FormatUint(uint64(p.Frames), 10))
	}
	if p.Skip != 1 {
		args = append(args, "-S", strconv.FormatUint(uint64(p.Skip), 10))
	}
	if p.DelayMs != 0 {
		// fswebcam's -D is in seconds and accepts a fraction; this is the one
		// place a value crosses into a float spelling, so it is formatted once,
		// here, and nowhere else.
		args = append(args, "-D", strconv.FormatFloat(float64(p.DelayMs)/1000, 'f', -1, 64))
	}

	// The output filename last, because fswebcam applies options in the order
	// they appear and only to images output after them. Putting it anywhere
	// else would apply half these settings to nothing.
	return append(args, out)
}

// savePhoto stores JPEG bytes and describes them.
//
// The dimensions come from decoding the file's header, not from the request:
// fswebcam's man page warns that "the actual resolution used may differ if the
// source or device cannot capture at the specified resolution", and a Photo that
// described the request would be describing a file nobody wrote.
func (c *Capture) savePhoto(buf []byte) (*domain.Photo, error) {
	name := storage.NameFor(storage.KindCamera, "jpg")
	sub, err := c.store.Sub(c.dir)
	if err != nil {
		return nil, err
	}
	if err := sub.WriteFileAtomic(name, buf, 0o644); err != nil {
		return nil, err
	}
	photo := &domain.Photo{
		Name:      name,
		SizeBytes: uint64(len(buf)),
		Kind:      storage.KindCamera,
		Path:      c.dir + "/" + name,
	}
	photo.Width, photo.Height = jpegSize(buf)
	return photo, nil
}

// jpegSize returns a JPEG's pixel dimensions from its header, or (0, 0).
//
// DecodeConfig reads the SOF marker and stops: no pixels are decoded, so this is
// cheap enough to do on every photograph. Zero means the header could not be
// read, which is a different fact from a photograph that is 0x0 -- and it is
// left as zero rather than guessed, because a made-up resolution is precisely
// the failure this package exists to stop reporting.
func jpegSize(buf []byte) (uint32, uint32) {
	if len(buf) < 2 || buf[0] != 0xFF || buf[1] != 0xD8 {
		return 0, 0
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(buf))
	if err != nil {
		return 0, 0
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return 0, 0
	}
	return uint32(cfg.Width), uint32(cfg.Height)
}

// Close satisfies domain.Shutdown. A one-shot camera holds nothing between
// captures, so this only clears the reported state.
func (c *Capture) Close() error { return nil }

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	const max = 200
	if len(s) > max {
		s = s[:max] + "..."
	}
	if s == "" {
		return "(no output)"
	}
	return s
}
