package pico

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	rocsarv1 "github.com/rocsar/obc/api/rocsar/v1"
	"github.com/rocsar/obc/internal/domain"
)

// Link is the connection to the flight controller.
//
// There is deliberately NO reconnect loop here. Reconnection is a decision the
// composition root makes, and this is the reason: a link that reconnects on its
// own turns a recoverable absence into an invisible retry storm, and telemetry
// keeps reporting a healthy system that is not talking to anything. The
// alternative -- report DISCONNECTED and let the operator or the root decide --
// means the fault is visible at the moment it happens.
//
// Responsibilities kept together because they are one mechanism split across
// three files would be three places to look: the port, the COBS framing, the
// command sequence, the ACK correlation and the telemetry fan-out.
type Link struct {
	transport Transport
	log       *slog.Logger
	now       func() time.Time

	ackTimeout time.Duration

	// seqMu guards the sequence counter alone. It is separate from mu because
	// assigning a sequence must not block behind a telemetry callback that has
	// gone slow.
	seqMu sync.Mutex
	seq   uint32

	mu              sync.RWMutex
	connected       bool
	pending         map[uint32]chan *rocsarv1.PicoAck
	telemetry       domain.PicoTelemetry
	haveTelemetry   bool
	lastTelemetryAt time.Time
	lastAck         *rocsarv1.PicoAck
	unsolicited     uint64
	protocolErrs    []error

	listeners []func(domain.PicoTelemetry)

	writeMu sync.Mutex

	reader *frameReader
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// Mocked marks this link as a double. Surfaced in telemetry so fabricated
	// data is visible on the wire, not only in a log.
	mocked bool
}

// Option configures a Link.
type Option func(*Link)

// WithAckTimeout overrides how long to wait for an acknowledgement.
func WithAckTimeout(d time.Duration) Option {
	return func(l *Link) { l.ackTimeout = d }
}

// WithClock overrides the time source. Tests need this; production does not.
func WithClock(now func() time.Time) Option {
	return func(l *Link) { l.now = now }
}

// WithMock marks the link as a test double.
func WithMock(mocked bool) Option {
	return func(l *Link) { l.mocked = mocked }
}

// NewLink wraps a transport. The transport is injected rather than opened here
// so the composition root decides what a port is.
func NewLink(t Transport, log *slog.Logger, opts ...Option) *Link {
	l := &Link{
		transport:  t,
		log:        log,
		now:        time.Now,
		ackTimeout: DefaultAckTimeout,
		pending:    make(map[uint32]chan *rocsarv1.PicoAck),
		reader:     newFrameReader(),
	}
	for _, o := range opts {
		o(l)
	}
	return l
}

