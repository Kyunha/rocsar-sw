package camera

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rocsar/obc/internal/domain"
	"github.com/rocsar/obc/internal/storage"
)

// The tests in this file are the ones this package did not have.
//
// It is the only hardware package in the repository with no test file at all,
// and the thing it produces -- a file written by a subprocess this code cannot
// see into, whose output nothing above it ever inspects -- is exactly the kind
// of thing a test is for. Every layer above a successful capture reports success
// without ever looking at the JPEG: the dispatcher returns a name, the HTTP
// server serves it, the console displays it. A capture that produced a truncated
// file, a PNG named .jpg, or a photograph at a resolution nobody asked for is a
// success at all four of them.

// ---------------------------------------------------------------------------
// The command line
// ---------------------------------------------------------------------------

// The default settings must produce the invocation that has been working, byte
// for byte, plus the resolution that was previously implied rather than passed.
//
// This is the test that keeps "add a control" from being a regression: every new
// option is passed only when an operator asked for the setting behind it, so the
// failure mode of an fswebcam that has never heard of a flag is a failed capture
// for the operator who wanted that feature rather than a camera that stopped
// working. See FswebcamArgs.
func TestTheDefaultSettingsReproduceThePreExistingInvocation(t *testing.T) {
	got := FswebcamArgs("/dev/video0", "/tmp/out.jpg", domain.DefaultCameraParams())
	want := []string{"-d", "/dev/video0", "--no-banner", "-r", "384x288", "/tmp/out.jpg"}
	if !slices.Equal(got, want) {
		t.Errorf("default args =\n  %v\nwant\n  %v", got, want)
	}
}

// Each setting reaches fswebcam as its own flag. A case per setting, because a
// table is what makes an omission visible: one field wired and eleven looking
// plausible is the failure mode.
func TestEachSettingReachesFswebcam(t *testing.T) {
	cases := []struct {
		name  string
		patch domain.CameraParamsPatch
		want  []string
	}{
		{
			name:  "jpeg quality",
			patch: domain.CameraParamsPatch{JPEGQuality: ptr(uint32(40))},
			want:  []string{"--jpeg", "40"},
		},
		{
			name:  "frames",
			patch: domain.CameraParamsPatch{Frames: ptr(uint32(4))},
			want:  []string{"-F", "4"},
		},
		{
			name:  "skip",
			patch: domain.CameraParamsPatch{Skip: ptr(uint32(6))},
			want:  []string{"-S", "6"},
		},
		{
			name:  "delay of 500 ms becomes half a second",
			patch: domain.CameraParamsPatch{DelayMs: ptr(uint32(500))},
			want:  []string{"-D", "0.5"},
		},
		{
			name:  "a whole second of delay is not spelled 1.0",
			patch: domain.CameraParamsPatch{DelayMs: ptr(uint32(2000))},
			want:  []string{"-D", "2"},
		},
		{
			name:  "resolution",
			patch: domain.CameraParamsPatch{Resolution: ptr("1280x720")},
			want:  []string{"-r", "1280x720"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := Merge(domain.DefaultCameraParams(), tc.patch)
			if err != nil {
				t.Fatalf("Merge: %v", err)
			}
			args := FswebcamArgs("/dev/video0", "/tmp/out.jpg", p)

			for i := 0; i+1 < len(tc.want); i += 2 {
				flag, val := tc.want[i], tc.want[i+1]
				idx := slices.Index(args, flag)
				if idx < 0 {
					t.Fatalf("%v does not contain %s %s", args, flag, val)
				}
				if args[idx+1] != val {
					t.Errorf("%s is followed by %q, want %q", flag, args[idx+1], val)
				}
			}
		})
	}
}

// The resolution is the one option passed even at its default, and the reason is
// worth pinning: omitting it inherits fswebcam's default on whichever version is
// installed, which is a resolution nobody chose and that this code could not
// report.
func TestTheResolutionIsAlwaysPassed(t *testing.T) {
	p := domain.DefaultCameraParams()
	if p.Resolution != DefaultResolution {
		t.Fatalf("the default resolution is %q, not %q; the test is about the default",
			p.Resolution, DefaultResolution)
	}
	args := FswebcamArgs("/dev/video0", "/tmp/out.jpg", p)
	if !slices.Contains(args, "-r") {
		t.Errorf("args = %v, want an explicit -r even at the default resolution", args)
	}
}

