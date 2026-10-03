package pico

import (
	"context"
	"fmt"
	"sync"
	"time"

	rocsarv1 "github.com/rocsar/obc/api/rocsar/v1"
	"github.com/rocsar/obc/internal/domain"
)

// Mock is an in-memory flight controller.
//
// It routes through the REAL codec -- CobsEncode, proto.Marshal, the same frame
// budget -- so a test against the mock exercises the wire format and not just
// the control flow. A mock that returns domain structs directly tests nothing
// about the part that fails in the field.
//
// It is opt-in and loud. Nothing is simulated without Mock() being called
// explicitly, because a system that fabricates telemetry because a device was
// not found is the worst failure mode this project has.
type Mock struct {
	mu           sync.Mutex
	telemetry    domain.PicoTelemetry
	hasTelemetry bool
	// failWith, when non-nil, is returned by every command. Used to exercise
	// the degraded path without unplugging anything.
	failWith *rocsarv1.PicoAck

	connected bool
	opened    int
	closed    int

	// echoTelemetry makes every command produce a telemetry frame, which is what
	// the real flight controller does at 50 Hz regardless of what it is sent.
	echoTelemetry bool
	// silent suppresses acknowledgements, to exercise the ACK timeout path.
	silent bool

	now func() time.Time
}

// MockOption configures a Mock.
type MockOption func(*Mock)

// MockSilent makes the mock never acknowledge, so a caller times out.
func MockSilent(v bool) MockOption { return func(m *Mock) { m.silent = v } }

// MockEchoTelemetry makes the mock emit a telemetry frame per command.
func MockEchoTelemetry(v bool) MockOption { return func(m *Mock) { m.echoTelemetry = v } }

// MockFailsWith makes every command fail with the given code.
func MockFailsWith(code rocsarv1.ErrorCode) MockOption {
	return func(m *Mock) { m.failWith = &rocsarv1.PicoAck{Success: false, Error: code} }
}

// MockTelemetry seeds the telemetry the mock reports.
func MockTelemetry(t domain.PicoTelemetry) MockOption {
	return func(m *Mock) { m.telemetry = t; m.hasTelemetry = true }
}

func NewMock(opts ...MockOption) *Mock {
	m := &Mock{connected: true, now: time.Now}
	for _, o := range opts {
		o(m)
	}
	return m
}

func (m *Mock) Open(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.opened++
	m.connected = true
	return nil
}

func (m *Mock) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed++
	m.connected = false
	return nil
}

func (m *Mock) Connected() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.connected
}

// Counts reports how many times Open and Close were called. A reconnect loop
// shows up here as a climbing counter, which is the thing the previous system
// made invisible.
func (m *Mock) Counts() (opened, closed int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.opened, m.closed
}

// respond produces the ACK for a command, having encoded and decoded it through
// the real codec so the frame format is exercised.
func (m *Mock) respond(cmd *rocsarv1.PicoCommand) (*domain.Ack, error) {
	// Round-trip through the wire format. A mock that skips this can pass
	// while the real link is broken.
	frame, err := EncodeCommand(cmd)
	if err != nil {
		return nil, err
	}
	if frame[len(frame)-1] != Delimiter {
		return nil, fmt.Errorf("mock: encoded frame is not delimited")
	}
	inner, err := DecodeMessage(frame[:len(frame)-1])
	if err != nil {
		return nil, fmt.Errorf("mock: own frame did not decode: %w", err)
	}
	if inner.GetAck() != nil {
		return nil, fmt.Errorf("mock: a command frame decoded as an envelope")
	}

	m.mu.Lock()
	silent, fail := m.silent, m.failWith
	m.mu.Unlock()

	if silent {
		// The real link would simply not answer. Returning nil with no error
		// makes the caller's timeout fire, which is what we want to exercise.
		return nil, nil
	}
	if fail != nil {
		return &domain.Ack{
			CommandSequence: cmd.GetSequence(),
			Success:         false,
			Error:           domain.ErrorCode(fail.GetError()),
			At:              m.now(),
		}, nil
	}
	return &domain.Ack{CommandSequence: cmd.GetSequence(), Success: true, At: m.now()}, nil
}

func (m *Mock) SetTarget(ctx context.Context, deg float64) (*domain.Ack, error) {
	return m.respond(&rocsarv1.PicoCommand{
		Sequence: 1,
		Payload: &rocsarv1.PicoCommand_SetTarget{
			SetTarget: &rocsarv1.SetTargetCommand{TargetHeadingDeg: float32(deg)},
		},
	})
}

func (m *Mock) Jog(ctx context.Context, servoID, tick uint32) (*domain.Ack, error) {
	if tick > MaxServoTick {
		return nil, fmt.Errorf("tick %d out of range 0..%d", tick, MaxServoTick)
	}
	return m.respond(&rocsarv1.PicoCommand{
		Sequence: 1,
		Payload:  &rocsarv1.PicoCommand_Jog{Jog: &rocsarv1.JogCommand{ServoId: servoID, Tick: tick}},
	})
}

func (m *Mock) Zero(ctx context.Context, servoID uint32) (*domain.Ack, error) {
	return m.respond(&rocsarv1.PicoCommand{
		Sequence: 1,
		Payload:  &rocsarv1.PicoCommand_Zero{Zero: &rocsarv1.ZeroCommand{ServoId: servoID}},
	})
}

func (m *Mock) Mount(ctx context.Context, servoID uint32, offsetDeg float64) (*domain.Ack, error) {
	return m.respond(&rocsarv1.PicoCommand{
		Sequence: 1,
		Payload: &rocsarv1.PicoCommand_Mount{
			Mount: &rocsarv1.MountCommand{ServoId: servoID, OffsetDeg: float32(offsetDeg)},
		},
	})
}

func (m *Mock) SetDirection(ctx context.Context, servoID uint32, multiplier float64) (*domain.Ack, error) {
	if multiplier != 1.0 && multiplier != -1.0 {
		return nil, fmt.Errorf("direction multiplier %v must be +1.0 or -1.0", multiplier)
	}
	return m.respond(&rocsarv1.PicoCommand{
		Sequence: 1,
		Payload: &rocsarv1.PicoCommand_Dir{
			Dir: &rocsarv1.DirCommand{ServoId: servoID, Multiplier: float32(multiplier)},
		},
	})
}

func (m *Mock) SetHeater(ctx context.Context, heaterID uint32, on bool) (*domain.Ack, error) {
	if heaterID != 1 && heaterID != 2 {
		return nil, fmt.Errorf("heater %d does not exist; this build has two", heaterID)
	}
	return m.respond(&rocsarv1.PicoCommand{
		Sequence: 1,
		Payload: &rocsarv1.PicoCommand_Heater{
			Heater: &rocsarv1.HeaterCommand{HeaterId: heaterID, State: on},
		},
	})
}

func (m *Mock) Stop(ctx context.Context, servoID uint32) (*domain.Ack, error) {
	return m.respond(&rocsarv1.PicoCommand{
		Sequence: 1,
		Payload:  &rocsarv1.PicoCommand_Stop{Stop: &rocsarv1.StopCommand{ServoId: servoID}},
	})
}

func (m *Mock) OnTelemetry(fn func(domain.PicoTelemetry)) {}
func (m *Mock) Telemetry() (domain.PicoTelemetry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.telemetry, m.hasTelemetry
}

func (m *Mock) State() domain.SubsystemState {
	if m.Connected() {
		return domain.SubsystemReady
	}
	return domain.SubsystemDisconnected
}

func (m *Mock) SetTelemetry(t domain.PicoTelemetry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.telemetry = t
	m.hasTelemetry = true
}