// Open starts the receive goroutine and proves the link with one status_request
// round trip.
//
// The round trip is the point. On Linux an unplugged USB CDC device leaves the
// port openable for a while, so a successful open is not evidence that anything
// is on the other end. A failed proof closes the port and returns the error, so
// a caller that ignores the result still does not end up believing it is
// connected.
//
// On failure the transport is closed and the receive goroutine is not left
// running. A half-open link that keeps reading is the shape of bug where the
// system reports telemetry it can no longer command.
func (l *Link) Open(ctx context.Context) error {
	l.mu.Lock()
	if l.connected {
		l.mu.Unlock()
		return nil
	}
	l.mu.Unlock()

	ctx, cancel := context.WithCancel(ctx)
	l.cancel = cancel

	// The framing accumulator is per-connection. One connection's half-received
	// frame must not be prepended to the next connection's first frame.
	l.reader.reset()

	seq := l.nextSequence()
	frame, err := EncodeTelemetryRequest(seq)
	if err != nil {
		cancel()
		return err
	}

	if err := l.transport.DrainRead(); err != nil {
		l.log.Debug("drain before probe failed", "err", err)
	}

	ackCh := l.register(seq)
	defer l.unregister(seq)

	// The receive goroutine starts BEFORE the write, or the ACK can arrive
	// before anyone is listening and be counted as unsolicited.
	l.wg.Add(1)
	go l.readLoop(ctx)

	if err := l.writeFrame(frame); err != nil {
		l.stop(cancel)
		return fmt.Errorf("status_request: %w", err)
	}

	// The flight controller emits telemetry every control tick whether or not
	// it was asked, and it acknowledges every command including this one. Either
	// proves the link. The timeout only fires when the device is absent or
	// wedged.
	proof := make(chan struct{})
	var once sync.Once
	markProven := func() { once.Do(func() { close(proof) }) }

	stopProof := make(chan struct{})
	defer close(stopProof)
	go func() {
		// If a telemetry frame arrives first, the link is proven too. Poll
		// rather than subscribe: the read loop must not grow a listener list
		// whose only job is to be closed here.
		t := time.NewTicker(5 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stopProof:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				l.mu.RLock()
				have := l.haveTelemetry
				l.mu.RUnlock()
				if have {
					markProven()
					return
				}
			}
		}
	}()

	timeout := time.NewTimer(l.ackTimeout)
	defer timeout.Stop()

	select {
	case ack, ok := <-ackCh:
		if !ok || ack == nil {
			l.abortOpen(cancel)
			return fmt.Errorf("open: %s closed the link during the handshake", l.transport)
		}
		if !ack.GetSuccess() {
			l.abortOpen(cancel)
			return fmt.Errorf("open: flight controller rejected status_request: %s", ack.GetError())
		}
		markProven()
		return nil

	case <-proof:
		return nil

	case <-timeout.C:
		l.abortOpen(cancel)
		return fmt.Errorf("open: %s did not answer a status_request within %s",
			l.transport, l.ackTimeout)
	}
}

// abortOpen unwinds a failed handshake, leaving nothing running.
func (l *Link) abortOpen(cancel context.CancelFunc) {
	l.mu.Lock()
	l.connected = false
	for seq, ch := range l.pending {
		close(ch)
		delete(l.pending, seq)
	}
	l.cancel = nil
	l.mu.Unlock()

	l.stop(cancel)
}

// stop tears the connection down in the only order that terminates.
//
// Order matters and getting it wrong deadlocks. The receive goroutine blocks
// inside Read, and Read has no deadline -- the serial library's read timeout
// bounds how long it waits for DATA, but a blocked call on a port that will
// never deliver anything only returns when the port is closed. So the port must
// be closed BEFORE waiting on the goroutine:
//
//	cancel()          stop the context, so the loop exits after its next read
//	transport.Close() unblock the Read that is currently in progress
//	wg.Wait()         now the loop can finish
//
// Waiting first, as this originally did, hangs forever: the goroutine is
// waiting for the close that is waiting for the goroutine.
func (l *Link) stop(cancel context.CancelFunc) {
	if cancel != nil {
		cancel()
	}
	if err := l.transport.Close(); err != nil {
		l.log.Debug("closing transport", "err", err)
	}
	l.wg.Wait()
	l.reader.reset()
}

// Close stops the receive goroutine and releases the port.
func (l *Link) Close() error {
	l.mu.Lock()
	if !l.connected {
		l.mu.Unlock()
		return nil
	}
	l.connected = false
	cancel := l.cancel
	l.cancel = nil

	// Every waiter must be released, or a command goroutine blocks until its
	// own timeout with nothing to show for it.
	for seq, ch := range l.pending {
		close(ch)
		delete(l.pending, seq)
	}
	l.mu.Unlock()

	l.stop(cancel)
	return nil
}

func (l *Link) readLoop(ctx context.Context) {
	defer l.wg.Done()

	chunk := make([]byte, ReadChunk)

	// The read timeout is what makes this loop responsive: it returns every
	// ReadTimeout so the context can be re-checked. A blocking Read on a silent
	// device would keep this goroutine alive until the port was closed.
	if err := l.transport.SetReadTimeout(ReadTimeout); err != nil {
		l.recordError(fmt.Errorf("set read timeout: %w", err))
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		n, err := l.transport.Read(chunk)
		if n > 0 {
			for _, frame := range l.reader.feed(chunk[:n]) {
				l.handleFrame(frame)
			}
		}

		// (0, nil) is the read timeout expiring with nothing to report. It is
		// the normal state of a healthy but idle link and must NOT be treated as
		// a fault -- doing so makes the link flap once per timeout period on a
		// device that is simply waiting for the next command.
		if err == nil {
			continue
		}
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, io.EOF) || errors.Is(err, ErrNotConnected) || isPortClosed(err) {
			return
		}
		if errors.Is(err, os.ErrDeadlineExceeded) {
			continue
		}

		// A real I/O error on a serial port is often a transient USB condition.
		// Recorded and counted, then the loop exits; the composition root
		// decides whether to reconnect, and it can see this in Diagnostics.
		l.recordError(fmt.Errorf("read: %w", err))
		return
	}
}

