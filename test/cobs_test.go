package test

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/rocsar/obc/internal/pico"
)

// The COBS implementation in Go is a transliteration of firmware/cobs.c. A
// transliteration that is only ever tested against itself proves it is
// self-consistent, which is not the same as being correct. These tests compile
// the actual C and compare byte for byte.

// newCobsOracle builds and runs a small C harness around the firmware's own
// cobs_encode, once per test run.
//
// cobs.c and cobs.h are COPIED into the temp dir rather than reached with -I.
// That is not tidiness: the gcc on this machine is a Nix wrapper that silently
// drops -I (verified -- `-I<dir>` and `-I <dir>` both fail to resolve a header
// that exists, and CPATH works). Compiling from a directory that contains both
// the harness and the sources needs no include mechanism at all, so this test
// cannot be broken by a toolchain quirk in a way that looks like a COBS bug.
//
// It links firmware/cobs.c, the same translation unit the Arduino build
// compiles, so a divergence between Go and C shows up here.
type cobsOracle struct {
	bin string
	dir string
}

func newCobsOracle(t *testing.T) *cobsOracle {
	t.Helper()

	if _, err := exec.LookPath("gcc"); err != nil {
		t.Skip("gcc not available; cannot check the Go COBS against the firmware's C")
	}

	srcDir := firmwareDir(t)
	dir := t.TempDir()

	for _, name := range []string{"cobs.c", "cobs.h"} {
		body, err := os.ReadFile(filepath.Join(srcDir, name))
		if err != nil {
			t.Fatalf("reading firmware/%s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	program := `
#include <stdio.h>
#include <stdint.h>
#include <string.h>
#include <stdlib.h>
#include "cobs.h"

int main(int argc, char **argv) {
    if (argc < 2) return 2;
    size_t n = strlen(argv[1]) / 2;
    uint8_t in[8192], out[16384];
    for (size_t i = 0; i < n; i++) {
        unsigned v; sscanf(argv[1] + 2*i, "%2x", &v); in[i] = (uint8_t)v;
    }
    size_t len = cobs_encode(in, n, out);
    for (size_t i = 0; i < len; i++) printf("%02x", out[i]);
    return 0;
}
`
	if err := os.WriteFile(filepath.Join(dir, "harness.c"), []byte(program), 0o644); err != nil {
		t.Fatal(err)
	}

	exe := filepath.Join(dir, "harness")
	cmd := exec.Command("gcc", "-O1", "-o", exe,
		filepath.Join(dir, "harness.c"), filepath.Join(dir, "cobs.c"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("compiling the COBS harness: %v\n%s", err, out)
	}

	return &cobsOracle{bin: exe, dir: dir}
}

func firmwareDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.Abs(filepath.Join("..", "firmware"))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func (o *cobsOracle) encode(t *testing.T, payload []byte) []byte {
	t.Helper()
	cmd := exec.Command(o.bin, fmt.Sprintf("%x", payload))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("cobs_encode failed: %v", err)
	}
	var got []byte
	for i := 0; i+1 < len(out); i += 2 {
		var v byte
		fmt.Sscanf(string(out[i:i+2]), "%02x", &v)
		got = append(got, v)
	}
	return got
}

// The Go encoder must produce exactly what the firmware's C produces. Any
// divergence is a link that works one way and not the other.
func TestCobsEncodeMatchesFirmwareC(t *testing.T) {
	o := newCobsOracle(t)

	cases := [][]byte{
		{},
		{0x00},
		{0x01},
		{0xFF},
		{0x00, 0x00, 0x00},
		{0x11, 0x22, 0x33},
		[]byte("hello"),
		bytes.Repeat([]byte{0x00}, 300), // forces the 0xFF rollover repeatedly
		bytes.Repeat([]byte{0xAB}, 254), // exactly one block, no rollover
		bytes.Repeat([]byte{0xAB}, 255), // one past the rollover boundary
		bytes.Repeat([]byte{0xAB}, 508),
		append(bytes.Repeat([]byte{0xAB}, 253), 0x00), // zero at a block edge
	}

	for _, c := range cases {
		want := o.encode(t, c)
		got := pico.CobsEncode(nil, c)
		if !bytes.Equal(got, want) {
			t.Errorf("CobsEncode(%d bytes):\n got %x\nwant %x\n(the C implementation is authoritative)",
				len(c), got, want)
		}
	}
}

// Randomised differential test. Edge cases listed by hand get missed; this
// covers the space between them.
func TestCobsEncodeMatchesFirmwareCRandomised(t *testing.T) {
	o := newCobsOracle(t)
	rng := rand.New(rand.NewSource(20261003)) // fixed seed: a failure is reproducible

	for i := 0; i < 300; i++ {
		n := rng.Intn(600)
		payload := make([]byte, n)
		for j := range payload {
			// Bias toward zeros, which is what makes COBS interesting.
			if rng.Intn(3) == 0 {
				payload[j] = 0
			} else {
				payload[j] = byte(rng.Intn(256))
			}
		}
		want := o.encode(t, payload)
		got := pico.CobsEncode(nil, payload)
		if !bytes.Equal(got, want) {
			t.Fatalf("iteration %d, %d bytes:\n got %x\nwant %x", i, n, got, want)
		}
	}
}

// Round trip, including the shapes that break naive implementations.
func TestCobsRoundTrip(t *testing.T) {
	cases := [][]byte{
		{},
		{0x00},
		{0x00, 0x00},
		{0xFF, 0xFF, 0xFF},
		[]byte("the quick brown fox"),
		bytes.Repeat([]byte{0x00}, 1000),
		bytes.Repeat([]byte{0x41}, 1000),
	}

	for i := 0; i < 200; i++ {
		rng := rand.New(rand.NewSource(int64(i)))
		n := rng.Intn(2000)
		p := make([]byte, n)
		for j := range p {
			p[j] = byte(rng.Intn(256))
		}
		cases = append(cases, p)
	}

	for _, orig := range cases {
		encoded := pico.CobsEncode(nil, orig)
		// A COBS frame must contain no zero bytes at all. That invariant is the
		// entire reason the framing exists.
		if bytes.IndexByte(encoded, 0x00) >= 0 {
			t.Fatalf("encoded frame contains a zero byte: %x", encoded)
		}
		back, err := pico.CobsDecode(encoded)
		if err != nil {
			t.Fatalf("decode of %d bytes: %v", len(orig), err)
		}
		if !bytes.Equal(back, orig) {
			t.Fatalf("round trip changed %d bytes:\n got %x\nwant %x", len(orig), back, orig)
		}
	}
}

func TestCobsSizeFormula(t *testing.T) {
	for _, n := range []int{0, 1, 253, 254, 255, 508, 509, 1000} {
		if got := len(pico.CobsEncode(nil, bytes.Repeat([]byte{0x01}, n))); got > pico.CobsEncodedSize(n) {
			t.Errorf("payload %d encoded to %d, over the stated worst case %d",
				n, got, pico.CobsEncodedSize(n))
		}
	}
}

// Malformed input must be rejected, not decoded into something plausible. The C
// original returns 0; a Go caller that ignored a zero-length decode and used the
// partial buffer would issue a servo command from corrupted bytes.
func TestCobsDecodeRejectsMalformed(t *testing.T) {
	if _, err := pico.CobsDecode(nil); err == nil {
		t.Error("empty input accepted")
	}
	// A zero byte cannot be inside a frame.
	if _, err := pico.CobsDecode([]byte{0x02, 0x11, 0x00, 0x33}); err == nil {
		t.Error("a zero byte inside the frame was accepted")
	}
	// A code byte claiming more data than remains.
	if _, err := pico.CobsDecode([]byte{0x05, 0x11}); err == nil {
		t.Error("a truncated frame was accepted")
	}
}
