// Package command dispatches Ground Station commands to subsystems.
//
// One `handle` function, one place where a command becomes an action. The
// transport calls nothing else, which is what makes the control path auditable:
// every consequence of a button press passes through here.
package command

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"google.golang.org/protobuf/proto"

	rocsarv1 "github.com/rocsar/obc/api/rocsar/v1"
	"github.com/rocsar/obc/internal/domain"
	"github.com/rocsar/obc/internal/gnss"
	"github.com/rocsar/obc/internal/qos"
)

// Dispatcher routes commands.
//
// Everything it holds is an interface from internal/domain, so this package is
// the whole surface between the wire and the hardware.

// maxCommandOutput caps what a command may put in a reply message.
//
// Artefact bytes go over HTTP, not in the ACK, so this is not about size limits
// on the protocol -- it is that an ACK is a human-readable line and a runaway
// command output should not become a multi-megabyte string on a 115 kbit/s link.
// Truncated loudly rather than silently, because a partial result that looks
// complete is worse than a short one.
const maxCommandOutput = 8192

type Dispatcher struct {
	pico    domain.Pico
	gnss    *gnss.Bank
	camera  domain.Camera
	sdr     domain.Sdr
	link    domain.LinkShaper
	heaters *HeaterKeeper

	log *slog.Logger

	// serial numbers successful responses, so a client can tell a repeat from a
	// fresh answer even when two buttons produce identical bytes.
	mu   sync.Mutex
	seen map[string]uint64
}

// Deps are the subsystems a Dispatcher needs.
//
// They are interfaces rather than concrete types so the dispatcher can be tested
// with doubles, and so the composition root is the only place that decides which
// implementation is real.
type Deps struct {
	Pico    domain.Pico
	GNSS    *gnss.Bank
	Camera  domain.Camera
	SDR     domain.Sdr
	Link    domain.LinkShaper
	Heaters *HeaterKeeper
	Log     *slog.Logger
}

// New returns a dispatcher.
func New(d Deps) *Dispatcher {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	return &Dispatcher{
		pico:    d.Pico,
		gnss:    d.GNSS,
		camera:  d.Camera,
		sdr:     d.SDR,
		link:    d.Link,
		heaters: d.Heaters,
		log:     d.Log,
		seen:    make(map[string]uint64),
	}
}

// Run starts the host half of the heater dead-man and blocks until ctx is done.
//
// The firmware turns anything it has not heard about off on its own; this is
// what "the operator asked for heat, keep it" means after the button has been
// released. The composition root starts it in its own goroutine.
func (d *Dispatcher) Run(ctx context.Context) {
	if d.heaters == nil {
		return
	}
	d.heaters.Run(ctx)
}

// Handle dispatches one command and returns its response.
//
// It never panics and never returns nil. A command that cannot be dispatched
// still gets a response saying so: the alternative is a client blocked in a
// send until its timeout expires, with nothing to show for it.
func (d *Dispatcher) Handle(ctx context.Context, req *rocsarv1.CommandRequest) *rocsarv1.CommandResponse {
	if req == nil {
		return fail("", rocsarv1.ErrorCode_ERROR_INVALID_COMMAND, "no command")
	}
	if req.GetPayload() == nil {
		return fail(req.GetRequestId(), rocsarv1.ErrorCode_ERROR_INVALID_COMMAND,
			"the command has no payload set")
	}

	d.log.Info("command", "request_id", req.GetRequestId(), "command", commandName(req))

	switch p := req.GetPayload().(type) {
	case *rocsarv1.CommandRequest_Pico:
		return d.picoCommand(ctx, req.GetRequestId(), p.Pico)
	case *rocsarv1.CommandRequest_SdrConnect:
		return d.sdrConnect(ctx, req.GetRequestId())
	case *rocsarv1.CommandRequest_SdrSetParams:
		return d.sdrSetParams(ctx, req.GetRequestId(), p.SdrSetParams)
	case *rocsarv1.CommandRequest_SdrResetUsb:
		return d.sdrResetUSB(ctx, req.GetRequestId())
	case *rocsarv1.CommandRequest_SdrProbe:
		return d.sdrProbe(ctx, req.GetRequestId())
	case *rocsarv1.CommandRequest_SdrGetParams:
		return d.sdrGetParams(ctx, req.GetRequestId())
	case *rocsarv1.CommandRequest_TakePhoto:
		return d.takePhoto(ctx, req.GetRequestId())
	case *rocsarv1.CommandRequest_GnssSelect:
		return d.gnssSelect(req.GetRequestId(), p.GnssSelect)
	case *rocsarv1.CommandRequest_LinkSetLimit:
		return d.linkSetLimit(ctx, req.GetRequestId(), p.LinkSetLimit)
	case *rocsarv1.CommandRequest_QueryStatus:
		return ok(req.GetRequestId(), "status is reported in the telemetry frame")
	case *rocsarv1.CommandRequest_SystemReset:
		// Deliberately not implemented. A "reset" that is merely a restart of
		// our own subsystems is not what an operator pressing it during a flight
		// expects, and a real reset would stop the control path. It stays
		// visible as UNSUPPORTED rather than quietly doing nothing.
		return fail(req.GetRequestId(), rocsarv1.ErrorCode_ERROR_UNSUPPORTED,
			"system_reset is not implemented; it would interrupt the control path and no operator should need it mid-flight")
	default:
		return fail(req.GetRequestId(), rocsarv1.ErrorCode_ERROR_INVALID_COMMAND,
			fmt.Sprintf("unhandled command %T", p))
	}
}

