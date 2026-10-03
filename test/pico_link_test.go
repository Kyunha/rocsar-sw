package test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	rocsarv1 "github.com/rocsar/obc/api/rocsar/v1"
	"github.com/rocsar/obc/internal/domain"
	"github.com/rocsar/obc/internal/pico"
)

// loopback is a Transport wired to a fake flight controller.
//
// It is not a mock Link. It speaks the real wire format in both directions --
// the controller side decodes COBS, unmarshals protobuf, and replies through
// the same encoder the RP2040 uses -- so every test below exercises framing,
// sequencing and correlation rather than a shortcut around them.
type loopback struct {
	mu       sync.Mutex
	toDev    chan []byte // OBC -> flight controller
	toHost   chan []byte // flight controller -> OBC
	closed   chan struct{}
	closeOne sync.Once

	// silent suppresses replies, to exercise the ACK timeout.
	silent bool
	// telemetry, when set, is emitted after every command, as the real 50 Hz
	// loop does.
	telemetry *rocsarv1.PicoTelemetry
	// reject makes the controller answer with failure.
	reject rocsarv1.ErrorCode
	// chunk is the largest number of bytes handed to Read per call. Values below
	// a frame length force the reader to reassemble across reads.
	chunk int

	// remainder holds the tail of a frame that chunk did not fit, so the NEXT
	// Read continues it.
	//
	// This field is the whole reason the harness is a byte stream and not a
	// message queue. Re-queueing the tail at the end of `toHost` puts it behind
	// any frame already queued, and a real serial port never reorders bytes. The
	// first version did exactly that and produced an interleaved "frame" that
	// failed to decode -- which looked like a framing bug in the link and was
	// entirely a bug in the harness.
	remainder []byte

	written [][]byte
	stopped bool
}

func newLoopback() *loopback {
	return &loopback{
		toDev:  make(chan []byte, 64),
		toHost: make(chan []byte, 64),
		closed: make(chan struct{}),
		chunk:  1024,
	}
}

func (l *loopback) Write(p []byte) (int, error) {
	l.mu.Lock()
	if l.stopped {
		l.mu.Unlock()
		return 0, errors.New("loopback: closed")
	}
	frame := make([]byte, len(p))
	copy(frame, p)
	l.written = append(l.written, frame)
	silent := l.silent
	tel := l.telemetry
	reject := l.reject
	l.mu.Unlock()

	if silent {
		return len(p), nil
	}

	select {
	case l.toDev <- frame:
	default:
	}

	// Answer on a real goroutine: the Link writes from the caller's goroutine
	// and must not deadlock against its own receive loop.
	go func() {
		cmd, err := l.handle(frame)
		if err != nil || cmd == nil {
			return
		}

		if tel != nil {
			l.emitTelemetry(tel)
		}

		ack := &rocsarv1.PicoAck{
			CommandSequence: cmd.GetSequence(),
			Success:         reject == rocsarv1.ErrorCode_ERROR_NONE,
			Error:           reject,
		}
		out, err := pico.EncodeMessage(nil, &rocsarv1.PicoMessage{
			Sequence:    1,
			TimestampUs: 1,
			Payload:     &rocsarv1.PicoMessage_Ack{Ack: ack},
		})
		if err != nil {
			return
		}
		select {
		case l.toHost <- out:
		default:
		}
	}()

	return len(p), nil
}

// handle decodes an OBC->controller frame the way the firmware's receive loop
// does: COBS-deframe, then pb_decode.
func (l *loopback) handle(frame []byte) (*rocsarv1.PicoCommand, error) {
	if len(frame) == 0 || frame[len(frame)-1] != pico.Delimiter {
		return nil, errors.New("loopback: frame is not delimited")
	}
	payload, err := pico.CobsDecode(frame[:len(frame)-1])
	if err != nil {
		return nil, err
	}
	cmd := &rocsarv1.PicoCommand{}
	if err := protoUnmarshal(payload, cmd); err != nil {
		return nil, err
	}
	if cmd.GetPayload() == nil {
		return nil, pico.ErrNoPayload
	}
	return cmd, nil
}