func (l *Link) handleFrame(frame []byte) {
	if len(frame) > MaxFrame {
		l.recordError(frameTooLarge(len(frame)))
		return
	}

	// The Pico->Pi direction is always an envelope: an ack or telemetry.
	msg, err := DecodeMessage(frame)
	if err != nil {
		l.recordError(fmt.Errorf("decode: %w", err))
		return
	}

	if ack := AckFromMessage(msg); ack != nil {
		l.handleAck(ack, msg.GetTimestampUs())
		return
	}
	if tel := TelemetryFromMessage(msg); tel != nil {
		l.handleTelemetry(tel)
		return
	}
	l.recordError(ErrNoPayload)
}

func (l *Link) handleAck(ack *rocsarv1.PicoAck, _ uint64) {
	l.mu.Lock()
	l.lastAck = ack
	ch, waiting := l.pending[ack.GetCommandSequence()]
	l.mu.Unlock()

	if !waiting {
		// An ACK nobody is waiting for is not noise: it means a command timed
		// out earlier and its reply arrived late, or the sequence counters have
		// desynchronised. Either way it is a fact about the link's health and it
		// is counted rather than dropped.
		l.mu.Lock()
		l.unsolicited++
		l.mu.Unlock()
		l.log.Warn("acknowledgement for a command nobody is waiting for",
			"command_sequence", ack.GetCommandSequence(),
			"unsolicited_total", l.unsolicited)
		return
	}

	select {
	case ch <- ack:
	default:
		// The waiter already gave up. Counted like the case above.
		l.mu.Lock()
		l.unsolicited++
		l.mu.Unlock()
	}
}

func (l *Link) handleTelemetry(tel *rocsarv1.PicoTelemetry) {
	d := telemetryToDomain(tel, l.now())

	l.mu.Lock()
	l.telemetry = d
	l.haveTelemetry = true
	l.lastTelemetryAt = l.now()
	l.mu.Unlock()
}