// commandName is for the log only. It switches on the same oneof as Handle, so
// a new command that is not dispatched also does not log sensibly -- which is
// the point, because that shows up in review.
func commandName(req *rocsarv1.CommandRequest) string {
	switch req.GetPayload().(type) {
	case *rocsarv1.CommandRequest_Pico:
		return "pico"
	case *rocsarv1.CommandRequest_SdrConnect:
		return "sdr.connect"
	case *rocsarv1.CommandRequest_SdrSetParams:
		return "sdr.set_params"
	case *rocsarv1.CommandRequest_SdrResetUsb:
		return "sdr.reset_usb"
	case *rocsarv1.CommandRequest_SdrProbe:
		return "sdr.probe"
	case *rocsarv1.CommandRequest_SdrGetParams:
		return "sdr.get_params"
	case *rocsarv1.CommandRequest_TakePhoto:
		return "camera.take_photo"
	case *rocsarv1.CommandRequest_GnssSelect:
		return "gnss.select"
	case *rocsarv1.CommandRequest_LinkSetLimit:
		return "link.set_limit"
	case *rocsarv1.CommandRequest_QueryStatus:
		return "system.query_status"
	case *rocsarv1.CommandRequest_SystemReset:
		return "system.reset"
	default:
		return "unknown"
	}
}

// ---------------------------------------------------------------------------
// Pico
// ---------------------------------------------------------------------------

func (d *Dispatcher) picoCommand(ctx context.Context, requestID string, cmd *rocsarv1.PicoCommand) *rocsarv1.CommandResponse {
	if d.pico == nil {
		return fail(requestID, rocsarv1.ErrorCode_ERROR_NOT_CONNECTED, "no flight controller is configured")
	}
	if cmd == nil {
		return fail(requestID, rocsarv1.ErrorCode_ERROR_INVALID_COMMAND, "no Pico command")
	}

	// The off side of the heater asymmetry is settled before the link is
	// consulted: an operator who asked for OFF gets its intent recorded even if
	// the board is unreachable, because a keep-alive the operator has cancelled
	// must not resurrect a heater the moment the link returns. An ON waits for
	// the acknowledgement below -- an intent that never arrived must not be
	// revived by the keeper either. See HeaterKeeper.
	if h := cmd.GetHeater(); h != nil && !h.GetState() {
		d.noteHeaterIntent(h.GetHeaterId(), false)
	}

	if !d.pico.Connected() {
		// Distinct from a timeout: this is "there is nothing to talk to", which
		// is a cable or an unplugged board, not a wedged one.
		return fail(requestID, rocsarv1.ErrorCode_ERROR_NOT_CONNECTED, "the flight controller is not connected")
	}

	var (
		ack *domain.Ack
		err error
	)

	switch p := cmd.GetPayload().(type) {
	case *rocsarv1.PicoCommand_SetTarget:
		ack, err = d.pico.SetTarget(ctx, float64(p.SetTarget.GetTargetHeadingDeg()))
	case *rocsarv1.PicoCommand_Jog:
		ack, err = d.pico.Jog(ctx, p.Jog.GetServoId(), p.Jog.GetTick())
	case *rocsarv1.PicoCommand_Zero:
		ack, err = d.pico.Zero(ctx, p.Zero.GetServoId())
	case *rocsarv1.PicoCommand_Mount:
		ack, err = d.pico.Mount(ctx, p.Mount.GetServoId(), float64(p.Mount.GetOffsetDeg()))
	case *rocsarv1.PicoCommand_Dir:
		ack, err = d.pico.SetDirection(ctx, p.Dir.GetServoId(), float64(p.Dir.GetMultiplier()))
	case *rocsarv1.PicoCommand_Heater:
		ack, err = d.pico.SetHeater(ctx, p.Heater.GetHeaterId(), p.Heater.GetState())
		// Only a successful ON enters the keeper: the keep-alive may re-assert
		// exactly what the board has confirmed it received. OFF was already
		// noted above, before the link was consulted.
		if p.Heater.GetState() && err == nil && ack != nil && ack.Success {
			d.noteHeaterIntent(p.Heater.GetHeaterId(), true)
		}
	case *rocsarv1.PicoCommand_Stop:
		ack, err = d.pico.Stop(ctx, p.Stop.GetServoId())
	case *rocsarv1.PicoCommand_StatusRequest:
		// Already answered by the flight controller's own telemetry; nothing to
		// wait for beyond the link being open.
		return ok(requestID, "the flight controller reports on its own 50 Hz tick")
	default:
		return fail(requestID, rocsarv1.ErrorCode_ERROR_INVALID_COMMAND,
			fmt.Sprintf("unhandled Pico command %T", p))
	}

	if err != nil {
		return fail(requestID, classify(err), err.Error())
	}
	if !ack.Success {
		return fail(requestID, rocsarv1.ErrorCode(ack.Error),
			fmt.Sprintf("the flight controller refused the command (%s)", ack.Error))
	}

	return ok(requestID, "acknowledged by the flight controller")
}

