// Command camera_bench takes one photograph and checks it.
//
//	camera_bench                       snapshot and report
//	camera_bench -device /dev/video1   against another node
//	camera_bench -out /tmp/shot.jpg    keep a copy
//
// It composes internal/camera, so it captures exactly as the server does. What
// it adds is the part the server cannot do: it DECODES the JPEG it just wrote
// and checks the dimensions are usable.
//
// That check is the point. A camera can silently give you a different resolution
// than the one asked for, so a snapshot can succeed, be written, be fetchable
// over HTTP, and be the wrong picture -- and every layer above reports success.
// Asserting that what came back is a real JPEG of a usable size is what catches it.
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
	"os"
	"path/filepath"

	"github.com/rocsar/obc/internal/camera"
	"github.com/rocsar/obc/internal/storage"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "camera_bench: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		device = flag.String("device", "/dev/video0", "video device node")
		dir    = flag.String("dir", "/mnt/rocsar/data", "data directory")
		sub    = flag.String("sub", "photos", "subdirectory within the data directory")
		out    = flag.String("out", "", "also copy the JPEG here")
	)
	flag.Parse()

	if _, err := os.Stat(*device); err != nil {
		return fmt.Errorf("no camera at %s", *device)
	}

	store := storage.New(*dir)
	if err := store.Check(); err != nil {
		return fmt.Errorf("data directory: %w", err)
	}

	cam := camera.NewCapture(*device, *sub, store, 85)

	fmt.Printf("device %s\n", *device)
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

	if *out != "" {
		if err := os.WriteFile(*out, body, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", *out, err)
		}
		fmt.Printf("\ncopy written to %s\n", *out)
	}

	fmt.Printf("\nOK: %s is a real JPEG of %dx%d\n", photo.Name, b.Dx(), b.Dy())
	return nil
}
