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

	// Telemetry returns the most recent frame and whether one has ever arrived.
	//
	// Polled, not pushed. A callback would run on the goroutine that owns the
	// serial port, so a listener that blocked would delay the next read and
	// eventually overrun the device's buffer. Telemetry is consumed at 1 Hz and
	// a frame is 20 ms old at worst, so the push buys nothing and costs a
	// lifetime of listener bookkeeping.
	Telemetry() (PicoTelemetry, bool)
}

// Ack is the flight controller's answer to one command.
type Ack struct {
	CommandSequence uint32
	Success         bool
	Error           ErrorCode
	At              time.Time
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

	// LastError is why the last acquisition is not running, or "" if it is
	// running or completed cleanly.
	//
	// Added because the field existed everywhere else and was populated
	// nowhere: SDRSnapshot and the wire both had a last_error, the encoder
	// copied it, and no provider ever filled it in, so an acquisition that had
	// crashed read as an idle SDR with a PID. A death the operator caused and a
	// death nobody caused are different facts and both are worth reporting.
	LastError() string

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
//
// Every key load_config() reads with j.at() is modelled here, plus
// PulseDurationS, which the program does NOT read: config.hpp has that line
// commented out, so it is carried as data and never offered as a control. The
// three keys below the antenna pair are the sweep window and the arming delay,
// and they were read by the program while being unreachable from any operator.
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
	// SweepWindowUs is [T_MIN_US, T_MAX_US]: the burst window the program
	// sweeps inside each PRI, in microseconds. T_MIN_US is 0 in the shipped
	// file, meaning the burst starts at the leading edge.
	SweepMinUs   float64 `json:"T_MIN_US"`
	SweepMaxUs   float64 `json:"T_MAX_US"`
	StartOffsetS float64 `json:"START_OFFSET_S"`
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
	BandwidthHz      *float64
	SessionDurationS *uint32
	TxAntenna        *string
	RxAntenna        *string
	SweepMinUs       *float64
	SweepMaxUs       *float64
	StartOffsetS     *float64
}

// LinkShaper is the kernel traffic control interface.
//
// Set never returns an error that stops the caller. Traffic control fails for
// dozens of reasons unrelated to our logic -- no CAP_NET_ADMIN, wrong interface,
// a kernel without HTB -- and none of them justify taking down a telemetry server.
// Every method returns (ok, message) so the reason reaches the operator and the
// rest of the system keeps running.
type LinkShaper interface {
	// Apply installs the HTB rate cap. It does not install a classification
	// filter: the one that used to be here classified nothing (see qos.Apply and
	// ARCHITECTURE.md 6.6), and the port has no method that could add one back.
	Apply(ctx context.Context, device string, rateKbps uint32) (bool, string)
	// Active reports whether shaping is currently in force.
	Active() bool
	// Status reports what the shaper believes, for telemetry.
	Status() LinkStatus
	// Verify re-reads the device and reports whether the cap is genuinely in
	// force, re-applying if it has gone.
	//
	// This exists because the in-process limiter that used to be the backstop is
	// gone: if the kernel tree is not on the device, the link is UNBOUNDED, and
	// nothing else in this program would know. Active and Status answer from
	// flags cached at Apply time, so without a read-back a qdisc removed by
	// NetworkManager or a hand-run tc is invisible while telemetry goes on
	// claiming protection.
	Verify(ctx context.Context) (bool, string)
	// QdiscPresent reads the device back from the kernel without changing
	// anything, for a caller that wants to look rather than to repair.
	QdiscPresent(ctx context.Context, device string) (bool, error)
}

// Shutdown releases a resource. Every long-lived component implements it so the
// composition root can unwind in reverse order without type-switching.
type Shutdown interface {
	Shutdown(ctx context.Context) error
}
