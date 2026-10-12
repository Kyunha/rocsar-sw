// Command camera_bench takes one photograph and checks it.
//
//	camera_bench                        snapshot and report
//	camera_bench -device /dev/video1    against another node
//	camera_bench -out /tmp/shot.jpg     keep a copy
//	camera_bench -resolution 1280x720   ask the device for something else
//	camera_bench -quality 40 -print-args  check the settings without looking
//
// It composes internal/camera, so it captures exactly as the server does. What
// it adds is the part the server cannot do: it DECODES the JPEG it just wrote,
// checks the dimensions are usable, and COMPARES them against the resolution
// that was asked for.
//
// That comparison is the point. A camera can silently give you a different
// resolution than the one requested, so a snapshot can succeed, be written, be
// fetchable over HTTP, and be the wrong picture -- and every layer above reports
// success. fswebcam's own man page warns of exactly this: "the actual resolution
// used may differ if the source or device cannot capture at the specified
// resolution". Asking for a mode this camera cannot do is how an operator finds
// out, and the finding belongs in the tool that talks to the camera rather than
// in a guess.
//
// -print-args prints the exact fswebcam command line without photographing
// anything. The flags that reach a camera we cannot see from here are gated on
// fswebcam's version, which nobody has written down; being able to read the
// command line is how a bench run confirms the installed build accepts them
// before a flight depends on it.
//
// There is no live stream: the camera is snapshot-only, and a stream would
// consume bandwidth the 115 kbit/s link does not have.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"image/jpeg"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/rocsar/obc/internal/camera"
	"github.com/rocsar/obc/internal/domain"
	"github.com/rocsar/obc/internal/storage"
)

