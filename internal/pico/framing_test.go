package pico

import "testing"

// A frame longer than the budget must be abandoned and the stream
// resynchronised, not accumulated without bound. One desynchronised sender must
// not become an unbounded allocation, and the link has to heal afterwards
// rather than staying broken for the life of the process.
//
// This lives in-package because frameReader is deliberately unexported: it is an
// implementation detail of the receive loop, and exporting it for a test would
// make it part of the package's API.
func TestOversizedFrameIsDiscardedAndStreamRecovers(t *testing.T) {
	r := newFrameReader()

	// A run of non-delimiter bytes with no delimiter in sight: exactly what a
	// desynchronised sender looks like.
	chunk := make([]byte, 4096)
	for i := range chunk {
		chunk[i] = 0xAA
	}
	for i := 0; i < 500; i++ {
		if frames := r.feed(chunk); len(frames) != 0 {
			t.Fatalf("iteration %d produced a frame from a run with no delimiter", i)
		}
	}

	// Recovery happens at the next delimiter, and that is the only place it can.
	//
	// A receiver that splits on 0x00 cannot resynchronise from the payload
	// alone: given a run of bytes with no delimiter, there is no way to know
	// whether the next byte begins a new frame or is the tail of the current
	// one. The firmware always terminates a frame with 0x00, so the delimiter
	// is a real resynchronisation point, and waiting for it is correct rather
	// than merely convenient.
	if frames := r.feed([]byte{byte(Delimiter)}); len(frames) != 0 {
		t.Fatalf("the oversized frame produced %d frames; it must produce none", len(frames))
	}

	good := append(CobsEncode(nil, []byte("hello")), byte(Delimiter))
	frames := r.feed(good)
	if len(frames) != 1 {
		t.Fatalf("after resynchronising, a valid frame yielded %d frames", len(frames))
	}
	out, err := CobsDecode(frames[0])
	if err != nil || string(out) != "hello" {
		t.Fatalf("recovered frame = %q (%v), want \"hello\"", out, err)
	}
}

// The accumulator must not grow without bound. Fed an endless delimiter-less
// run it stays capped at the frame budget, which is the property that stops one
// bad sender from becoming an out-of-memory condition.
func TestAccumulatorStaysBounded(t *testing.T) {
	r := newFrameReader()
	chunk := make([]byte, 64*1024)
	for i := range chunk {
		chunk[i] = 0xAA
	}
	for i := 0; i < 1000; i++ {
		r.feed(chunk)
	}
	if len(r.buf) > MaxFrame {
		t.Fatalf("accumulator grew to %d bytes, over the %d byte budget", len(r.buf), MaxFrame)
	}
}

// Two delimiters in a row is an empty frame, not an error. The firmware's
// receive loop resets and produces one.
func TestEmptyFrameIsSkippedNotAnError(t *testing.T) {
	r := newFrameReader()
	frame := append(CobsEncode(nil, []byte("x")), Delimiter)
	got := r.feed(append(append([]byte{}, frame...), byte(Delimiter)))
	if len(got) != 1 {
		t.Fatalf("got %d frames, want 1", len(got))
	}
}

// A frame split across many feeds must come out whole.
func TestFrameReassembledAcrossFeeds(t *testing.T) {
	r := newFrameReader()
	frame := append(CobsEncode(nil, []byte("a longer payload to split")), Delimiter)

	var out [][]byte
	for i := 0; i < len(frame); i++ {
		out = append(out, r.feed(frame[i:i+1])...)
	}
	if len(out) != 1 {
		t.Fatalf("byte-at-a-time delivery produced %d frames, want 1", len(out))
	}
	payload, err := CobsDecode(out[0])
	if err != nil || string(payload) != "a longer payload to split" {
		t.Fatalf("reassembled %q (%v)", payload, err)
	}
}

// reset() must drop a partial frame, so one connection's leftovers cannot be
// prepended to the next connection's first frame after a reconnect.
func TestResetDropsPartialFrame(t *testing.T) {
	r := newFrameReader()
	r.feed([]byte{0x01, 0x02, 0x03}) // partial, no delimiter
	r.reset()

	good := append(CobsEncode(nil, []byte("fresh")), Delimiter)
	frames := r.feed(good)
	if len(frames) != 1 {
		t.Fatalf("got %d frames after reset, want 1", len(frames))
	}
	payload, _ := CobsDecode(frames[0])
	if string(payload) != "fresh" {
		t.Fatalf("payload = %q, want \"fresh\"; stale bytes survived the reset", payload)
	}
}