// fswebcam applies options in the order they appear and only to images output
// after them. An output filename anywhere but last gets none of the settings.
func TestTheOutputFilenameIsTheLastArgument(t *testing.T) {
	p := domain.DefaultCameraParams()
	p.JPEGQuality = 60
	args := FswebcamArgs("/dev/video0", "/tmp/out.jpg", p)
	if got := args[len(args)-1]; got != "/tmp/out.jpg" {
		t.Errorf("last argument is %q, want the output filename", got)
	}
}

// A device name from a config file must never be able to become an argument of
// its own. Separate arguments all the way down, no shell anywhere.
func TestADeviceNameCannotBecomeAnArgument(t *testing.T) {
	args := FswebcamArgs("/dev/video0 --frames 8", "/tmp/out.jpg", domain.DefaultCameraParams())
	idx := slices.Index(args, "-d")
	if idx < 0 || args[idx+1] != "/dev/video0 --frames 8" {
		t.Errorf("device was split across arguments: %v", args)
	}
	// The value is one element whatever it contains, so nothing after -d can be
	// read as a flag.
	for i, a := range args {
		if i != idx+1 && strings.Contains(a, "frames") {
			t.Errorf("%q leaked out of the -d value: %v", a, args)
		}
	}
}

// ---------------------------------------------------------------------------
// A capture, end to end over a fake fswebcam
// ---------------------------------------------------------------------------

// argvLog is what the fake records. One test reads it to check the command line
// that actually ran, not only the one FswebcamArgs would produce.
const argvLog = "argv.txt"

func TestACaptureWritesADecodableJPEGTakenAtTheRequestedResolution(t *testing.T) {
	h := newHarness(t, 640, 480)
	h.camera.params.Resolution = "640x480"

	photo, err := h.camera.Capture(context.Background())
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	if photo.Width != 640 || photo.Height != 480 {
		t.Errorf("photo is %dx%d, want 640x480", photo.Width, photo.Height)
	}
	if photo.SizeBytes == 0 {
		t.Error("SizeBytes is zero")
	}

	// It is on disk, at the path it said, and it decodes.
	body, err := os.ReadFile(filepath.Join(h.dir, photo.Path))
	if err != nil {
		t.Fatalf("the photograph is not where it said it was: %v", err)
	}
	img, err := jpeg.Decode(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("%d bytes that are not a decodable JPEG: %v", len(body), err)
	}
	if b := img.Bounds(); b.Dx() != 640 || b.Dy() != 480 {
		t.Errorf("decoded %dx%d, want 640x480", b.Dx(), b.Dy())
	}

	// The command line that ran is the one the tests above assert, checked again
	// through the subprocess boundary where a wiring mistake would hide.
	if !slices.Contains(h.argv(), "-r") {
		t.Errorf("fswebcam was not asked for a resolution: %v", h.argv())
	}
}