// exchange is the one path every command takes: assign a sequence, register a
// waiter, write, wait for the ACK, unregister.
//
// The unregister is deferred and unconditional. Leaving an entry in the pending
// map after a timeout means the next late ACK is delivered to a channel nobody
// reads, and the map grows by one entry per timed-out command for the life of
// the process.
func (l *Link) exchange(ctx context.Context, build func(seq uint32) (*rocsarv1.PicoCommand, error)) (*domain.Ack, error) {
	if !l.Connected() {
		return nil, ErrNotConnected
	}

	seq := l.nextSequence()
	cmd, err := build(seq)
	if err != nil {
		return nil, err
	}
	frame, err := EncodeCommand(cmd)
	if err != nil {
		return nil, err
	}

	ch := l.register(seq)
	defer l.unregister(seq)

	// Anything left over from an abandoned exchange would satisfy this waiter
	// with an ACK belonging to the previous command.
	if err := l.transport.DrainRead(); err != nil {
		l.log.Debug("drain before write failed", "err", err)
	}

	if err := l.writeFrame(frame); err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}

	timeout := time.NewTimer(l.ackTimeout)
	defer timeout.Stop()

	select {
	case ack, ok := <-ch:
		if !ok || ack == nil {
			return nil, ErrNotConnected
		}
		return &domain.Ack{
			CommandSequence: ack.GetCommandSequence(),
			Success:         ack.GetSuccess(),
			Error:           domain.ErrorCode(ack.GetError()),
			At:              l.now(),
		}, nil

	case <-timeout.C:
		return nil, fmt.Errorf("%w: sequence %d after %s", ErrAckTimeout, seq, l.ackTimeout)

	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// nextSequence assigns the next command sequence.
//
// Starts at 1 and wraps at 2^32. Zero is skipped on purpose: the field is
// non-optional on the wire and a frame carrying 0 almost always means the caller
// forgot to assign one, which would otherwise be indistinguishable from a
// wrapped counter.
func (l *Link) nextSequence() uint32 {
	l.seqMu.Lock()
	defer l.seqMu.Unlock()
	l.seq++
	if l.seq == 0 {
		l.seq = 1
	}
	return l.seq
}

func (l *Link) register(seq uint32) chan *rocsarv1.PicoAck {
	ch := make(chan *rocsarv1.PicoAck, 1)
	l.mu.Lock()
	l.pending[seq] = ch
	l.connected = true
	l.mu.Unlock()
	return ch
}

func (l *Link) unregister(seq uint32) {
	l.mu.Lock()
	delete(l.pending, seq)
	l.mu.Unlock()
}

// writeFrame serialises writes to the port.
//
// A ZeroMQ-like discipline: the transport is a single byte pipe and two
// goroutines writing to it interleave at arbitrary byte boundaries. COBS gives
// no protection against that -- a frame spliced into another decodes to garbage
// that passes the marker check.
func (l *Link) writeFrame(frame []byte) error {
	l.writeMu.Lock()
	defer l.writeMu.Unlock()

	n, err := l.transport.Write(frame)
	if err != nil {
		return err
	}
	if n != len(frame) {
		// A short write on a serial port is possible and is not recoverable by
		// retrying blindly: the frame is already half on the wire and the
		// receiver will resynchronise at the next delimiter, dropping whatever
		// is left. Reported so the operator sees a corrupted exchange rather
		// than silence.
		return fmt.Errorf("short write: %d of %d bytes", n, len(frame))
	}
	return nil
}

func (l *Link) recordError(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// Bounded ring. An unbounded error log on a flapping link is a memory leak
	// that looks like telemetry working perfectly.
	l.protocolErrs = append(l.protocolErrs, err)
	if len(l.protocolErrs) > 16 {
		l.protocolErrs = l.protocolErrs[len(l.protocolErrs)-16:]
	}
	l.log.Debug("link protocol error", "err", err)
}

// Connected reports whether the link believes it is up.
func (l *Link) Connected() bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.connected
}

// Telemetry returns the most recent frame and whether one has ever arrived.
func (l *Link) Telemetry() (domain.PicoTelemetry, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.telemetry, l.haveTelemetry
}

// LastAck returns the most recent acknowledgement.
func (l *Link) LastAck() *rocsarv1.PicoAck {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.lastAck
}

// Diagnostics reports link health for the operator console. These are the
// numbers that distinguish "the flight controller is wedged" from "the cable
// came loose", which are otherwise the same silence.
func (l *Link) Diagnostics() (unsolicited uint64, protocolErrors int, telemetryAge time.Duration, haveTelemetry bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	age := time.Duration(0)
	if l.haveTelemetry {
		age = l.now().Sub(l.lastTelemetryAt)
	}
	return l.unsolicited, len(l.protocolErrs), age, l.haveTelemetry
}

func (l *Link) Mocked() bool { return l.mocked }

// ---------------------------------------------------------------------------
// domain.Pico implementation
// ---------------------------------------------------------------------------

func (l *Link) SetTarget(ctx context.Context, deg float64) (*domain.Ack, error) {
	return l.exchange(ctx, func(seq uint32) (*rocsarv1.PicoCommand, error) {
		return &rocsarv1.PicoCommand{
			Sequence: seq,
			Payload: &rocsarv1.PicoCommand_SetTarget{
				SetTarget: &rocsarv1.SetTargetCommand{TargetHeadingDeg: float32(deg)},
			},
		}, nil
	})
}