// noteHeaterIntent forwards the operator's word to the keeper.
//
// A dispatcher built without a keeper (a test, or a build that does not run
// the keep-alive) simply has no opinion to record.
func (d *Dispatcher) noteHeaterIntent(heaterID uint32, on bool) {
	if d.heaters == nil {
		return
	}
	d.heaters.Note(heaterID, on)
}

// classify maps a dispatch error onto the wire enum.
//
// The distinction between "not connected" and everything else is load-bearing:
// one is a cable, the other is a timeout on a device that is present but not
// answering.
func classify(err error) rocsarv1.ErrorCode {
	switch {
	case err == nil:
		return rocsarv1.ErrorCode_ERROR_NONE
	case isTimeout(err):
		return rocsarv1.ErrorCode_ERROR_NOT_CONNECTED
	case containsAny(err, "out of range", "must be", "does not exist"):
		return rocsarv1.ErrorCode_ERROR_INVALID_PARAMETER
	case containsAny(err, "no such receiver", "unknown receiver"):
		return rocsarv1.ErrorCode_ERROR_INVALID_PARAMETER
	case containsAny(err, "already running"):
		return rocsarv1.ErrorCode_ERROR_UNSUPPORTED
	default:
		return rocsarv1.ErrorCode_ERROR_HARDWARE_FAULT
	}
}

// ---------------------------------------------------------------------------
// SDR
// ---------------------------------------------------------------------------

func (d *Dispatcher) sdrConnect(ctx context.Context, requestID string) *rocsarv1.CommandResponse {
	if d.sdr == nil {
		return fail(requestID, rocsarv1.ErrorCode_ERROR_NOT_CONNECTED, "no SDR is configured")
	}
	if err := d.sdr.Connect(ctx); err != nil {
		return fail(requestID, classify(err), err.Error())
	}
	return ok(requestID, fmt.Sprintf("acquisition started, pid %d, log %s",
		d.sdr.PID(), d.sdr.LastLog()))
}

