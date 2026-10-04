package sdr

import (
	"context"
	"sync"

	"github.com/rocsar/obc/internal/domain"
)

// Mock is an SDR that simulates an acquisition without a USB device.
//
// The observable difference from the real service is one field: Mocked. It
// reaches telemetry, so a Ground Station can tell a simulated acquisition from a
// real one. That is the whole point -- see ARCHITECTURE.md 9, on a system that
// fabricates telemetry because a device was not found being the worst failure
// mode this project has.
type Mock struct {
	mu       sync.Mutex
	state    domain.SubsystemState
	running  bool
	pid      int64
	acquires int
	lastLog  string
	probeOut string
}

var _ domain.Sdr = (*Mock)(nil)

// NewMock returns a mock SDR.
func NewMock() *Mock {
	return &Mock{
		state:    domain.SubsystemDisconnected,
		probeOut: "Mock SDR: no hardware present.\n",
	}
}

func (m *Mock) Params(ctx context.Context) (domain.SdrParams, error) {
	return domain.SdrParams{PRFHz: 2750, SampleRateHz: 31.251e6, TxFreqHz: 5.8e9}, nil
}

func (m *Mock) SetParams(ctx context.Context, p domain.SdrParamsPatch) error { return nil }

func (m *Mock) Connect(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.running {
		return ErrAlreadyRunning
	}
	m.running = true
	m.acquires++
	m.pid = int64(1000 + m.acquires)
	m.state = domain.SubsystemBusy
	m.lastLog = "mock: no log was written"
	return nil
}

func (m *Mock) Running() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running
}

func (m *Mock) PID() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pid
}

func (m *Mock) LastLog() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastLog
}

func (m *Mock) LastOutput() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return ""
}

func (m *Mock) Probe(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.probeOut, nil
}

func (m *Mock) ResetUSB(ctx context.Context) error { return nil }

func (m *Mock) State() domain.SubsystemState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

// Stop ends the simulated acquisition.
func (m *Mock) Stop(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.running = false
	m.state = domain.SubsystemReady
	return nil
}

func (m *Mock) Close() error { return nil }