// linkKbps is the shipped cap, used only to turn a photograph's size into the
// time it would take to fetch it. It is a bench convenience, so it is a flag
// rather than a constant: the whole reason to look at a photograph's size is the
// link it has to cross, and a bench on a laptop crosses none of them.
const defaultLinkKbps = 115

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "camera_bench: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		device     = flag.String("device", "/dev/video0", "video device node")
		dir        = flag.String("dir", "/mnt/rocsar/data", "data directory")
		sub        = flag.String("sub", "photos", "subdirectory within the data directory")
		out        = flag.String("out", "", "also copy the JPEG here")
		quality    = flag.Uint("quality", 0, "JPEG compression factor 1..95; 0 is automatic")
		resolution = flag.String("resolution", "", "requested capture size, WxH; empty is fswebcam's own default")
		frames     = flag.Uint("frames", 0, "frames to average into the output; 0 leaves the setting alone")
		skip       = flag.Uint("skip", 0, "frames to discard before the real one; 0 leaves the setting alone")
		delayMs    = flag.Uint("delay-ms", 0, "settle time in ms; 0 leaves the setting alone")
		linkKbps   = flag.Uint("link-kbps", defaultLinkKbps, "link rate for the fetch-time estimate, 0 to skip it")
		printArgs  = flag.Bool("print-args", false, "print the fswebcam command line and exit without capturing")
	)
	flag.Parse()

	params := domain.DefaultCameraParams()
	if *resolution != "" {
		params.Resolution = *resolution
	}
	// A zero on the numeric flags means "leave it at the default" rather than
	// "set it to zero", because zero is not a legal value for any of them --
	// except quality, where it means automatic and is the default anyway.
	if *quality != 0 {
		params.JPEGQuality = uint32(*quality)
	}
	if *frames != 0 {
		params.Frames = uint32(*frames)
	}
	if *skip != 0 {
		params.Skip = uint32(*skip)
	}
	if *delayMs != 0 {
		params.DelayMs = uint32(*delayMs)
	}

	// Validated before the device is even looked for, so a mistyped resolution
	// is reported as the typo it is rather than as a camera that is not there.
	if err := camera.Validate(params); err != nil {
		return err
	}

	if *printArgs {
		// The output filename is a placeholder: it is the last argument, and
		// this tool does not know the scratch path the capture will use.
		fmt.Printf("fswebcam %s\n",
			strings.Join(camera.FswebcamArgs(*device, "<out.jpg>", params), " "))
		return nil
	}

	if _, err := os.Stat(*device); err != nil {
		return fmt.Errorf("no camera at %s", *device)
	}

	store := storage.New(*dir, "")
	if err := store.Check(); err != nil {
		return fmt.Errorf("data directory: %w", err)
	}

	cam := camera.NewCapture(*device, *sub, store, params, slog.New(discardLogger{}))

	fmt.Printf("device %s\n", *device)
	fmt.Printf("asking   %s\n", describeParams(params))
	fmt.Println("capturing...")

	photo, err := cam.Capture(context.Background())
	if err != nil {
		return err
	}

	fmt.Printf("\ncaptured %s\n", photo.Name)
	fmt.Printf("  size      %d bytes\n", photo.SizeBytes)
	fmt.Printf("  kind      %s\n", photo.Kind)
	fmt.Printf("  path      %s\n", photo.Path)
	fmt.Printf("  shots     %d\n", cam.PhotosTaken())

	// Read it back and decode. This is the check that makes the capture mean
	// something: a file that exists, is fetchable, and is not a JPEG is a
	// success at every layer above this one.
	abs := filepath.Join(*dir, photo.Path)
	body, err := os.ReadFile(abs)
	if err != nil {
		return fmt.Errorf("the photograph is not where it said it was: %w", err)
	}

	img, err := jpeg.Decode(bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("the file is %d bytes and is not a decodable JPEG: %w\n"+
			"  A camera that ignored S_FMT and gave us raw pixels would land here, "+
			"and every layer above would have reported success", len(body), err)
	}

	b := img.Bounds()
	fmt.Printf("\ndecoded JPEG: %dx%d, %T\n", b.Dx(), b.Dy(), img)

	// Sanity: a zero-sized or absurd image decodes without error and is useless.
	if b.Dx() < 16 || b.Dy() < 16 {
		return fmt.Errorf("the JPEG is %dx%d, which is not a usable photograph", b.Dx(), b.Dy())
	}
	ratio := float64(b.Dx()) / float64(b.Dy())
	if ratio < 0.5 || ratio > 2.5 {
		fmt.Printf("\nnote: aspect ratio %.2f is unusual for a photograph.\n"+
			"  If you expected 4:3, the driver may have clamped the resolution.\n", ratio)
	}

	// The requested-versus-actual check, stated as a comparison rather than a
	// guess. This is the one thing on this page the server cannot do for itself,
	// because the server has no way to know what was asked for at the moment the
	// file was written.
	if got, want := fmt.Sprintf("%dx%d", b.Dx(), b.Dy()), params.Resolution; got != want {
		fmt.Printf("\nWARNING: asked for %s and got %s.\n"+
			"  fswebcam's man page says the resolution used may differ if the device\n"+
			"  cannot capture at the requested one, so this is the driver declining,\n"+
			"  not a fault here. The OBC reports the DECODED size for exactly this\n"+
			"  reason: the console shows %s, never %s.\n", want, got, got, want)
	}

	if *linkKbps > 0 {
		fmt.Printf("\nfetching this over a %d kbit/s link takes about %s.\n",
			*linkKbps, fetchTime(photo.SizeBytes, *linkKbps))
	}

	if *out != "" {
		if err := os.WriteFile(*out, body, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", *out, err)
		}
		fmt.Printf("\ncopy written to %s\n", *out)
	}

	fmt.Printf("\nOK: %s is a real JPEG of %dx%d\n", photo.Name, b.Dx(), b.Dy())
	return nil
}

// fetchTime is how long a photograph of this size takes over a rate-limited
// link, as a wall-clock duration rather than a byte count.
//
// The link is the reason a photograph's size matters at all on this project
// (constraint H1), so a bench that reports bytes without reporting the cost
// leaves the operator to do the arithmetic at 14 kB/s.
func fetchTime(size uint64, kbps uint) time.Duration {
	bytesPerSecond := float64(kbps) * 1000 / 8
	secs := float64(size) / bytesPerSecond
	d := time.Duration(secs * float64(time.Second))
	if d < time.Second {
		d = time.Second
	}
	return d.Round(time.Second)
}

// describeParams spells the settings in one line for the bench header, with the
// automatic quality written out rather than as a zero.
func describeParams(p domain.CameraParams) string {
	quality := "auto"
	if p.JPEGQuality != 0 {
		quality = strconv.FormatUint(uint64(p.JPEGQuality), 10)
	}
	return fmt.Sprintf("quality %s, resolution %s, frames %d, skip %d, delay %dms",
		quality, p.Resolution, p.Frames, p.Skip, p.DelayMs)
}

// discardLogger drops the camera's own startup logging.
//
// A bench prints its own report; the settings it prints are the same ones the
// logger would report, and having both is how a bench output ends up showing two
// different resolutions.
type discardLogger struct{}

func (discardLogger) Enabled(context.Context, slog.Level) bool  { return false }
func (discardLogger) Handle(context.Context, slog.Record) error { return nil }
func (d discardLogger) WithAttrs([]slog.Attr) slog.Handler      { return d }
func (d discardLogger) WithGroup(string) slog.Handler           { return d }