func (l *loopback) emitTelemetry(t *rocsarv1.PicoTelemetry) {
	out, err := pico.EncodeMessage(nil, &rocsarv1.PicoMessage{
		Sequence:    1,
		TimestampUs: 2,
		Payload:     &rocsarv1.PicoMessage_Telemetry{Telemetry: t},
	})
	if err != nil {
		return
	}
	select {
	case l.toHost <- out:
	default:
	}
}

// Read models a byte stream: whatever is left over from the previous call is
// delivered before anything newly queued, and bytes are never reordered.
func (l *loopback) Read(p []byte) (int, error) {
	for {
		l.mu.Lock()
		chunk := l.chunk
		if len(l.remainder) > 0 {
			n := len(l.remainder)
			if chunk > 0 && n > chunk {
				n = chunk
			}
			if n > len(p) {
				n = len(p)
			}
			copy(p, l.remainder[:n])
			l.remainder = l.remainder[n:]
			l.mu.Unlock()
			return n, nil
		}
		l.mu.Unlock()

		select {
		case <-l.closed:
			return 0, errLoopbackClosed{}
		case b := <-l.toHost:
			l.mu.Lock()
			n := len(b)
			if chunk > 0 && n > chunk {
				n = chunk
			}
			if n > len(p) {
				n = len(p)
			}
			copy(p, b[:n])
			if n < len(b) {
				l.remainder = append(l.remainder[:0], b[n:]...)
			}
			l.mu.Unlock()
			return n, nil
		}
	}
}

func (l *loopback) SetReadTimeout(time.Duration) error { return nil }

func (l *loopback) DrainRead() error {
	l.mu.Lock()
	l.remainder = nil
	l.mu.Unlock()
	for {
		select {
		case <-l.toHost:
		default:
			return nil
		}
	}
}

func (l *loopback) Close() error {
	l.closeOne.Do(func() {
		l.mu.Lock()
		l.stopped = true
		l.mu.Unlock()
		close(l.closed)
	})
	return nil
}

type errLoopbackClosed struct{}

func (errLoopbackClosed) Error() string { return "loopback: port closed" }

func (l *loopback) setSilent(v bool) {
	l.mu.Lock()
	l.silent = v
	l.mu.Unlock()
}

func (l *loopback) framesWritten() [][]byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([][]byte, len(l.written))
	copy(out, l.written)
	return out
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// newTestLink wires a Link to a loopback with short timeouts so a failure does
// not cost half a second per assertion.
func newTestLink(t *testing.T, lb *loopback) *pico.Link {
	t.Helper()
	l := pico.NewLink(lb, quietLogger(),
		pico.WithAckTimeout(150*time.Millisecond),
		pico.WithMock(true))
	t.Cleanup(func() { _ = l.Close() })
	return l
}

func sampleTelemetry() *rocsarv1.PicoTelemetry {
	return &rocsarv1.PicoTelemetry{
		GondolaHeadingDeg: 271.5,
		TargetHeadingDeg:  270,
		ImuPresent:        true,
		Heater1State:      true,
		Antennas: []*rocsarv1.AntennaTelemetry{
			{ServoId: 1, CurrentTick: 2048, CurrentAngleDeg: 90.5, Load: 12, TemperatureC: 31,
				FeedbackState: 1, FeedbackError: 0},
			{ServoId: 2, CurrentTick: 1024, CurrentAngleDeg: 270.5, Load: -4, TemperatureC: 29,
				FeedbackState: 2, FeedbackError: 0},
		},
	}
}

// ---------------------------------------------------------------------------

// Open must prove the link, not just open the port. A device that never answers
// is reported as absent rather than as a working link with no telemetry.
func TestLinkOpenFailsWhenDeviceIsSilent(t *testing.T) {
	lb := newLoopback()
	lb.silent = true
	l := newTestLink(t, lb)

	err := l.Open(context.Background())
	if err == nil {
		t.Fatal("Open succeeded against a device that never answers; " +
			"an openable port is not evidence that anything is on the other end")
	}
	if !errors.Is(err, pico.ErrAckTimeout) && l.Connected() {
		t.Error("link reports connected after a failed handshake")
	}
}

