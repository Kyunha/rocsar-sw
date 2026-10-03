package gnss

import (
	"context"
	"sync"
	"time"

	"github.com/rocsar/obc/internal/domain"
)

// Mock is a GNSS receiver that never fixes.
//
// It exists so a bench or a test can exercise the bank and the telemetry frame
// without a receiver on the bench. It deliberately never produces a fix: a mock
// that invented coordinates would be indistinguishable from a real one, and
// "the OBC invented a position" is exactly the failure this project is most
// afraid of.
//
// It is never selected unless the operator asks for it. A bank of three mocks
// reports three receivers with fix_ok = false, which is honest.
type Mock struct {
	id int

	mu       sync.Mutex
	selected bool
	accepted uint64
	rejected uint64
	hasFix   bool
	last     domain.Fix

	fixes chan domain.Fix
	done  chan struct{}
	once  sync.Once
}

// NewMock returns a mock receiver with the given ID.
func NewMock(id int) *Mock {
	return &Mock{
		id:    id,
		fixes: make(chan domain.Fix, 16),
		done:  make(chan struct{}),
	}
}

func (m *Mock) Open(ctx context.Context) error { return nil }

func (m *Mock) Close() error {
	m.once.Do(func() { close(m.done) })
	return nil
}

func (m *Mock) Fixes() <-chan domain.Fix { return m.fixes }

func (m *Mock) Status() domain.ReceiverStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := domain.ReceiverStatus{
		ReceiverID: m.id,
		Selected:   m.selected,
		Accepted:   m.accepted,
		Rejected:   m.rejected,
		HasFix:     m.hasFix,
	}
	if m.hasFix {
		st.Fix = m.last
		st.FixAge = time.Since(m.last.ObservedAt)
		st.FixOK = m.last.Valid()
	}
	return st
}

// SetSelected marks this receiver as selected, as the Bank does for a real one.
func (m *Mock) SetSelected(v bool) {
	m.mu.Lock()
	m.selected = v
	m.mu.Unlock()
}

// Inject publishes a fix. Only for tests that need a specific coordinate; the
// mock never invents one on its own.
func (m *Mock) Inject(f domain.Fix) {
	m.mu.Lock()
	m.hasFix = true
	m.last = f
	m.accepted++
	m.mu.Unlock()

	select {
	case m.fixes <- f:
	default:
	}
}
