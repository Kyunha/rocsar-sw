// Package telemetry assembles the 1 Hz aggregate frame.
//
// Engine is pure: it takes a snapshot of providers and returns a domain value.
// It has no goroutine, no clock and no socket. The Pipeline owns the clock.
//
// That separation is what makes the assembly testable at all -- a telemetry frame
// is the operator's only view of the vehicle, and testing it by starting a server
// and waiting a second per assertion is not testing.
package telemetry

import (
	"sort"
	"time"

	"github.com/rocsar/obc/internal/domain"
)

// FrameBudgetBytes is the hard cap on a serialised telemetry frame.
//
// The link's priority class is 41 kbit/s, about 5 kB/s. A 2048-byte frame at
// 1 Hz uses 41% of it -- headroom for a resync and for a slow joiner -- while a
// 5 kB frame would use the entire class and starve command responses.
//
// Enforced by test/telemetry_test.go. A frame that outgrows this is a bug in
// whatever was added, and the test names the field.
const FrameBudgetBytes = 2048

// Providers supplies one piece of the frame. Each is a method rather than an
// interface so the caller can pass closures and the package needs no knowledge
// of who implements what.
type Providers struct {
	System     func() SystemSnapshot
	GNSS       func() []domain.ReceiverStatus
	Pico       func() (domain.PicoTelemetry, bool)
	PicoConn   func() bool
	PicoAck    func() *domain.Ack
	SDR        func() SDRSnapshot
	Camera     func() domain.SubsystemState
	CameraDev  func() string
	CameraLast func() (string, uint64)
	Link       func() domain.LinkStatus
}

// SystemSnapshot is the host's own health.
type SystemSnapshot struct {
	State   domain.SubsystemState
	Uptime  time.Duration
	CPUTemp float64
	Mocked  []string
}

// SDRSnapshot is the acquisition program's state.
type SDRSnapshot struct {
	State     domain.SubsystemState
	Running   bool
	PID       int64
	LastLog   string
	LastError string
}

// Engine assembles frames.
type Engine struct {
	providers Providers
	now       func() time.Time
	startedAt time.Time
	sequence  uint64
}

// NewEngine returns an engine reading from the given providers.
func NewEngine(p Providers, now func() time.Time) *Engine {
	if now == nil {
		now = time.Now
	}
	return &Engine{providers: p, now: now, startedAt: now()}
}

// Sequence returns the sequence number of the last assembled frame.
func (e *Engine) Sequence() uint64 { return e.sequence }

// Build assembles one frame and advances the sequence.
//
// The sequence advances here and nowhere else. A gap must mean "frames were
// lost on the link" and a lower number must mean "the OBC restarted"; if any
// other code path could touch the counter, neither would mean anything.
func (e *Engine) Build() Snapshot {
	now := e.now()
	e.sequence++

	s := Snapshot{
		GeneratedAt: now,
		Uptime:      now.Sub(e.startedAt),
		Sequence:    e.sequence,
	}

	if e.providers.System != nil {
		s.System = e.providers.System()
	}
	if e.providers.GNSS != nil {
		s.GNSS = e.providers.GNSS()
		sort.Slice(s.GNSS, func(i, j int) bool { return s.GNSS[i].ReceiverID < s.GNSS[j].ReceiverID })
	}
	if e.providers.Pico != nil {
		s.Pico, s.HasPico = e.providers.Pico()
	}
	if e.providers.PicoConn != nil {
		s.PicoConnected = e.providers.PicoConn()
	}
	if e.providers.PicoAck != nil {
		s.PicoAck = e.providers.PicoAck()
	}
	if e.providers.SDR != nil {
		s.SDR = e.providers.SDR()
	}
	if e.providers.Camera != nil {
		s.Camera = e.providers.Camera()
	}
	if e.providers.CameraDev != nil {
		s.CameraDevice = e.providers.CameraDev()
	}
	if e.providers.CameraLast != nil {
		s.LastPhoto, s.PhotosTaken = e.providers.CameraLast()
	}
	if e.providers.Link != nil {
		s.Link = e.providers.Link()
	}

	return s
}

// Snapshot is one instant of the whole system.
//
// Domain types, not protobuf. The conversion to the wire happens in
// internal/transport, which keeps this package free of the schema -- see
// ARCHITECTURE.md 3.
type Snapshot struct {
	Sequence    uint64
	GeneratedAt time.Time
	Uptime      time.Duration

	System SystemSnapshot
	GNSS   []domain.ReceiverStatus

	Pico          domain.PicoTelemetry
	HasPico       bool
	PicoConnected bool
	PicoAck       *domain.Ack

	SDR          SDRSnapshot
	Camera       domain.SubsystemState
	CameraDevice string
	LastPhoto    string
	PhotosTaken  uint64

	Link domain.LinkStatus
}

// SelectedFix returns the trusted receiver's fix.
//
// Kept as a method rather than recomputed at each call site, because "which
// receiver is trusted" is a question with exactly one answer and it should have
// exactly one implementation.
func (s Snapshot) SelectedFix() (domain.Fix, bool) {
	for _, r := range s.GNSS {
		if !r.Selected {
			continue
		}
		if !r.FixOK {
			// The receiver is selected but its fix is stale or invalid. Reported
			// as no fix: the point of the staleness bound is that an old
			// coordinate must not be used, and returning it "because it is the
			// selected one" would defeat that.
			return domain.Fix{}, false
		}
		return r.Fix, true
	}
	return domain.Fix{}, false
}

// Healthy reports whether every required subsystem is READY.
//
// Advisory, for the Ground Station's banner. Nothing in the OBC acts on it: the
// failure policy is per-subsystem degradation, not a global gate, and a single
// unhealthy subsystem must not stop the telemetry for the others.
func (s Snapshot) Healthy() bool {
	return s.System.State == domain.SubsystemReady &&
		s.PicoConnected &&
		s.Camera != domain.SubsystemError &&
		s.SDR.State != domain.SubsystemError
}

// MockedSubsystems returns the sorted names of subsystems running against a
// double.
//
// Sorted because it goes on the wire, and an unordered list makes two frames
// differ for no reason, which trains a client to ignore differences.
func (s Snapshot) MockedSubsystems() []string {
	out := append([]string(nil), s.System.Mocked...)
	sort.Strings(out)
	return out
}