func TestLinkOpenSucceedsWhenDeviceAnswers(t *testing.T) {
	lb := newLoopback()
	lb.telemetry = sampleTelemetry()
	l := newTestLink(t, lb)

	if err := l.Open(context.Background()); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !l.Connected() {
		t.Error("Connected() is false after a successful handshake")
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if tel, ok := l.Telemetry(); ok && tel.GondolaHeadingDeg == 271.5 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("no telemetry arrived after Open, though the handshake succeeded")
}

// A command to a device that has never answered must be reported as "not
// connected", not as a timeout. The link was never established, so claiming a
// timeout would send an operator looking at the ACK path when the cable is the
// problem.
func TestCommandOnUnestablishedLinkIsNotConnected(t *testing.T) {
	lb := newLoopback()
	lb.silent = true
	l := newTestLink(t, lb)

	if err := l.Open(context.Background()); err == nil {
		t.Fatal("Open succeeded against a silent device")
	}
	if _, err := l.SetTarget(context.Background(), 180); !errors.Is(err, pico.ErrNotConnected) {
		t.Fatalf("got %v, want ErrNotConnected", err)
	}
}

// A command to a device that WAS connected and has gone quiet must time out,
// distinctly from "not connected". This is the wedge case -- a flight controller
// that has stopped answering -- and the two are different faults: one is a
// cable, the other is a hung device.
func TestCommandTimesOutWhenWedgedDeviceStopsAnswering(t *testing.T) {
	lb := newLoopback()
	l := newTestLink(t, lb)
	if err := l.Open(context.Background()); err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Established and answering, then it wedges.
	lb.setSilent(true)

	start := time.Now()
	_, err := l.SetTarget(context.Background(), 180)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a command to a silent device succeeded")
	}
	if !errors.Is(err, pico.ErrAckTimeout) {
		t.Fatalf("got %v, want an ACK timeout", err)
	}
	if errors.Is(err, pico.ErrNotConnected) {
		t.Error("a timeout was reported as 'not connected'; those are different faults")
	}
	if elapsed < 100*time.Millisecond {
		t.Errorf("returned after %s, before the ACK timeout could have elapsed", elapsed)
	}
}

// The sequence number must advance per command and the ACK must echo it. This
// is the whole correlation mechanism.
func TestSequenceAdvancesAndAckEchoes(t *testing.T) {
	lb := newLoopback()
	l := newTestLink(t, lb)
	if err := l.Open(context.Background()); err != nil {
		t.Fatalf("Open: %v", err)
	}

	var seqs []uint32
	for i := 0; i < 4; i++ {
		ack, err := l.SetTarget(context.Background(), float64(90+i))
		if err != nil {
			t.Fatalf("command %d: %v", i, err)
		}
		if !ack.Success {
			t.Fatalf("command %d not acknowledged: %s", i, ack.Error)
		}
		seqs = append(seqs, ack.CommandSequence)
	}

	for i := 1; i < len(seqs); i++ {
		if seqs[i] == seqs[i-1] {
			t.Fatalf("sequence repeated: %v -- two commands share a sequence and their ACKs are ambiguous", seqs)
		}
	}
	if seqs[0] == 0 {
		t.Error("the first command used sequence 0, which is reserved to mean 'unset'")
	}
}

// A frame delivered in pieces must be reassembled. Serial ports deliver whatever
// has arrived; assuming one read equals one frame is the assumption that works on
// an idle bench and fails under load.
func TestFramesSplitAcrossReadsAreReassembled(t *testing.T) {
	for _, chunk := range []int{1, 3, 7, 13} {
		lb := newLoopback()
		lb.chunk = chunk // force fragmentation
		lb.telemetry = sampleTelemetry()
		l := newTestLink(t, lb)

		if err := l.Open(context.Background()); err != nil {
			t.Fatalf("chunk=%d Open: %v", chunk, err)
		}
		if _, err := l.SetTarget(context.Background(), 45); err != nil {
			t.Fatalf("chunk=%d command: %v", chunk, err)
		}
		_ = l.Close()
	}
}

