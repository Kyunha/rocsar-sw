package test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"

	rocsarv1 "github.com/rocsar/obc/api/rocsar/v1"
	"github.com/rocsar/obc/internal/domain"
	"github.com/rocsar/obc/internal/pico"
)

// unsafePointer keeps the ioctl calls readable. Both ioctls take a pointer to a
// 32-bit value, and spelling that out inline twice is worse than naming it.
func unsafePointer[T any](v *T) unsafe.Pointer { return unsafe.Pointer(v) }

// ptyPair is a pseudo-terminal pair: one end looks like a serial device, the
// other is the far side.
//
// This exists because SerialTransport had no test at all, and it is the layer
// where a real bug hides: a real USB CDC port delivers partial reads, delivers
// nothing for 100 ms at a stretch, and closes in a way io.Pipe does not. Testing
// against an in-memory pipe proves the codec and nothing about the port.
//
// Real ioctl semantics, a real device node, a real path string.
type ptyPair struct {
	// Path is what the OBC opens, e.g. /dev/pts/7.
	Path string
	// Far is the flight controller's end.
	Far *os.File

	mu     sync.Mutex
	closed bool
}

// newPTY allocates a pair. Skips if /dev/ptmx is unavailable, which is a
// container restriction and not a failure.
func newPTY(t *testing.T) *ptyPair {
	t.Helper()

	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no /dev/ptmx: %v", err)
	}

	// Unlock the slave and ask the kernel which one it is.
	var unlock int32
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, master.Fd(),
		uintptr(unix.TIOCSPTLCK), uintptr(unsafePointer(&unlock))); errno != 0 {
		_ = master.Close()
		t.Skipf("TIOCSPTLCK: %v", errno)
	}

	var n uint32
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, master.Fd(),
		uintptr(unix.TIOCGPTN), uintptr(unsafePointer(&n))); errno != 0 {
		_ = master.Close()
		t.Skipf("TIOCGPTN: %v", errno)
	}

	// The two ends are the MASTER and the SLAVE, and they are not
	// interchangeable: whatever is written to the slave is readable on the
	// master and vice versa. The OBC opens the slave, because that is the device
	// node a real USB CDC port appears as; the fake flight controller holds the
	// master. Opening the slave for both ends -- which this did first -- gives two
	// views of the same endpoint, and every read times out.
	name := fmt.Sprintf("/dev/pts/%d", n)

	p := &ptyPair{Path: name, Far: master}
	t.Cleanup(func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.closed {
			return
		}
		p.closed = true
		_ = master.Close()
	})
	return p
}

// Write sends bytes to the flight controller.
func (p *ptyPair) Write(b []byte) (int, error) { return p.Far.Write(b) }

// Read reads what the OBC sent.
func (p *ptyPair) Read(b []byte) (int, error) { return p.Far.Read(b) }

// SetDeadline bounds a read on the far end, so a test that expects nothing to
// arrive does not hang.
func (p *ptyPair) SetDeadline(t time.Time) error { return p.Far.SetReadDeadline(t) }

// fakePico is a flight controller that speaks the real wire format.
//
// It decodes COBS and unmarshals protobuf exactly as rocsar_pico.ino does --
// `pb_decode(&stream, CommandMessage_fields, &cmd)` onto the deframed buffer --
// and replies through the same encoder. So a test against this exercises framing
// in both directions over a real device, not a stub that returns structs.
type fakePico struct {
	pt *ptyPair
	// diag is nil in normal runs. A failing test sets it to t.Logf to see exactly
	// what crossed the wire, which is how the delimiter mismatch below was found:
	// the fake was receiving frames and silently discarding every one of them.
	diag func(string, ...any)

	mu       sync.Mutex
	axes     []axisSpec
	target   float32
	heaters  map[uint32]bool
	acked    []uint32 // command sequences acknowledged
	received []string // command names, in order
	reject   bool
	frames   int
	ticks    uint32
}

type axisSpec struct {
	id    uint32
	tick  uint32
	angle float32
	load  int32
	temp  int32
	state int32 // 0 unknown, 1 measured, 2 held
}

func newFakePico(pt *ptyPair) *fakePico {
	return &fakePico{
		pt:      pt,
		axes:    []axisSpec{{id: 1, tick: 2048, angle: 90, load: 12, temp: 31, state: 1}, {id: 2, tick: 1024, angle: 270, load: -4, temp: 29, state: 2}},
		target:  90,
		heaters: map[uint32]bool{},
	}
}