// The requested resolution is a REQUEST. fswebcam's man page: "the actual
// resolution used may differ if the source or device cannot capture at the
// specified resolution."
//
// This is the whole reason Photo carries dimensions. A capture that asked for
// 1280x720 and got 320x240 must report 320x240, because a console showing
// 1280x720 next to a 320x240 file would be reporting a number no device agreed
// to -- and the mismatch is the finding an operator needs.
func TestAPhotographReportsTheDimensionsItHasNotTheOnesRequested(t *testing.T) {
	h := newHarness(t, 320, 240) // the fake writes 320x240 whatever it is asked for
	h.camera.params.Resolution = "1280x720"

	photo, err := h.camera.Capture(context.Background())
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	if photo.Width != 320 || photo.Height != 240 {
		t.Errorf("photo reports %dx%d, want the 320x240 that was actually written "+
			"(a clamped 1280x720 request is not a 1280x720 photograph)",
			photo.Width, photo.Height)
	}
	// And the request is still on record, because the operator asked for it and
	// the log is where they find out it was declined.
	got, err := h.camera.Params(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Resolution != "1280x720" {
		t.Errorf("the settings in force say %q; the request should survive the clamp",
			got.Resolution)
	}
}

// Bytes that are not a JPEG must fail the capture, not be stored under a .jpg
// name. Every layer above this one would report the resulting file as a
// photograph.
func TestACaptureThatWritesSomethingElseIsRefused(t *testing.T) {
	h := newHarness(t, 0, 0)
	h.script("#!/bin/sh\nfor a in \"$@\"; do last=\"$a\"; done\nprintf 'not a jpeg' > \"$last\"\n")

	photo, err := h.camera.Capture(context.Background())
	if err == nil {
		t.Fatalf("a capture that wrote 11 non-JPEG bytes succeeded: %+v", photo)
	}
	if !strings.Contains(err.Error(), "not a JPEG") {
		t.Errorf("error = %v, want it to name the problem", err)
	}
	if photo != nil {
		t.Errorf("a refused capture returned a photograph: %+v", photo)
	}
}

// fswebcam's stderr is the only diagnostic there is when a capture fails on the
// aircraft, so it must survive into the error.
func TestAFailedCaptureCarriesFswebcamsOwnDiagnostic(t *testing.T) {
	h := newHarness(t, 0, 0)
	h.script("#!/bin/sh\necho 'VIDIOC_STREAMON: Invalid argument' >&2\nexit 1\n")

	_, err := h.camera.Capture(context.Background())
	if err == nil {
		t.Fatal("a capture whose subprocess failed succeeded")
	}
	if !strings.Contains(err.Error(), "VIDIOC_STREAMON") {
		t.Errorf("error = %v, want fswebcam's own message", err)
	}
}

// A scratch JPEG left behind after a failed capture is a file an operator later
// finds in the data directory and believes.
func TestTheScratchFileIsRemovedEvenWhenTheCaptureFails(t *testing.T) {
	h := newHarness(t, 0, 0)
	h.script("#!/bin/sh\nfor a in \"$@\"; do last=\"$a\"; done\nprintf 'not a jpeg' > \"$last\"\n")

	if _, err := h.camera.Capture(context.Background()); err == nil {
		t.Fatal("expected the capture to fail")
	}

	scratch := filepath.Join(h.dir, ".camera-scratch", "fswebcam-scratch.jpg")
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Errorf("the scratch file survived a failed capture: %v", err)
	}
}

