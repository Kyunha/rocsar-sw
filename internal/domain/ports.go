package domain

import (
	"context"
	"time"
)

// This file is the seam. Every interface the OBC reaches the outside world
// through is declared here, in the package that consumes it, so the dependency
// arrow points from user to implementation and nothing above this line knows
// that serial ports, sockets or subprocesses exist.

// Pico is the flight controller.
//
// There is deliberately no reconnect loop in the real implementation. A link
// that reconnects on its own turns a recoverable absence into an invisible
// retry storm that hides the fault from telemetry -- the operator sees a healthy
// system that is not actually talking to anything. Reconnection is a decision
// the composition root makes, and it reports the outcome.
type Pico interface {
	// Open establishes the serial connection and proves it with one
	// status_request round trip. Opening a port successfully is not evidence
	// that a device is on the other end of it.
	Open(ctx context.Context) error
	Close() error
	Connected() bool

	// SetTarget points both axes at an absolute bearing in degrees.
	SetTarget(ctx context.Context, deg float64) (*Ack, error)
	// Jog moves one axis to an absolute tick, 0..4095, in manual mode.
	Jog(ctx context.Context, servoID, tick uint32) (*Ack, error)
	Zero(ctx context.Context, servoID uint32) (*Ack, error)
	Mount(ctx context.Context, servoID uint32, offsetDeg float64) (*Ack, error)
	SetDirection(ctx context.Context, servoID uint32, multiplier float64) (*Ack, error)
	SetHeater(ctx context.Context, heaterID uint32, on bool) (*Ack, error)
	Stop(ctx context.Context, servoID uint32) (*Ack, error)

	// OnTelemetry registers a callback invoked on the receive goroutine for
	// every telemetry frame. Callbacks must not block: they run in the path
	// that reads the serial port.
	OnTelemetry(fn func(PicoTelemetry))
	// Telemetry returns the most recent frame and when it arrived.
	Telemetry() (PicoTelemetry, bool)
}

// Ack is the flight controller's answer to one command.
type Ack struct {
	CommandSequence uint32
	Success         bool
	Error           ErrorCode
	At              time.Time
}

// GnssReceiver is one passive u-blox receiver.
//
// Passive means it binds and reads; it never connects. A connected UDP socket
// only receives from a peer that was already talking to it, which is the
// classic bug that makes a feed look dead.
type GnssReceiver interface {
	// Open binds the UDP port. It does not wait for a sender.
	Open(ctx context.Context) error
	Close() error

	// Fixes delivers every accepted fix. Only validated datagrams arrive here;
	// rejects are counted and not forwarded.
	Fixes() <-chan Fix

	// Status reports health for telemetry, including the selected flag which
	// the Bank owns rather than the receiver.
	Status() ReceiverStatus
}

// Camera is the USB camera.
type Camera interface {
	// Capture grabs one full-resolution JPEG and writes it to the data
	// directory. The write is atomic, so a Photo returned here is complete.
	Capture(ctx context.Context) (*Photo, error)
	State() SubsystemState
	Device() string
	PhotosTaken() uint64
}

// Sdr is the Ettus B200mini acquisition program.
type Sdr interface {
	// Params reads the current parameters.json.
	Params(ctx context.Context) (SdrParams, error)
	// SetParams applies a partial update. Absent fields are left alone, so a
	// caller changing the PRF does not also zero the bandwidth.
	SetParams(ctx context.Context, p SdrParamsPatch) error

	// Connect starts ./connect detached, with output to a timestamped log.
	// It refuses to start a second acquisition while one is running and says
	// which PID is holding it.
	Connect(ctx context.Context) error
	// Running reports whether an acquisition is in flight.
	Running() bool
	PID() int64
	LastLog() string
	LastOutput() string

	// Probe returns uhd_usrp_probe output verbatim.
	Probe(ctx context.Context) (string, error)
	// ResetUSB power-cycles the SDR's USB port.
	ResetUSB(ctx context.Context) error

	State() SubsystemState
}

// SdrParams is the full contents of parameters/params.json.
//
// This is a frozen contract with the vendored C++ program. A value it cannot
// use is a bricked SDR, not a runtime error, which is why SetParams validates
// before it writes.
type SdrParams struct {
	PRFHz            float64 `json:"PRF"`
	SampleRateHz     float64 `json:"FS"`
	TxFreqHz         float64 `json:"TX_FREQ"`
	NormalizedGainTx float64 `json:"NORMALIZED_GAIN_TX"`
	NormalizedGainRx float64 `json:"NORMALIZED_GAIN_RX"`
	PulseDurationS   float64 `json:"PULSE_DURATION"`
	BandwidthHz      float64 `json:"BW"`
	SessionDurationS uint32  `json:"SESSION_DURATION"`
	TxAntenna        string  `json:"TX_ANTENNA"`
	RxAntenna        string  `json:"RX_ANTENNA"`
}

// SdrParamsPatch is a partial update. A nil pointer means "leave it alone".
//
// The distinction from SdrParams is the whole point of using optional fields on
// the wire: a PRF of zero is not a request to set the PRF to zero.
type SdrParamsPatch struct {
	PRFHz            *float64
	SampleRateHz     *float64
	TxFreqHz         *float64
	NormalizedGainTx *float64
	NormalizedGainRx *float64
	PulseDurationS   *float64
	BandwidthHz      *float64
	SessionDurationS *uint32
}

// LinkShaper is the kernel traffic control interface.
//
// Set never returns an error that stops the caller. tc fails for dozens of
// reasons unrelated to our logic -- sudo without a password, tc not installed,
// wrong interface, kernel without HTB -- and none of them justify taking down a
// telemetry server. Every method returns (ok, message) so the reason reaches the
// operator and the rest of the system keeps running.
type LinkShaper interface {
	// Apply installs the HTB hierarchy and the flower filter.
	Apply(ctx context.Context, device string, rateKbps uint32) (bool, string)
	// Active reports whether shaping is currently in force.
	Active() bool
	// Status reports what the shaper believes, for telemetry.
	Status() LinkStatus
	// QdiscPresent reads the device back from the kernel. The absence of this
	// read-back is why an unnoticed classification failure can persist: the
	// commands succeed and the traffic still lands in the wrong class.
	QdiscPresent(ctx context.Context, device string) (bool, error)
}

// Shutdown releases a resource. Every long-lived component implements it so the
// composition root can unwind in reverse order without type-switching.
type Shutdown interface {
	Shutdown(ctx context.Context) error
}
