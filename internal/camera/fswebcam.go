package camera

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// fswebcamCapturer photographs by running fswebcam.
//
// WHY THIS EXISTS
// ---------------
// The hand-written V4L2 path in v4l2.go negotiates the camera correctly and then
// never receives a frame. On the UGREEN camera on the target Pi, every ioctl
// succeeds -- QUERYCAP, G_FMT, S_FMT, REQBUFS, STREAMON -- and then poll()
// returns POLLERR the instant STREAMON completes, every time, at every resolution
// (320x240 through 1920x1080), in both MJPEG and YUYV, with and without
// O_NONBLOCK, with and without S_INPUT, with and without a settle delay, and via
// both mmap/DQBUF and plain read(). fswebcam photographs the same camera, on the
// same node, at the same moment, and produces a real 320x240 JPEG.
//
// That gap was not worth more of the project's time than the rest of the system
// is worth. So the working mechanism is used and the one that is not is kept for
// the case where fswebcam is absent.
//
// The internal/qos dead-code pass deleted 291 lines of limiter for having no
// caller. This is the same shape of decision and it deserves the same scrutiny:
// what is here has a caller, is verified against real hardware, and the code it
// replaces is still reachable when fswebcam is not installed.
//
// It is NOT link shaping, and it is not a general-purpose V4L2 implementation. It
// shells out, so it needs the binary present, and it does whatever fswebcam does
// with the format.
type fswebcamCapturer struct {
	device string
	binary string
	log    *slog.Logger
	// tmpDir is where the intermediate file is written. fswebcam writes to a path
	// rather than to a descriptor, so there is no way around a temporary file.
	tmpDir string
}

// errNoFswebcam is returned when the binary is not installed, which is the signal
// to fall back to the built-in V4L2 path rather than to fail.
var errNoFswebcam = fmt.Errorf("fswebcam is not installed")

func newFswebcamCapturer(device, tmpDir string, log *slog.Logger) (*fswebcamCapturer, error) {
	path, err := exec.LookPath("fswebcam")
	if err != nil {
		return nil, errNoFswebcam
	}
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return nil, fmt.Errorf("camera: prepare the fswebcam scratch directory: %w", err)
	}
	if log == nil {
		log = slog.Default()
	}
	return &fswebcamCapturer{device: device, binary: path, log: log, tmpDir: tmpDir}, nil
}

// Capture writes one photograph and returns its bytes.
//
// The file is removed before returning on every path, including failure. A scratch
// JPEG left in the data directory after a failed capture is exactly the sort of
// thing an operator later finds and believes.
func (f *fswebcamCapturer) Capture(ctx context.Context) ([]byte, error) {
	out := filepath.Join(f.tmpDir, "fswebcam-scratch.jpg")
	_ = os.Remove(out)
	defer os.Remove(out)

	// Separate arguments, never a shell string: `device` comes from a config file
	// and must not be able to become a command.
	args := []string{"-d", f.device, "--no-banner", out}
	cmd := exec.CommandContext(ctx, f.binary, args...)

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
