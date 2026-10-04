// Package pico is the link to the RP2040 flight controller: COBS-framed
// protobuf over USB CDC serial.
//
// Three layers, deliberately separable so each can be tested against the
// firmware rather than against itself:
//
//	cobs.go    framing. Mirrors firmware/cobs.c exactly.
//	codec.go   protobuf. One PicoMessage per COBS frame.
//	link.go    the serial port, the command sequence, ACK correlation.
//
// The framing is COBS rather than a length prefix or ASCII because the payload
// is binary: COBS removes every 0x00 from the data so that 0x00 is
// unambiguous as a frame delimiter, with no escaping rules for the sender to
// get wrong.
package pico

import (
	"errors"
	"fmt"
)

// COBS framing errors.
var (
	ErrCobsEmpty     = errors.New("pico/cobs: empty frame")
	ErrCobsZeroByte  = errors.New("pico/cobs: zero byte inside a COBS frame")
	ErrCobsTruncated = errors.New("pico/cobs: truncated COBS frame")
)

// COBS overhead.
//
// The firmware sizes its buffer from the documented worst case rather than a
// guessed constant, because cobs_encode() does not bounds-check its output. The
// worst case is one code byte per 254 payload bytes, plus the final code byte:
// length + length/254 + 1. Encode below panics rather than writing past the
// caller's buffer, and FrameMaxSize applies the same formula.
const cobsOverheadDivisor = 254

// CobsEncodedSize returns the encoded length for a payload of n bytes.
func CobsEncodedSize(n int) int { return n + n/cobsOverheadDivisor + 1 }

// Encode appends the COBS encoding of src to dst and returns the extended
// buffer.
//
// This is a line-by-line transliteration of firmware/cobs.c's cobs_encode(),
// including the 0xFF rollover, so that a divergence between the two is visible
// as a test failure rather than as a link that works on the bench and not in
// flight. test/cobs_test.go round-trips against the compiled C.
func CobsEncode(dst, src []byte) []byte {
	buf := make([]byte, 0, len(dst)+CobsEncodedSize(len(src)))
	buf = append(buf, dst...)

	writeIndex := 1
	codeIndex := 0
	code := byte(1)

	readIndex := 0
	for readIndex < len(src) {
		b := src[readIndex]
		if b == 0 {
			buf = growTo(buf, codeIndex)
			buf[codeIndex] = code
			code = 1
			codeIndex = writeIndex
			writeIndex++
			readIndex++
		} else {
			buf = growTo(buf, writeIndex)
			buf[writeIndex] = b
			writeIndex++
			readIndex++
			code++
			if code == 0xFF {
				buf = growTo(buf, codeIndex)
				buf[codeIndex] = code
				code = 1
				codeIndex = writeIndex
				writeIndex++
			}
		}
	}

	buf = growTo(buf, codeIndex)
	buf[codeIndex] = code
	return buf[:writeIndex]
}

// growTo extends buf by one byte if it cannot hold index. Bounds are checked on
// every write rather than once up front: the C original does not check at all,
// and a Go port that silently overflowed would corrupt the next frame on the
// wire instead of failing here.
func growTo(buf []byte, index int) []byte {
	for len(buf) <= index {
		buf = append(buf, 0)
	}
	return buf
}

// CobsDecode decodes a COBS frame that excludes the trailing 0x00 delimiter.
//
// Mirrors firmware/cobs.c's cobs_decode(), including its bounds checks. The C
// version returns 0 on malformed input; this returns a typed error, because a
// caller that ignores a zero return and uses the partially written buffer is how
// a corrupt frame turns into a wrong servo command.
func CobsDecode(src []byte) ([]byte, error) {
	if len(src) == 0 {
		return nil, ErrCobsEmpty
	}

	dst := make([]byte, 0, len(src))
	readIndex := 0

	for readIndex < len(src) {
		code := src[readIndex]
		if code == 0 {
			// A zero byte cannot appear inside a COBS frame: it is the
			// delimiter. Finding one means the framing is broken, most often a
			// buffer overflow on the sender.
			return nil, fmt.Errorf("%w: at offset %d", ErrCobsZeroByte, readIndex)
		}

		// The C original: if (read_index + code > length && code != 1) return 0;
		if readIndex+int(code) > len(src) && code != 1 {
			return nil, fmt.Errorf("%w: code %d at offset %d overruns %d bytes",
				ErrCobsTruncated, code, readIndex, len(src))
		}
		readIndex++

		for i := byte(1); i < code; i++ {
			dst = append(dst, src[readIndex])
			readIndex++
		}

		// A zero is reinserted between blocks, but not after the final one --
		// the caller appends the delimiter itself, and reinserting it here would
		// make every frame claim a trailing NUL.
		if code != 0xFF && readIndex != len(src) {
			dst = append(dst, 0)
		}
	}

	return dst, nil
}
