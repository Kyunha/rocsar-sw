package gnss

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/rocsar/obc/internal/domain"
)

// BankError is returned when a selection names a receiver that does not exist.
var BankError = errors.New("gnss: no such receiver")

// Bank holds every receiver and tracks which one is selected.
//
// Selection is EXPLICIT and never automatic. There is no voting, no outlier
// rejection, no "best fix" heuristic, and that is a deliberate decision rather
// than an omission:
//
//   - An automatic switchover changes which position the antennas are being
//     pointed from, without anyone deciding to. During a flight that is a
//     surprise change of a safety-relevant input.
//   - A heuristic that is wrong is worse than no heuristic, because it is
//     invisible. A receiver reporting a wildly different position might be
//     right -- a spoofing or multipath situation is exactly when you would want
//     to see three disagreeing fixes and a human choosing.
//   - The operator can see all three and which one is trusted, which is the
//     entire reason there are three receivers.
//
// The cost is that redundancy does not reduce operator workload. That is the
// trade, taken knowingly.
type Bank struct {
	receivers  []*Receiver
	byID       map[int]*Receiver
	selectedID int
	staleAfter time.Duration
	log        *slog.Logger

	mu      sync.RWMutex
	latest  map[int]domain.Fix
	fixes   chan domain.Fix
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	running bool
}

// NewBank creates receivers for the given ports.
//
// IDs are 1-based and correspond to the order of ports, because that is what
// the operator sees on the console. Port 2001 is receiver 1.
func NewBank(ports []int, selectedID int, staleAfter time.Duration, log *slog.Logger) (*Bank, error) {
	if len(ports) == 0 {
		return nil, errors.New("gnss: at least one port is required")
	}
	if selectedID < 1 || selectedID > len(ports) {
		return nil, fmt.Errorf("%w: selected %d, valid IDs are 1..%d", BankError, selectedID, len(ports))
	}

	b := &Bank{
		byID:       make(map[int]*Receiver, len(ports)),
		selectedID: selectedID,
		staleAfter: staleAfter,
		log:        log,
		latest:     make(map[int]domain.Fix, len(ports)),
		fixes:      make(chan domain.Fix, 128),
	}

	for i, port := range ports {
		r := NewReceiver(i+1, port, staleAfter, log)
		b.receivers = append(b.receivers, r)
		b.byID[i+1] = r
	}

	return b, nil
}

// Start opens every receiver and begins forwarding fixes.
func (b *Bank) Start(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	b.cancel = cancel

	for _, r := range b.receivers {
		if err := r.Open(ctx); err != nil {
			// Degrade, do not abort. One absent receiver is exactly the case
			// this bank exists for: two of three still fly. --require-hardware
			// in the composition root turns this into a startup failure for
			// anyone who would rather know immediately.
			b.log.Error("receiver did not bind, continuing degraded", "receiver", r.id, "err", err)
		}
	}

	b.mu.Lock()
	b.running = true
	b.mu.Unlock()

	for _, r := range b.receivers {
		b.wg.Add(1)
		go func(rr *Receiver) {
			defer b.wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case f, ok := <-rr.Fixes():
					if !ok {
						return
					}
					b.mu.Lock()
					b.latest[f.ReceiverID] = f
					b.mu.Unlock()
					select {
					case b.fixes <- f:
					default:
					}
				}
			}
		}(r)
	}

	r := b.byID[b.selectedID]
	r.SetSelected(true)
	b.log.Info("bank started", "receivers", len(b.receivers), "selected", b.selectedID)
	return nil
}

func (b *Bank) Close() error {
	if b.cancel != nil {
		b.cancel()
	}
	var firstErr error
	for _, r := range b.receivers {
		if err := r.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	b.wg.Wait()
	b.mu.Lock()
	b.running = false
	b.mu.Unlock()
	return firstErr
}

// Fixes is the merged stream from every receiver, tagged with its ID.
func (b *Bank) Fixes() <-chan domain.Fix { return b.fixes }

// Select makes receiverID the trusted one. The previous selection is marked
// unselected rather than removed, so telemetry keeps reporting all three.
func (b *Bank) Select(receiverID int) error {
	r, ok := b.byID[receiverID]
	if !ok {
		return fmt.Errorf("%w: %d", BankError, receiverID)
	}

	b.mu.Lock()
	prev := b.selectedID
	b.selectedID = receiverID
	b.mu.Unlock()

	b.byID[prev].SetSelected(false)
	r.SetSelected(true)

	b.log.Info("selection changed", "from", prev, "to", receiverID)
	return nil
}

// Rotate advances the selection to the next receiver, wrapping.
//
// This is the one control that makes the redundancy usable without an operator
// having to remember which receiver is trusted.
func (b *Bank) Rotate() error {
	b.mu.RLock()
	next := b.selectedID + 1
	if next > len(b.receivers) {
		next = 1
	}
	b.mu.RUnlock()
	return b.Select(next)
}

// SelectedID returns the trusted receiver.
func (b *Bank) SelectedID() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.selectedID
}

// SelectedFix returns the trusted receiver's most recent fix and whether it is
// usable.
//
// ok is false when there has never been a fix, when it is older than the
// staleness bound, or when the coordinates are out of range. All three mean the
// same thing to the caller: do not use this.
func (b *Bank) SelectedFix() (domain.Fix, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	f, ok := b.latest[b.selectedID]
	if !ok {
		return domain.Fix{}, false
	}
	if time.Since(f.ObservedAt) > b.staleAfter {
		return f, false
	}
	if !f.Valid() {
		return f, false
	}
	return f, true
}

// Status returns every receiver's health, ordered by ID so telemetry frames are
// stable and a diff between two frames means something.
func (b *Bank) Status() []domain.ReceiverStatus {
	out := make([]domain.ReceiverStatus, 0, len(b.receivers))
	for _, r := range b.receivers {
		out = append(out, r.Status())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ReceiverID < out[j].ReceiverID })
	return out
}

// Shutdown satisfies domain.Shutdown.
func (b *Bank) Shutdown(ctx context.Context) error { return b.Close() }