func (l *Link) Jog(ctx context.Context, servoID, tick uint32) (*domain.Ack, error) {
	if tick > MaxServoTick {
		return nil, fmt.Errorf("tick %d out of range 0..%d", tick, MaxServoTick)
	}
	return l.exchange(ctx, func(seq uint32) (*rocsarv1.PicoCommand, error) {
		return &rocsarv1.PicoCommand{
			Sequence: seq,
			Payload: &rocsarv1.PicoCommand_Jog{
				Jog: &rocsarv1.JogCommand{ServoId: servoID, Tick: tick},
			},
		}, nil
	})
}

func (l *Link) Zero(ctx context.Context, servoID uint32) (*domain.Ack, error) {
	return l.exchange(ctx, func(seq uint32) (*rocsarv1.PicoCommand, error) {
		return &rocsarv1.PicoCommand{
			Sequence: seq,
			Payload: &rocsarv1.PicoCommand_Zero{
				Zero: &rocsarv1.ZeroCommand{ServoId: servoID},
			},
		}, nil
	})
}

func (l *Link) Mount(ctx context.Context, servoID uint32, offsetDeg float64) (*domain.Ack, error) {
	return l.exchange(ctx, func(seq uint32) (*rocsarv1.PicoCommand, error) {
		return &rocsarv1.PicoCommand{
			Sequence: seq,
			Payload: &rocsarv1.PicoCommand_Mount{
				Mount: &rocsarv1.MountCommand{ServoId: servoID, OffsetDeg: float32(offsetDeg)},
			},
		}, nil
	})
}

func (l *Link) SetDirection(ctx context.Context, servoID uint32, multiplier float64) (*domain.Ack, error) {
	// Checked here rather than left to the firmware. The set is closed and a
	// multiplier of 0 would park an axis permanently; finding that out from the
	// console costs a round trip and a flight.
	if multiplier != 1.0 && multiplier != -1.0 {
		return nil, fmt.Errorf("direction multiplier %v must be +1.0 or -1.0", multiplier)
	}
	return l.exchange(ctx, func(seq uint32) (*rocsarv1.PicoCommand, error) {
		return &rocsarv1.PicoCommand{
			Sequence: seq,
			Payload: &rocsarv1.PicoCommand_Dir{
				Dir: &rocsarv1.DirCommand{ServoId: servoID, Multiplier: float32(multiplier)},
			},
		}, nil
	})
}

func (l *Link) SetHeater(ctx context.Context, heaterID uint32, on bool) (*domain.Ack, error) {
	if heaterID != 1 && heaterID != 2 {
		return nil, fmt.Errorf("heater %d does not exist; this build has two", heaterID)
	}
	return l.exchange(ctx, func(seq uint32) (*rocsarv1.PicoCommand, error) {
		return &rocsarv1.PicoCommand{
			Sequence: seq,
			Payload: &rocsarv1.PicoCommand_Heater{
				Heater: &rocsarv1.HeaterCommand{HeaterId: heaterID, State: on},
			},
		}, nil
	})
}

func (l *Link) Stop(ctx context.Context, servoID uint32) (*domain.Ack, error) {
	return l.exchange(ctx, func(seq uint32) (*rocsarv1.PicoCommand, error) {
		return &rocsarv1.PicoCommand{
			Sequence: seq,
			Payload: &rocsarv1.PicoCommand_Stop{
				Stop: &rocsarv1.StopCommand{ServoId: servoID},
			},
		}, nil
	})
}

func (l *Link) State() domain.SubsystemState {
	if l.Connected() {
		return domain.SubsystemReady
	}
	return domain.SubsystemDisconnected
}

// MaxServoTick is the ST3215's tick range.
//
// It comes from the servo, not from this system: 4096 positions over the
// output range. The firmware has the same constant in gondola_model.h and it
// must stay equal to it, or the console will accept a jog the axis cannot
// perform.
const MaxServoTick = 4095

// isPortClosed reports whether err means the transport is closed, which is how
// the receive loop learns to exit after Close.
func isPortClosed(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "port closed") || strings.Contains(msg, "closed port")
}

// Shutdown satisfies domain.Shutdown. It is Close with a context, so the
// composition root can unwind everything uniformly without type-switching.
func (l *Link) Shutdown(ctx context.Context) error { return l.Close() }
