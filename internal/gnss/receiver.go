package gnss

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/rocsar/obc/internal/domain"
)

// Receiver is one passive u-blox receiver over UDP.
//
// Passive means bind-then-read, never connect. A connected UDP socket only
// receives datagrams from a peer that was already talking to it, and that is
// the classic bug that makes a feed look dead: Read_uB would have to know the
// OBC's ephemeral port in advance. The OBC is the server here, not the client.
type Receiver struct {
	id   int
	port int
	log  *slog.Logger

	conn  *net.UDPConn
	fixes chan domain.Fix

	mu       sync.Mutex
	accepted uint64
	rejected uint64
	lastErr  error
	lastFix  domain.Fix
	hasFix   bool
	lastSeen time.Time
	selected bool

	done chan struct{}
	wg   sync.WaitGroup
}

// NewReceiver returns a receiver bound to nothing yet. Call Open.
func NewReceiver(id, port int, log *slog.Logger) *Receiver {
	return &Receiver{
		id:    id,
		port:  port,
		log:   log.With("receiver", id, "port", port),
		fixes: make(chan domain.Fix, 64),
		done:  make(chan struct{}),
	}
}

// Open binds the UDP port and starts the receive goroutine.
//
// It returns as soon as the socket is bound. Nothing is waiting for Read_uB to
// appear, because a GNSS receiver that has not started yet is normal at boot
// and must not delay the rest of the system.
func (r *Receiver) Open(ctx context.Context) error {
	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: r.port}
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		return fmt.Errorf("bind udp4 127.0.0.1:%d: %w", r.port, err)
	}
	r.conn = conn

	r.wg.Add(1)
	go r.readLoop(ctx)
	r.log.Info("bound")
	return nil
}

func (r *Receiver) readLoop(ctx context.Context) {
	defer r.wg.Done()

	// One buffer, reused. A datagram is exactly 142 bytes; anything longer is
	// truncated by the kernel and reported, which the length check then
	// rejects. Buffering 64 KiB would accept a flood at line rate, and this
	// socket faces a local sender we control.
	buf := make([]byte, 2048)

	for {
		select {
		case <-ctx.Done():
			return
		case <-r.done:
			return
		default:
		}

		n, _, err := r.conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-r.done:
				return
			default:
			}
			// A closed socket during shutdown is expected, not a fault.
			if ctx.Err() != nil {
				return
			}
			r.noteError(err)
			// Back off briefly so a permanently broken socket cannot spin.
			time.Sleep(50 * time.Millisecond)
			continue
		}

		fix, err := Decode(buf[:n], r.id, time.Now())
		if err != nil {
			r.mu.Lock()
			r.rejected++
			r.lastErr = err
			r.mu.Unlock()
			// Logged at debug, not warn: a malformed datagram is counted and
			// visible in telemetry. Warning-level logging per bad packet would
			// bury everything else on a link that is misbehaving.
			r.log.Debug("rejected datagram", "bytes", n, "err", err)
			continue
		}

		d := domain.Fix{
			ReceiverID:    r.id,
			LatitudeDeg:   fix.Latitude, // already degrees. See decode.go.
			LongitudeDeg:  fix.Longitude,
			AltitudeM:     fix.Height,
			GroundSpeedMP: fix.GroundSpeed(),
			CourseDeg:     fix.Heading,
			ObservedAt:    time.Now(),
		}

		r.mu.Lock()
		r.accepted++
		r.lastFix = d
		r.hasFix = true
		r.lastSeen = d.ObservedAt
		r.mu.Unlock()

		// Non-blocking send. Dropping a fix is correct: the next one is 100 ms
		// behind and fresher, and blocking here would stall the socket read
		// while a slow consumer held up the sequence.
		select {
		case r.fixes <- d:
		default:
			r.log.Debug("fix channel full, dropped")
		}
	}
}

func (r *Receiver) noteError(err error) {
	r.mu.Lock()
	r.lastErr = err
	r.mu.Unlock()
}

func (r *Receiver) Close() error {
	close(r.done)
	if r.conn != nil {
		err := r.conn.Close()
		r.wg.Wait()
		return err
	}
	r.wg.Wait()
	return nil
}

// Fixes returns the accepted-fix stream. Closed on Close.
func (r *Receiver) Fixes() <-chan domain.Fix { return r.fixes }

// SetSelected marks this receiver as the bank's chosen one. The flag lives on
// the receiver only so Status can report it in one place; the authoritative
// selection is the Bank's.
func (r *Receiver) SetSelected(v bool) {
	r.mu.Lock()
	r.selected = v
	r.mu.Unlock()
}

// Status reports health for telemetry.
func (r *Receiver) Status(staleAfter time.Duration) domain.ReceiverStatus {
	r.mu.Lock()
	defer r.mu.Unlock()

	st := domain.ReceiverStatus{
		ReceiverID: r.id,
		Selected:   r.selected,
		Accepted:   r.accepted,
		Rejected:   r.rejected,
		HasFix:     r.hasFix,
	}
	if r.hasFix {
		st.Fix = r.lastFix
		st.FixAge = time.Since(r.lastSeen)
		// An old fix is worse than no fix. Reported false even though the last
		// decoded value was valid, because that is the situation where an
		// operator is most likely to trust a stale number.
		st.FixOK = r.lastFix.Valid() && st.FixAge <= staleAfter
	}
	return st
}
