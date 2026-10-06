package pico

import (
	"errors"
	"fmt"
	"io"
	"time"
)

// Transport is the byte pipe under the link.
//
// It exists so the link can be driven by a buffer in a test and by a real serial
// port in the field, with the framing and correlation logic identical in both.
// The bug this shape prevents is the one that always happens: the framing gets
// tested against a mock that behaves nothing like a serial port, and the real
// port then delivers partial reads and overruns.
//
// The two behaviours a real serial port has and a naive mock does not:
//
//   - Read returns (0, nil) on timeout. That is "nothing arrived", NOT "the
//     link is down". Treating it as an error makes the link flap once per
//     timeout period on a healthy but idle device.
//   - Close unblocks a blocked Read. That is the only way to stop the receive
//     goroutine, since Read has no deadline.
type Transport interface {
	io.ReadWriteCloser

	// SetReadTimeout bounds one read. The receive loop needs it so a silent
	// device cannot block the goroutine forever and shutdown stays responsive.
	SetReadTimeout(d time.Duration) error
	// DrainRead discards buffered input, so a reply to a command that timed out
	// cannot be delivered as the acknowledgement to the next one.
	DrainRead() error
}

// ReadTimeout is how long one read waits before returning empty.
//
// Short, because its only job is to let the loop re-check whether it should
// still be running. A long timeout makes shutdown take as long as the timeout.
const ReadTimeout = 100 * time.Millisecond

// ReadChunk is the receive buffer size.
//
// Larger than one frame so a frame split across two reads is the normal case
// rather than an edge case. A serial port delivers whatever has arrived when it
// is asked; assuming one read equals one frame is the assumption that breaks
// under load.
const ReadChunk = 1024

// readLoop accumulates bytes until the 0x00 delimiter, then hands the frame to
// onFrame.
//
// The accumulator is per-connection and is reset on every delimiter, including
// on error. A half-received frame followed by a reconnect must not leave stale
// bytes in the buffer for the next connection to prepend to its first frame.
//
// Overflow behaviour: a frame longer than MaxFrame is abandoned and the
// accumulator resynchronised at the next delimiter. The alternative -- keeping
// growing -- turns one desynchronised sender into an unbounded allocation.
type frameReader struct {
	buf      []byte
	overflow bool
}

func newFrameReader() *frameReader {
	return &frameReader{buf: make([]byte, 0, MaxFrame)}
}

// feed consumes a read chunk and returns every complete frame it completes.
func (r *frameReader) feed(chunk []byte) [][]byte {
	var frames [][]byte

	for _, b := range chunk {
		if b != Delimiter {
			if r.overflow {
				// Resynchronising: drop everything until the next delimiter.
				continue
			}
			if len(r.buf) >= MaxFrame {
				r.overflow = true
				r.buf = r.buf[:0]
				continue
			}
			r.buf = append(r.buf, b)
			continue
		}

		// Delimiter.
		if r.overflow {
			// End of the oversized frame. Resynchronised.
			r.overflow = false
			r.buf = r.buf[:0]
			continue
		}
		if len(r.buf) == 0 {
			// Two delimiters in a row: an empty frame. The firmware emits one
			// as a keepalive-adjacent artefact of its receive loop resetting, and
			// it is not an error.
			continue
		}

		frame := make([]byte, len(r.buf))
		copy(frame, r.buf)
		r.buf = r.buf[:0]
		frames = append(frames, frame)
	}

	return frames
}

// reset drops any partial frame. Called on reconnect so one connection's
// leftovers cannot corrupt the next one's first frame.
func (r *frameReader) reset() {
	r.buf = r.buf[:0]
	r.overflow = false
}

// ErrNotConnected is returned by a command issued on a closed link.
//
// It is deliberately distinct from a timeout. "The link is down" and "the link
// is up and the device did not answer in time" are different faults and the
// operator needs to be able to tell them apart: one is a cable, the other is a
// wedged flight controller.
var ErrNotConnected = errors.New("pico: link is not connected")

// ErrAckTimeout is returned when a command is not acknowledged in time.
var ErrAckTimeout = errors.New("pico: command was not acknowledged")

// DefaultAckTimeout is how long to wait for an ACK.
//
// The flight controller answers inside its 20 ms control loop, so a healthy
// round trip is single-digit milliseconds. Half a second is generous enough to
// absorb a scheduling delay on a busy Pi and short enough that a wedged device
// is noticed while the operator is still watching.
//
// `zero` is the exception that sets the floor, and it is worth knowing about
// before raising or lowering this. Teaching a servo its centre is the only
// command that writes to hardware: it reads the position, writes the ST3215's
// EEPROM with the vendor's 20 ms settle delay either side of each write, then
// reads the register and the position back to prove the write took. That is
// 100-250 ms of firmware time during which the control loop does not tick, and
// the acknowledgement arrives at the end of it. The margin here is therefore
// only about 2-5x, not the 50x every other command enjoys -- if a teach ever
// grows (a second read-back, a retry), this is the constant that will start
// firing, and the symptom will be a zero that succeeded and reported a timeout.
const DefaultAckTimeout = 500 * time.Millisecond

// frameTooLarge reports whether a frame exceeded the budget, for a clearer
// message than a generic parse failure.
func frameTooLarge(n int) error {
	return fmt.Errorf("pico: frame of %d bytes exceeds the %d byte budget", n, MaxFrame)
}