func (d *Dispatcher) sdrSetParams(ctx context.Context, requestID string, cmd *rocsarv1.SdrSetParamsCommand) *rocsarv1.CommandResponse {
	if d.sdr == nil {
		return fail(requestID, rocsarv1.ErrorCode_ERROR_NOT_CONNECTED, "no SDR is configured")
	}
	p := cmd.GetParams()
	if p == nil {
		return fail(requestID, rocsarv1.ErrorCode_ERROR_INVALID_PARAMETER, "no parameters supplied")
	}

	// Every field is optional and absent means "leave it alone". A patch with
	// nothing set is rejected rather than treated as a no-op, because an
	// operator who pressed the button expects a change and a silent success is
	// indistinguishable from a broken button.
	if p.PrfHz == nil && p.SampleRateHz == nil && p.TxFreqHz == nil &&
		p.NormalizedGainTx == nil && p.NormalizedGainRx == nil &&
		p.BandwidthHz == nil && p.SessionDurationS == nil &&
		p.TMinUs == nil && p.TMaxUs == nil && p.StartOffsetS == nil &&
		p.TxAntenna == nil && p.RxAntenna == nil {
		return fail(requestID, rocsarv1.ErrorCode_ERROR_INVALID_PARAMETER,
			"no parameters were set; a partial update must name at least one")
	}

	patch := domain.SdrParamsPatch{
		PRFHz:            p.PrfHz,
		SampleRateHz:     p.SampleRateHz,
		TxFreqHz:         p.TxFreqHz,
		NormalizedGainTx: p.NormalizedGainTx,
		NormalizedGainRx: p.NormalizedGainRx,
		BandwidthHz:      p.BandwidthHz,
		SweepMinUs:       p.TMinUs,
		SweepMaxUs:       p.TMaxUs,
		StartOffsetS:     p.StartOffsetS,
	}
	if p.SessionDurationS != nil {
		v := p.GetSessionDurationS()
		patch.SessionDurationS = &v
	}
	// Strings, so they cannot be aliased from the message the way the *float64
	// fields are: GetTxAntenna returns the receiver's own field, and taking its
	// address would hand the patch a pointer into a message this frame does not
	// own. A local copy per field is the same shape as the SessionDurationS case
	// above and for the same reason.
	if p.TxAntenna != nil {
		v := p.GetTxAntenna()
		patch.TxAntenna = &v
	}
	if p.RxAntenna != nil {
		v := p.GetRxAntenna()
		patch.RxAntenna = &v
	}

	if err := d.sdr.SetParams(ctx, patch); err != nil {
		return fail(requestID, classify(err), err.Error())
	}
	return ok(requestID, "parameters updated")
}

func (d *Dispatcher) sdrResetUSB(ctx context.Context, requestID string) *rocsarv1.CommandResponse {
	if d.sdr == nil {
		return fail(requestID, rocsarv1.ErrorCode_ERROR_NOT_CONNECTED, "no SDR is configured")
	}
	if err := d.sdr.ResetUSB(ctx); err != nil {
		return fail(requestID, classify(err), err.Error())
	}
	return ok(requestID, "the SDR's USB port was power-cycled")
}

func (d *Dispatcher) sdrProbe(ctx context.Context, requestID string) *rocsarv1.CommandResponse {
	if d.sdr == nil {
		return fail(requestID, rocsarv1.ErrorCode_ERROR_NOT_CONNECTED, "no SDR is configured")
	}
	out, err := d.sdr.Probe(ctx)
	// The output is returned verbatim on success. An operator diagnosing
	// hardware needs the line that says what is wrong, not a summary.
	if err != nil {
		return fail(requestID, classify(err), err.Error())
	}
	if len(out) > maxCommandOutput {
		out = out[:maxCommandOutput] + "\n[truncated]"
	}
	return ok(requestID, out)
}

// sdrGetParams reads parameters/params.json and returns it as JSON.
//
// The read side of sdr_set_params: the Ground Station shows current values as
// placeholder hints while blank inputs keep meaning "leave alone". The JSON
// shape is domain.SdrParams marshalled as-is, so the keys are the params.json
// keys (PRF, FS, TX_FREQ, ...) the operator already knows, not a second
// vocabulary. Like sdr_probe, the output is verbatim; unlike sdr_probe it
// cannot overflow the message cap, but the truncation guard stays because a
// params file is operator-edited and unbounded in principle.
func (d *Dispatcher) sdrGetParams(ctx context.Context, requestID string) *rocsarv1.CommandResponse {
	if d.sdr == nil {
		return fail(requestID, rocsarv1.ErrorCode_ERROR_NOT_CONNECTED, "no SDR is configured")
	}
	params, err := d.sdr.Params(ctx)
	if err != nil {
		return fail(requestID, classify(err), err.Error())
	}
	out, err := json.Marshal(params)
	if err != nil {
		return fail(requestID, rocsarv1.ErrorCode_ERROR_INTERNAL,
			fmt.Sprintf("could not encode parameters: %v", err))
	}
	if len(out) > maxCommandOutput {
		out = out[:maxCommandOutput]
	}
	return ok(requestID, string(out))
}

// ---------------------------------------------------------------------------
// Camera
// ---------------------------------------------------------------------------