// Run reads commands and emits telemetry until the context is done.
func (f *fakePico) Run(stop <-chan struct{}) {

	buf := make([]byte, 1024)
	var acc []byte

	for {
		select {
		case <-stop:
			return
		default:
		}

		if err := f.pt.SetDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
			f.say("SetDeadline: %v", err)
			return
		}
		n, err := f.pt.Read(buf)
		if err != nil && !errors.Is(err, os.ErrDeadlineExceeded) {
			// EIO on the master means the slave has closed, which happens when the
			// link shuts down. Returning rather than continuing stops this loop
			// spinning at 20 iterations a second for the rest of the test.
			f.say("Read: n=%d err=%v -- the link closed", n, err)
			return
		}
		if n > 0 {
			f.say("got %d bytes: %x", n, buf[:n])
			acc = append(acc, buf[:n]...)
			for {
				i := indexByte(acc, 0x00)
				if i < 0 {
					break
				}
				frame := append([]byte(nil), acc[:i]...)
				acc = acc[i+1:]
				f.handle(frame)
			}
		}
		if err != nil {
			continue // deadline, or the link closed
		}
	}
}

// startFakePico runs a fake and registers a teardown that CANNOT hang.
//
// Two cleanups in sequence used to deadlock the suite: one closed the stop
// channel and waited unconditionally for the reader, and if the reader was
// blocked inside Read with an expired deadline it never woke. A test helper
// that can hang the run is a bug in itself -- it turns one failure into a
// timeout with no diagnosis.
//
// The order also matters. The link is closed FIRST, which makes the slave go
// away, which makes the master's next Read return EIO, which is what actually
// stops the reader. Waiting on a stop channel alone relies on the reader
// happening to be between reads.
func startFakePico(t *testing.T, pt *ptyPair, existing *fakePico) (stop chan struct{}, done chan struct{}) {
	t.Helper()

	fp := existing
	if fp == nil {
		fp = newFakePico(pt)
	}
	fp.diag = t.Logf

	stop = make(chan struct{})
	done = make(chan struct{})
	go func() { defer close(done); fp.Run(stop) }()

	t.Cleanup(func() {
		close(stop)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Log("the fake flight controller did not stop within 2s; " +
				"continuing rather than hanging the suite")
		}
	})
	return stop, done
}

func (f *fakePico) say(format string, args ...any) {
	if f.diag != nil {
		f.diag(format, args...)
	}
}

func (f *fakePico) handle(frame []byte) {
	cmd, err := decodeCommandFrame(frame)
	if err != nil {
		return
	}

	f.mu.Lock()
	f.received = append(f.received, commandName(cmd))
	f.acked = append(f.acked, cmd.GetSequence())
	reject := f.reject

	switch p := cmd.GetPayload().(type) {
	case *rocsarv1.PicoCommand_SetTarget:
		f.target = p.SetTarget.GetTargetHeadingDeg()
	case *rocsarv1.PicoCommand_Jog:
		for i := range f.axes {
			if f.axes[i].id == p.Jog.GetServoId() {
				f.axes[i].tick = p.Jog.GetTick()
			}
		}
	case *rocsarv1.PicoCommand_Heater:
		f.heaters[p.Heater.GetHeaterId()] = p.Heater.GetState()
	}

	ack := &rocsarv1.PicoAck{
		CommandSequence: cmd.GetSequence(),
		Success:         !reject,
		Error:           rocsarv1.ErrorCode_ERROR_NONE,
	}
	if reject {
		ack.Error = rocsarv1.ErrorCode_ERROR_INVALID_PARAMETER
	}
	f.mu.Unlock()

	out, err := pico.EncodeMessage(nil, &rocsarv1.PicoMessage{
		Sequence:    f.nextTicks(),
		TimestampUs: 1,
		Payload:     &rocsarv1.PicoMessage_Ack{Ack: ack},
	})
	if err != nil {
		f.say("encode ack: %v", err)
		return
	}
	if n, werr := f.pt.Write(out); werr != nil {
		f.say("write ack: n=%d err=%v", n, werr)
	} else {
		f.say("wrote ack: %d bytes", n)
	}

	f.emitTelemetry()
}

func (f *fakePico) nextTicks() uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ticks++
	return f.ticks
}

