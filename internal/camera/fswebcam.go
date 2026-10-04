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
package camera

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/rocsar/obc/internal/domain"
	"github.com/rocsar/obc/internal/storage"
)

// Capture is a snapshot camera that shells out to fswebcam.
type Capture struct {
	device string
	store  *storage.Store
	dir    string

	quality int

	mu     sync.Mutex
	state  domain.SubsystemState
	shots  uint64
	last   string
}

var _ domain.Camera = (*Capture)(nil)

// NewCapture returns a camera writing into the given subdirectory of the store.
func NewCapture(device, dir string, store *storage.Store, quality int) *Capture {
	if quality <= 0 || quality > 100 {
		quality = 85
	}
	return &Capture{
		device:  device,
		store:   store,
		dir:     dir,
		quality: quality,
		state:   domain.SubsystemDisconnected,
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

// Capture grabs one JPEG and writes it to the data directory atomically.
func (c *Capture) Capture(ctx context.Context) (*domain.Photo, error) {
	c.mu.Lock()
	if c.state == domain.SubsystemBusy {
		c.mu.Unlock()
		return nil, fmt.Errorf("camera: a capture is already in progress")
	}
	c.state = domain.SubsystemBusy
	c.mu.Unlock()

	photo, err := c.captureOnce(ctx)

	c.mu.Lock()
	if err != nil {
		c.state = domain.SubsystemDisconnected
	} else {
		c.state = domain.SubsystemReady
		c.shots++
		c.last = photo.Name
	}
	c.mu.Unlock()

	return photo, err
}

func (c *Capture) captureOnce(ctx context.Context) (*domain.Photo, error) {
	if _, err := os.Stat(c.device); err != nil {
		return nil, fmt.Errorf("camera: %s is not present", c.device)
	}

	body, err := c.runFswebcam(ctx)
	if err != nil {
		return nil, err
	}

	return c.savePhoto(body)
}

// runFswebcam writes one photograph to a scratch file and returns its bytes.
//
// The file is removed before returning on every path, including failure. A scratch
// JPEG left in the data directory after a failed capture is exactly the sort of
// thing an operator later finds and believes.
func (c *Capture) runFswebcam(ctx context.Context) ([]byte, error) {
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

	// Separate arguments, never a shell string: `device` comes from a config file
	// and must not be able to become a command.
	args := []string{"-d", c.device, "--no-banner", out}
	cmd := exec.CommandContext(ctx, "fswebcam", args...)

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

// savePhoto stores JPEG bytes and describes them.
func (c *Capture) savePhoto(buf []byte) (*domain.Photo, error) {
	name := storage.NameFor(storage.KindCamera, "jpg")
	sub, err := c.store.Sub(c.dir)
	if err != nil {
		return nil, err
	}
	if err := sub.WriteFileAtomic(name, buf, 0o644); err != nil {
		return nil, err
	}
	return &domain.Photo{
		Name:      name,
		SizeBytes: uint64(len(buf)),
		Kind:      storage.KindCamera,
		Path:      c.dir + "/" + name,
	}, nil
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