// takePhoto returns metadata, never bytes.
//
// The air link is 115 kbit/s. A multi-megabyte JPEG in a command response would
// go through the priority class and starve command traffic; the Ground Station
// fetches it over the bulk port instead. See ARCHITECTURE.md 5.1.
func (d *Dispatcher) takePhoto(ctx context.Context, requestID string) *rocsarv1.CommandResponse {
	if d.camera == nil {
		return fail(requestID, rocsarv1.ErrorCode_ERROR_NOT_CONNECTED, "no camera is configured")
	}
	photo, err := d.camera.Capture(ctx)
	if err != nil {
		return fail(requestID, classify(err), err.Error())
	}
	if photo == nil {
		return fail(requestID, rocsarv1.ErrorCode_ERROR_INTERNAL,
			"the camera reported success but produced no photograph")
	}

	resp := ok(requestID, fmt.Sprintf("captured %s (%d bytes); fetch it over HTTP",
		photo.Name, photo.SizeBytes))
	resp.ArtefactName = proto.String(photo.Path)
	resp.ArtefactSizeBytes = proto.Uint64(photo.SizeBytes)
	resp.ArtefactKind = proto.String(photo.Kind)
	return resp
}

// ---------------------------------------------------------------------------
// GNSS
// ---------------------------------------------------------------------------

func (d *Dispatcher) gnssSelect(requestID string, cmd *rocsarv1.GnssSelectCommand) *rocsarv1.CommandResponse {
	if d.gnss == nil {
		return fail(requestID, rocsarv1.ErrorCode_ERROR_NOT_CONNECTED, "no GNSS receivers are configured")
	}

	var err error
	if cmd.GetRotate() {
		err = d.gnss.Rotate()
	} else {
		err = d.gnss.Select(int(cmd.GetReceiverId()))
	}
	if err != nil {
		return fail(requestID, rocsarv1.ErrorCode_ERROR_INVALID_PARAMETER, err.Error())
	}
	return ok(requestID, fmt.Sprintf("receiver %d is now trusted", d.gnss.SelectedID()))
}

// ---------------------------------------------------------------------------
// Link
// ---------------------------------------------------------------------------

func (d *Dispatcher) linkSetLimit(ctx context.Context, requestID string, cmd *rocsarv1.LinkSetLimitCommand) *rocsarv1.CommandResponse {
	if d.link == nil {
		return fail(requestID, rocsarv1.ErrorCode_ERROR_UNSUPPORTED, "no link shaper is configured")
	}

	rate := cmd.GetRateKbps()
	if rate == 0 {
		return fail(requestID, rocsarv1.ErrorCode_ERROR_INVALID_PARAMETER,
			"rate_kbps must be greater than zero")
	}

	// Re-validated here because the operator's number goes straight into tc, and
	// a rate below the priority floor leaves telemetry with no guaranteed
	// bandwidth at all -- which is the opposite of what "limit the link" means.
	if prio := qos.PriorityKbps(rate); prio == 0 {
		return fail(requestID, rocsarv1.ErrorCode_ERROR_INVALID_PARAMETER,
			fmt.Sprintf("a limit of %d kbit/s leaves no priority bandwidth for telemetry", rate))
	}

	device := d.link.Status().Device
	if device == "" {
		device = defaultDevice
	}
	applied, reason := d.link.Apply(ctx, device, rate)

	// Reported as a failure with the real reason, and the telemetry link is
	// unaffected either way: tc failing is not a reason to stop controlling the
	// vehicle. See ARCHITECTURE.md 8.
	if !applied {
		return fail(requestID, rocsarv1.ErrorCode_ERROR_HARDWARE_FAULT, reason)
	}
	return ok(requestID, fmt.Sprintf("link limit set to %d kbit/s on %s", rate, device))
}

// defaultDevice is used when the shaper has not been applied yet and therefore
// does not know its device.
const defaultDevice = "eth0"

// ---------------------------------------------------------------------------

func ok(requestID, message string) *rocsarv1.CommandResponse {
	return &rocsarv1.CommandResponse{
		RequestId: requestID,
		Success:   true,
		Error:     rocsarv1.ErrorCode_ERROR_NONE,
		Message:   message,
	}
}

func fail(requestID string, code rocsarv1.ErrorCode, message string) *rocsarv1.CommandResponse {
	return &rocsarv1.CommandResponse{
		RequestId: requestID,
		Success:   false,
		Error:     code,
		Message:   message,
	}
}

func containsAny(err error, subs ...string) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, s := range subs {
		if len(s) > 0 && strings.Contains(msg, strings.ToLower(s)) {
			return true
		}
	}
	return false
}

func isTimeout(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "timeout") || strings.Contains(msg, "not acknowledged") ||
		strings.Contains(msg, "timed out")
}