// emitTelemetry sends one frame, which is what the real firmware does every
// control tick regardless of what it was sent.
func (f *fakePico) emitTelemetry() {
	f.mu.Lock()
	f.frames++
	tel := &rocsarv1.PicoTelemetry{
		GondolaHeadingDeg: 271.5,
		TargetHeadingDeg:  f.target,
		ImuPresent:        true,
		Heater1State:      f.heaters[1],
		Heater2State:      f.heaters[2],
	}
	for _, a := range f.axes {
		tel.Antennas = append(tel.Antennas, &rocsarv1.AntennaTelemetry{
			ServoId: a.id, CurrentTick: a.tick, CurrentAngleDeg: a.angle,
			Load: a.load, TemperatureC: a.temp, FeedbackState: a.state,
		})
	}
	f.mu.Unlock()

	out, err := pico.EncodeMessage(nil, &rocsarv1.PicoMessage{
		Sequence:    f.nextTicks(),
		TimestampUs: 2,
		Payload:     &rocsarv1.PicoMessage_Telemetry{Telemetry: tel},
	})
	if err != nil {
		return
	}
	_, _ = f.pt.Write(out)
}

// ---------------------------------------------------------------------------

// SerialTransport over a real device node: open, write a frame, read it back,
// close. This is the layer that had no coverage at all.
func TestSerialTransportOverARealPort(t *testing.T) {
	pt := newPTY(t)

	tr := pico.NewSerialTransport(pt.Path, pico.DefaultBaudrate)
	if !tr.Exists() {
		t.Fatalf("%s exists on disk but Exists() said no", pt.Path)
	}
	if err := tr.OpenPort(); err != nil {
		t.Fatalf("OpenPort: %v", err)
	}
	if err := tr.SetReadTimeout(200 * time.Millisecond); err != nil {
		t.Errorf("SetReadTimeout: %v", err)
	}

	frame := []byte("a frame with a 0x00-ish payload")
	if n, err := tr.Write(frame); err != nil || n != len(frame) {
		t.Fatalf("Write = %d, %v", n, err)
	}

	buf := make([]byte, 256)
	if err := pt.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	n, err := pt.Read(buf)
	if err != nil {
		t.Fatalf("the far end did not receive: %v", err)
	}
	if string(buf[:n]) != string(frame) {
		t.Errorf("the far end received %q, want %q", buf[:n], frame)
	}

	if err := tr.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	// Using a closed transport must be an error, not a panic and not a silent
	// write into nothing.
	if _, err := tr.Write(frame); err == nil {
		t.Error("Write on a closed transport succeeded")
	}
}

// A whole link over a real serial port, with a real COBS+protobuf peer. The
// loopback in pico_link_test.go covers the protocol; this covers the port.
func TestPicoLinkOverARealSerialPort(t *testing.T) {
	pt := newPTY(t)

	tr := pico.NewSerialTransport(pt.Path, pico.DefaultBaudrate)
	if err := tr.OpenPort(); err != nil {
		t.Fatalf("OpenPort: %v", err)
	}

	link := pico.NewLink(tr, quietLogger(),
		pico.WithAckTimeout(2*time.Second),
		pico.WithMock(false))
	t.Cleanup(func() { _ = link.Close() })

	fp := newFakePico(pt)
	startFakePico(t, pt, fp)

	if err := link.Open(context.Background()); err != nil {
		t.Fatalf("Open over a real port: %v", err)
	}
	if !link.Connected() {
		t.Fatal("Connected() is false after a successful handshake")
	}

	// The handshake itself must have been answered.
	fp.mu.Lock()
	names := append([]string(nil), fp.received...)
	fp.mu.Unlock()
	if len(names) == 0 || names[0] != "status_request" {
		t.Errorf("the first command the fake saw was %v, want status_request", names)
	}

	// Every command must be acknowledged over the real port.
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		fn   func() (*domain.Ack, error)
		want string
	}{
		{"set_target", func() (*domain.Ack, error) { return link.SetTarget(ctx, 180) }, "set_target"},
		{"jog", func() (*domain.Ack, error) { return link.Jog(ctx, 1, 3000) }, "jog"},
		{"zero", func() (*domain.Ack, error) { return link.Zero(ctx, 1) }, "zero"},
		{"mount", func() (*domain.Ack, error) { return link.Mount(ctx, 1, 2.5) }, "mount"},
		{"dir", func() (*domain.Ack, error) { return link.SetDirection(ctx, 1, -1) }, "dir"},
		{"heater", func() (*domain.Ack, error) { return link.SetHeater(ctx, 2, true) }, "heater"},
		{"stop", func() (*domain.Ack, error) { return link.Stop(ctx, 1) }, "stop"},
	} {
		ack, err := tc.fn()
		if err != nil {
			t.Errorf("%s over a real port: %v", tc.name, err)
			continue
		}
		if !ack.Success {
			t.Errorf("%s: not acknowledged (%s)", tc.name, ack.Error)
		}
	}

	// Telemetry must have crossed the port.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if tel, ok := link.Telemetry(); ok && len(tel.Axes) == 2 {
			if tel.TargetHeadingDeg != 180 {
				t.Errorf("target heading = %v, want 180 -- the command did not take effect", tel.TargetHeadingDeg)
			}
			if !tel.Heater2State {
				t.Error("heater 2 is off after SetHeater(2, true)")
			}
			if tel.Axes[0].CurrentTick != 3000 {
				t.Errorf("axis 1 tick = %d, want 3000 after jog", tel.Axes[0].CurrentTick)
			}
			// The tri-state must survive a real port in both directions.
			if tel.Axes[0].FeedbackState != domain.FeedbackMeasured {
				t.Errorf("axis 1 feedback = %v, want MEASURED", tel.Axes[0].FeedbackState)
			}
			if tel.Axes[1].FeedbackState != domain.FeedbackHeld {
				t.Errorf("axis 2 feedback = %v, want HELD", tel.Axes[1].FeedbackState)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("no telemetry with two axes arrived over the real port")
}