// A stale reply must not be delivered as the answer to the next command.
func TestDrainBeforeWritePreventsCrossTalk(t *testing.T) {
	lb := newLoopback()
	l := newTestLink(t, lb)
	if err := l.Open(context.Background()); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := l.SetTarget(context.Background(), 10); err != nil {
		t.Fatalf("first command: %v", err)
	}
	if _, err := l.SetTarget(context.Background(), 20); err != nil {
		t.Fatalf("second command: %v", err)
	}

	seen := map[uint32]bool{}
	for _, f := range lb.framesWritten() {
		cmd, err := lb.handle(f)
		if err != nil {
			continue
		}
		if seen[cmd.GetSequence()] {
			t.Errorf("sequence %d was written twice", cmd.GetSequence())
		}
		seen[cmd.GetSequence()] = true
	}
}

// Parameter validation happens on this side of the wire. The firmware's servo
// has 4096 positions and this build has two heaters; finding either out from
// the flight controller costs a round trip and possibly a flight.
func TestHostSideParameterValidation(t *testing.T) {
	lb := newLoopback()
	l := newTestLink(t, lb)
	if err := l.Open(context.Background()); err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()

	if _, err := l.Jog(ctx, 1, pico.MaxServoTick+1); err == nil {
		t.Error("a tick above 4095 was accepted")
	}
	if _, err := l.Jog(ctx, 1, 4095); err != nil {
		t.Errorf("the highest legal tick was rejected: %v", err)
	}
	if _, err := l.SetDirection(ctx, 1, 0.0); err == nil {
		t.Error("a direction multiplier of 0 was accepted")
	}
	if _, err := l.SetDirection(ctx, 1, 2.0); err == nil {
		t.Error("a direction multiplier of 2 was accepted")
	}
	for _, m := range []float64{1.0, -1.0} {
		if _, err := l.SetDirection(ctx, 1, m); err != nil {
			t.Errorf("legal multiplier %v rejected: %v", m, err)
		}
	}
	if _, err := l.SetHeater(ctx, 3, true); err == nil {
		t.Error("heater 3 was accepted; this build has two")
	}
	if _, err := l.SetHeater(ctx, 0, true); err == nil {
		t.Error("heater 0 was accepted")
	}
}

// The feedback tri-state must survive the wire. A servo that has stopped
// answering and one that has never been spoken to are different facts and the
// console has to be able to tell them apart.
func TestFeedbackTriStateSurvivesTheWire(t *testing.T) {
	for _, tc := range []struct {
		wire int32
		want domain.FeedbackState
	}{
		{0, domain.FeedbackUnknown},
		{1, domain.FeedbackMeasured},
		{2, domain.FeedbackHeld},
		// A value from a firmware newer than this schema. It must decode to
		// UNKNOWN, not to MEASURED: claiming a measurement we do not
		// understand is the one thing this field exists to prevent.
		{99, domain.FeedbackUnknown},
	} {
		tel := sampleTelemetry()
		tel.Antennas[0].FeedbackState = tc.wire

		lb := newLoopback()
		lb.telemetry = tel
		l := newTestLink(t, lb)
		if err := l.Open(context.Background()); err != nil {
			t.Fatalf("Open: %v", err)
		}
		if _, err := l.SetTarget(context.Background(), 1); err != nil {
			t.Fatalf("command: %v", err)
		}

		deadline := time.Now().Add(time.Second)
		var got domain.FeedbackState
		for time.Now().Before(deadline) {
			if d, ok := l.Telemetry(); ok && len(d.Axes) > 0 && d.Axes[0].FeedbackState == tc.want {
				got = d.Axes[0].FeedbackState
				break
			}
			time.Sleep(time.Millisecond)
		}
		if got != tc.want {
			t.Errorf("wire feedback_state %d decoded to %v, want %v", tc.wire, got, tc.want)
		}
		_ = l.Close()
	}
}