// LastPhoto is nil until something has been photographed.
//
// It was not: the field was written on every capture and read by nobody, and the
// console rendered "last " with nothing after it for the life of the system. It
// is asserted here rather than in the telemetry test because the interesting
// half is that it is nil to begin with -- a port that returns a zero Photo would
// make the console show a filename that does not exist.
func TestLastPhotoIsNilUntilOneIsTaken(t *testing.T) {
	h := newHarness(t, 320, 240)

	if got := h.camera.LastPhoto(); got != nil {
		t.Fatalf("LastPhoto is %+v before any capture; want nil", got)
	}

	photo, err := h.camera.Capture(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	got := h.camera.LastPhoto()
	if got == nil {
		t.Fatal("LastPhoto is nil after a capture")
	}
	if got.Name != photo.Name {
		t.Errorf("LastPhoto is %q, want %q", got.Name, photo.Name)
	}
	if got.Width != 320 || got.Height != 240 {
		t.Errorf("LastPhoto carries %dx%d, want the decoded 320x240", got.Width, got.Height)
	}

	// A copy, so a caller cannot reach into the camera's state.
	got.Width = 1
	if again := h.camera.LastPhoto(); again.Width == 1 {
		t.Error("LastPhoto handed out its own struct rather than a copy")
	}
}

// The shot counter counts captures, including two that landed on the same
// artefact name.
func TestPhotosTakenCountsEveryCapture(t *testing.T) {
	h := newHarness(t, 320, 240)

	for i := uint64(1); i <= 3; i++ {
		if _, err := h.camera.Capture(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := h.camera.PhotosTaken(); got != i {
			t.Errorf("after %d captures PhotosTaken = %d", i, got)
		}
	}
}

// Two photographs inside one second share an artefact name and the second
// overwrites the first. That is storage.NameFor's documented and accepted
// behaviour -- "an operator taking two photographs of the same thing a second
// apart has almost certainly taken the same photograph twice" -- and it is
// pinned here so that changing it is a decision somebody makes rather than
// something that happens.
//
// camera.Mock does NOT do this: it adds the shot number as a discriminator, so a
// bench and a flight differ on double shots. If this test ever starts failing
// because the policy changed, the mock and the real camera are the two things to
// bring back into agreement.
func TestTwoCapturesInOneSecondShareANameAndTheSecondWins(t *testing.T) {
	h := newHarness(t, 320, 240)

	first, err := h.camera.Capture(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.camera.Capture(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if first.Name != second.Name {
		t.Skip("these two captures fell in different seconds; the collision needs the same one")
	}
	if first.Path != second.Path {
		t.Errorf("the same name gave two paths: %q and %q", first.Path, second.Path)
	}
}

// The settings survive an OBC restart, which is the reason they are in a file.
func TestSettingsSurviveARestart(t *testing.T) {
	h := newHarness(t, 320, 240)

	q := uint32(45)
	if err := h.camera.SetParams(context.Background(),
		domain.CameraParamsPatch{JPEGQuality: &q}); err != nil {
		t.Fatalf("SetParams: %v", err)
	}

	// A second camera over the same data directory is a restart as far as the
	// settings file is concerned.
	restarted := NewCapture(h.device, "photos", h.store, domain.DefaultCameraParams(),
		slog.New(discardLogger{}))

	got, err := restarted.Params(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.JPEGQuality != 45 {
		t.Errorf("after a restart the JPEG quality is %d, want the 45 that was set", got.JPEGQuality)
	}
}

// ---------------------------------------------------------------------------
// The harness
// ---------------------------------------------------------------------------

type harness struct {
	t      *testing.T
	dir    string
	device string
	store  *storage.Store
	camera *Capture
	bin    string
}

// newHarness returns a camera whose fswebcam is a script.
//
// width and height are the dimensions the fake writes. Zero means "write nothing
// useful", for the failure cases.
func newHarness(t *testing.T, width, height int) *harness {
	t.Helper()
	dir := t.TempDir()

	// The device has to exist: captureOnce stats it before starting anything,
	// and that check is worth keeping.
	device := filepath.Join(dir, "video0")
	if err := os.WriteFile(device, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	h := &harness{t: t, dir: dir, device: device, bin: filepath.Join(dir, "fswebcam-fake")}
	h.script(repeatJPEGScript(t, dir, h.bin, width, height))

	store := storage.New(dir, "")
	if err := store.Check(); err != nil {
		t.Fatalf("store: %v", err)
	}
	h.store = store
	h.camera = NewCapture(device, "photos", store, domain.DefaultCameraParams(),
		slog.New(discardLogger{}))
	h.camera.bin = h.bin
	return h
}

// script replaces the fake with something else, for the failure paths.
func (h *harness) script(body string) {
	h.t.Helper()
	if err := os.WriteFile(h.bin, []byte(body), 0o755); err != nil {
		h.t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(h.dir, argvLog))
}

// argv is what the fake was actually invoked with.
func (h *harness) argv() []string {
	h.t.Helper()
	body, err := os.ReadFile(filepath.Join(h.dir, argvLog))
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimRight(string(body), "\n"), "\n")
}

// repeatJPEGScript writes a real JPEG to whatever the last argument is, and
// records the argument list beside it.
//
// Both halves matter: the JPEG is what makes the capture succeed, and the log is
// what lets a test assert on the command line that crossed the subprocess
// boundary rather than on the one the pure function produced.
func repeatJPEGScript(t *testing.T, dir, bin string, width, height int) string {
	t.Helper()
	body := "#!/bin/sh\n"
	body += "for a in \"$@\"; do last=\"$a\"; done\n"
	body += "for a in \"$@\"; do printf '%s\\n' \"$a\"; done > " +
		filepath.Join(dir, argvLog) + "\n"
	if width > 0 && height > 0 {
		fixture := filepath.Join(dir, "fixture.jpg")
		if err := os.WriteFile(fixture, testJPEG(t, width, height), 0o644); err != nil {
			t.Fatal(err)
		}
		body += "cp " + fixture + " \"$last\"\n"
	}
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return body
}

// testJPEG encodes a gradient of the requested size.
func testJPEG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 255 / width), G: uint8(y * 255 / height), B: 0x40, A: 0xFF})
		}
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, nil); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func ptr[T any](v T) *T { return &v }

type discardLogger struct{}

func (discardLogger) Enabled(context.Context, slog.Level) bool  { return false }
func (discardLogger) Handle(context.Context, slog.Record) error { return nil }
func (d discardLogger) WithAttrs([]slog.Attr) slog.Handler      { return d }
func (d discardLogger) WithGroup(string) slog.Handler           { return d }