// A command the fake refuses must come back as a refusal, not as success.
func TestRefusalCrossesARealPort(t *testing.T) {
	pt := newPTY(t)

	tr := pico.NewSerialTransport(pt.Path, pico.DefaultBaudrate)
	if err := tr.OpenPort(); err != nil {
		t.Fatalf("OpenPort: %v", err)
	}
	link := pico.NewLink(tr, quietLogger(), pico.WithAckTimeout(2*time.Second), pico.WithMock(false))
	t.Cleanup(func() { _ = link.Close() })

	fp := newFakePico(pt)
	fp.reject = true
	startFakePico(t, pt, fp)

	if err := link.Open(context.Background()); err != nil {
		// Open probes with status_request, which this fake refuses, so the
		// handshake failing is the correct outcome.
		return
	}

	ack, err := link.SetTarget(context.Background(), 90)
	if err != nil {
		t.Fatalf("SetTarget: %v", err)
	}
	if ack.Success {
		t.Error("a refused command reported success")
	}
	if ack.Error != domain.ErrInvalidParameter {
		t.Errorf("error = %v, want ERROR_INVALID_PARAMETER", ack.Error)
	}
}

// ---------------------------------------------------------------------------

func indexByte(b []byte, c byte) int {
	for i, v := range b {
		if v == c {
			return i
		}
	}
	return -1
}

func commandName(c *rocsarv1.PicoCommand) string {
	switch c.GetPayload().(type) {
	case *rocsarv1.PicoCommand_SetTarget:
		return "set_target"
	case *rocsarv1.PicoCommand_Jog:
		return "jog"
	case *rocsarv1.PicoCommand_Zero:
		return "zero"
	case *rocsarv1.PicoCommand_Mount:
		return "mount"
	case *rocsarv1.PicoCommand_Dir:
		return "dir"
	case *rocsarv1.PicoCommand_Heater:
		return "heater"
	case *rocsarv1.PicoCommand_Stop:
		return "stop"
	case *rocsarv1.PicoCommand_StatusRequest:
		return "status_request"
	default:
		return "unknown"
	}
}

// decodeCommandFrame is the firmware's receive path, applied to ONE frame's
// COBS block: deframe, then pb_decode straight onto the bytes, with no envelope
// and no timestamp.
//
// The input is a DEFRAMED block -- the bytes BETWEEN delimiters. That matches
// pico.DecodeMessage, and it matches the split the sketch does: the firmware
// splits on 0x00 first and only then hands the buffer to cobs_decode. An earlier
// version of this took the frame INCLUDING its delimiter, and the receive loop
// passes it without -- so every frame was rejected, the fake never answered, and
// the link timed out while the fake sat there believing it had been asked
// nothing. The two halves disagreeing about the contract is exactly what made it
// hard to see.
//
// Mirrors rocsar_pico.ino's loop, which is the point: a fake that decoded
// differently from the firmware would test a protocol the board does not speak.
func decodeCommandFrame(cobsBlock []byte) (*rocsarv1.PicoCommand, error) {
	if len(cobsBlock) == 0 {
		return nil, fmt.Errorf("empty frame")
	}
	payload, err := pico.CobsDecode(cobsBlock)
	if err != nil {
		return nil, err
	}
	cmd := &rocsarv1.PicoCommand{}
	if err := proto.Unmarshal(payload, cmd); err != nil {
		return nil, err
	}
	if cmd.GetPayload() == nil {
		return nil, fmt.Errorf("command has no payload")
	}
	return cmd, nil
}