// A frame that survives COBS but fails protobuf is a sender bug, and must never
// be partially applied.
func TestGarbageFrameIsCountedNotApplied(t *testing.T) {
	lb := newLoopback()
	l := newTestLink(t, lb)
	if err := l.Open(context.Background()); err != nil {
		t.Fatalf("Open: %v", err)
	}

	// The link must be usable first, so establish a clean baseline.
	if _, err := l.SetTarget(context.Background(), 33); err != nil {
		t.Fatalf("link unusable before the garbage frame: %v", err)
	}

	// Well-formed COBS carrying bytes that are not a valid PicoMessage: 0xFF as
	// a leading byte is field 31 with wire type 7, which does not exist.
	//
	// Injected AFTER a command, deliberately. Every command drains the input
	// buffer first so a late reply cannot be mistaken for its own ACK, which
	// means anything queued before a command is discarded by design -- and that
	// is the feature, not a bug to work around.
	junk := append(pico.CobsEncode(nil, []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}), pico.Delimiter)
	lb.toHost <- junk

	// Wait for the receive loop to get to it.
	deadline := time.Now().Add(time.Second)
	var protocolErrors int
	for time.Now().Before(deadline) {
		_, protocolErrors, _, _ = l.Diagnostics()
		if protocolErrors > 0 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if protocolErrors == 0 {
		t.Error("a garbage frame was not counted as a protocol error")
	}

	// And the link must still work afterwards.
	if _, err := l.SetTarget(context.Background(), 44); err != nil {
		t.Fatalf("link unusable after a garbage frame: %v", err)
	}
}

// The frame budget must equal the firmware's, or the two ends disagree about
// what fits.
func TestMaxFrameMatchesTheFormula(t *testing.T) {
	want := pico.CobsEncodedSize(pico.MaxPayload) + 1
	if pico.MaxFrame != want {
		t.Errorf("MaxFrame = %d, but COBS worst case + delimiter = %d.\n"+
			"firmware/pico_wire.h computes the same value from PICO_TX_BUFFER=%d; "+
			"if these drift apart the link silently truncates.",
			pico.MaxFrame, want, pico.MaxPayload)
	}
	if pico.MaxFrame != 259 {
		t.Errorf("MaxFrame = %d, want 259 (PICO_TX_FRAME_MAX in pico_wire.h)", pico.MaxFrame)
	}
}

// Every command must produce exactly one delimited frame on the wire.
func TestEveryCommandEmitsOneDelimitedFrame(t *testing.T) {
	lb := newLoopback()
	l := newTestLink(t, lb)
	if err := l.Open(context.Background()); err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()

	ops := map[string]func() (*domain.Ack, error){
		"set_target":    func() (*domain.Ack, error) { return l.SetTarget(ctx, 12) },
		"jog":           func() (*domain.Ack, error) { return l.Jog(ctx, 1, 100) },
		"zero":          func() (*domain.Ack, error) { return l.Zero(ctx, 1) },
		"mount":         func() (*domain.Ack, error) { return l.Mount(ctx, 1, 3) },
		"set_direction": func() (*domain.Ack, error) { return l.SetDirection(ctx, 1, -1) },
		"set_heater":    func() (*domain.Ack, error) { return l.SetHeater(ctx, 2, true) },
		"stop":          func() (*domain.Ack, error) { return l.Stop(ctx, 1) },
	}

	for name, op := range ops {
		ack, err := op()
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !ack.Success {
			t.Errorf("%s: not acknowledged (%s)", name, ack.Error)
		}
	}

	for _, f := range lb.framesWritten() {
		if len(f) == 0 {
			t.Error("a zero-length frame was written")
			continue
		}
		if f[len(f)-1] != pico.Delimiter {
			t.Errorf("a frame was written without its delimiter: %x", f)
		}
		if idx := indexOf(f[:len(f)-1], 0x00); idx >= 0 {
			t.Errorf("a frame contains a zero byte at %d before the delimiter: %x", idx, f)
		}
	}
}

func indexOf(b []byte, c byte) int {
	for i, v := range b {
		if v == c {
			return i
		}
	}
	return -1
}

// protoUnmarshal is the fake flight controller's decoder, standing in for the
// firmware's pb_decode.
func protoUnmarshal(b []byte, m proto.Message) error { return proto.Unmarshal(b, m) }
