package client

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	rocsarv1 "github.com/rocsar/obc/api/rocsar/v1"
)

// Request is a Ground Station command, Response its answer.
//
// Aliases, not wrappers: cmd/gs works with these types pervasively (submit,
// match, report) but must not import the generated bindings -- the layering
// test forbids it, on purpose. An alias lets a consumer name the type without
// importing the package it comes from, which is exactly the seam a Wails
// binding layer needs: it touches every command and owns none of the schema.
type (
	Request  = rocsarv1.CommandRequest
	Response = rocsarv1.CommandResponse
)

// SdrParamsPatch is a partial SDR parameter update. A nil pointer means
// "leave it alone" -- it does not mean zero, and a zero would brick the SDR
// rather than fail cleanly, which is why this struct cannot be floats.
//
// It mirrors the wire's SdrParams field for field without importing domain:
// domain.SdrParamsPatch describes what the OBC's service accepts, and this
// describes what the console sends. The two agree by construction here, in the
// one builder below, rather than by sharing a type across the link. An all-nil
// patch is refused, mirroring the dispatcher: an operator who pressed the
// button expects a change, and a silent success is indistinguishable from a
// broken button.
type SdrParamsPatch struct {
	PRFHz            *float64 `json:"prf_hz,omitempty"`
	SampleRateHz     *float64 `json:"sample_rate_hz,omitempty"`
	TxFreqHz         *float64 `json:"tx_freq_hz,omitempty"`
	NormalizedGainTx *float64 `json:"normalized_gain_tx,omitempty"`
	NormalizedGainRx *float64 `json:"normalized_gain_rx,omitempty"`
	BandwidthHz      *float64 `json:"bandwidth_hz,omitempty"`
	SessionDurationS *uint32  `json:"session_duration_s,omitempty"`
}

// BuildSetParamsRequest turns a typed patch into a CommandRequest. Typed,
// unlike BuildRequests, because the only caller is a window with numeric
// fields -- parsing floats to strings to re-parse them would be validation
// theatre twice over.
func BuildSetParamsRequest(patch SdrParamsPatch) (*Request, error) {
	if patch.PRFHz == nil && patch.SampleRateHz == nil && patch.TxFreqHz == nil &&
		patch.NormalizedGainTx == nil && patch.NormalizedGainRx == nil &&
		patch.BandwidthHz == nil && patch.SessionDurationS == nil {
		return nil, fmt.Errorf("no parameters were set; a partial update must name at least one")
	}
	params := &rocsarv1.SdrParams{}
	if patch.PRFHz != nil {
		v := *patch.PRFHz
		params.PrfHz = &v
	}
	if patch.SampleRateHz != nil {
		v := *patch.SampleRateHz
		params.SampleRateHz = &v
	}
	if patch.TxFreqHz != nil {
		v := *patch.TxFreqHz
		params.TxFreqHz = &v
	}
	if patch.NormalizedGainTx != nil {
		v := *patch.NormalizedGainTx
		params.NormalizedGainTx = &v
	}
	if patch.NormalizedGainRx != nil {
		v := *patch.NormalizedGainRx
		params.NormalizedGainRx = &v
	}
	if patch.BandwidthHz != nil {
		v := *patch.BandwidthHz
		params.BandwidthHz = &v
	}
	if patch.SessionDurationS != nil {
		v := *patch.SessionDurationS
		params.SessionDurationS = &v
	}
	return &rocsarv1.CommandRequest{
		RequestId: NewRequestID("sdr-set-params"),
		Payload: &rocsarv1.CommandRequest_SdrSetParams{
			SdrSetParams: &rocsarv1.SdrSetParamsCommand{Params: params},
		},
	}, nil
}

// requestPrefix is the first field of every request_id this package generates.
//
// It was "gs-cli-" while this code lived in tools/gs_cli. It is neutral now
// because two consoles share it, and a request_id is the only trace either of
// them leaves in the OBC's log -- a GUI command logged as "gs-cli" is a small
// lie that costs an afternoon the first time somebody greps for it.
//
// The OBC only echoes the id, so nothing depends on the shape.
const requestPrefix = "rocsar-"

// NewRequestID returns a unique request id for one command.
//
// Uniqueness is load-bearing: the reply is matched on it, so two commands in
// flight cannot be told apart when their replies arrive. A counter would be
// smaller and would collide across a restart, which is exactly when a stale
// reply is most likely to be sitting in a buffer.
func NewRequestID(suffix string) string {
	return fmt.Sprintf("%s%d-%s", requestPrefix, time.Now().UnixNano(), suffix)
}

// BuildRequests turns a command name and its arguments into CommandRequests.
//
// Arguments are validated here rather than by the OBC, because the alternative
// is a command that reaches the servos with a nonsense value and is refused after
// the fact. These run in the same process as the operator's intent, so a mistake
// is caught before it is sent.
//
// The names are the ones gs_cli has always used on the command line, and they are
// kept rather than renamed because they are the vocabulary an operator already
// knows. cmd/gs shows the same list in a window; the two cannot drift, because
// this is the one implementation of it.
//
// A slice, because some operations are genuinely more than one command -- `zero`
// centres each axis separately -- and pretending otherwise would either send one
// command and quietly do half the work, or smuggle a loop through a builder whose
// name says otherwise.
func BuildRequests(name string, args []string) ([]*rocsarv1.CommandRequest, error) {
	req := &rocsarv1.CommandRequest{RequestId: NewRequestID(name)}

	need := func(n int) error {
		if len(args) < n {
			return fmt.Errorf("%s needs %d argument(s), got %d", name, n, len(args))
		}
		return nil
	}
	f64 := func(i int) (float64, error) {
		v, err := strconv.ParseFloat(args[i], 64)
		if err != nil {
			return 0, fmt.Errorf("argument %d (%q) is not a number", i+1, args[i])
		}
		return v, nil
	}
	u32 := func(i int) (uint32, error) {
		v, err := strconv.ParseUint(args[i], 10, 32)
		if err != nil {
			return 0, fmt.Errorf("argument %d (%q) is not a whole number", i+1, args[i])
		}
		return uint32(v), nil
	}

	switch name {
	case "query":
		req.Payload = &rocsarv1.CommandRequest_QueryStatus{
			QueryStatus: &rocsarv1.QueryStatusCommand{},
		}

	case "photo":
		req.Payload = &rocsarv1.CommandRequest_TakePhoto{
			TakePhoto: &rocsarv1.TakePhotoCommand{},
		}

	case "gnss":
		if err := need(1); err != nil {
			return nil, err
		}
		id, err := u32(0)
		if err != nil {
			return nil, err
		}
		if id < 1 {
			return nil, errors.New("receiver ids start at 1")
		}
		req.Payload = &rocsarv1.CommandRequest_GnssSelect{
			GnssSelect: &rocsarv1.GnssSelectCommand{ReceiverId: id},
		}

	case "gnss-rotate":
		// Trust the next receiver instead of naming one. The wire carries a
		// Rotate flag beside the id for exactly this; the CLI never needed it
		// spelled out, but a window's "next receiver" button does.
		if len(args) != 0 {
			return nil, fmt.Errorf("gnss-rotate takes no arguments, got %d", len(args))
		}
		req.Payload = &rocsarv1.CommandRequest_GnssSelect{
			GnssSelect: &rocsarv1.GnssSelectCommand{Rotate: true},
		}

	case "heading":
		if err := need(1); err != nil {
			return nil, err
		}
		deg, err := f64(0)
		if err != nil {
			return nil, err
		}
		req.Payload = &rocsarv1.CommandRequest_Pico{
			Pico: &rocsarv1.PicoCommand{
				Payload: &rocsarv1.PicoCommand_SetTarget{
					SetTarget: &rocsarv1.SetTargetCommand{TargetHeadingDeg: float32(deg)},
				},
			},
		}

	case "jog":
		if err := need(2); err != nil {
			return nil, err
		}
		id, err := u32(0)
		if err != nil {
			return nil, err
		}
		tick, err := u32(1)
		if err != nil {
			return nil, err
		}
		// The ST3215 tick range is 0..4095. Out of range is refused by the firmware,
		// but refusing it here means the operator finds out before it is sent -- and
		// nothing travels to the flight controller at all, which matters because a jog
		// moves an antenna.
		if tick > 4095 {
			return nil, fmt.Errorf("tick %d is outside the ST3215 range 0..4095", tick)
		}
		req.Payload = &rocsarv1.CommandRequest_Pico{
			Pico: &rocsarv1.PicoCommand{
				Payload: &rocsarv1.PicoCommand_Jog{
					Jog: &rocsarv1.JogCommand{ServoId: id, Tick: tick},
				},
			},
		}

	case "zero":
		// Both axes unless one is named. Each is its own command because the
		// PicoCommand oneof carries a single servo_id.
		ids := []uint32{1, 2}
		if len(args) == 1 {
			v, err := u32(0)
			if err != nil {
				return nil, err
			}
			ids = []uint32{v}
		}
		out := make([]*rocsarv1.CommandRequest, 0, len(ids))
		for _, id := range ids {
			out = append(out, &rocsarv1.CommandRequest{
				RequestId: NewRequestID(fmt.Sprintf("zero-%d", id)),
				Payload: &rocsarv1.CommandRequest_Pico{
					Pico: &rocsarv1.PicoCommand{
						Payload: &rocsarv1.PicoCommand_Zero{
							Zero: &rocsarv1.ZeroCommand{ServoId: id},
						},
					},
				},
			})
		}
		return out, nil

	case "mount":
		if err := need(2); err != nil {
			return nil, err
		}
		id, err := u32(0)
		if err != nil {
			return nil, err
		}
		deg, err := f64(1)
		if err != nil {
			return nil, err
		}
		req.Payload = &rocsarv1.CommandRequest_Pico{
			Pico: &rocsarv1.PicoCommand{
				Payload: &rocsarv1.PicoCommand_Mount{
					Mount: &rocsarv1.MountCommand{ServoId: id, OffsetDeg: float32(deg)},
				},
			},
		}

	case "dir":
		if err := need(2); err != nil {
			return nil, err
		}
		id, err := u32(0)
		if err != nil {
			return nil, err
		}
		m := int32(-1)
		switch args[1] {
		case "1", "+1":
			m = 1
		case "-1":
			m = -1
		default:
			return nil, fmt.Errorf("direction is %q, want +1 or -1", args[1])
		}
		req.Payload = &rocsarv1.CommandRequest_Pico{
			Pico: &rocsarv1.PicoCommand{
				Payload: &rocsarv1.PicoCommand_Dir{
					Dir: &rocsarv1.DirCommand{ServoId: id, Multiplier: float32(m)},
				},
			},
		}

	case "heater":
		if err := need(2); err != nil {
			return nil, err
		}
		id, err := u32(0)
		if err != nil {
			return nil, err
		}
		if id < 1 || id > 2 {
			return nil, errors.New("heater id is 1 or 2")
		}
		on, err := parseOnOff(args[1])
		if err != nil {
			return nil, err
		}
		req.Payload = &rocsarv1.CommandRequest_Pico{
			Pico: &rocsarv1.PicoCommand{
				Payload: &rocsarv1.PicoCommand_Heater{
					Heater: &rocsarv1.HeaterCommand{HeaterId: id, State: on},
				},
			},
		}

	case "stop":
		if err := need(1); err != nil {
			return nil, err
		}
		id := uint32(0)
		if !strings.EqualFold(args[0], "all") {
			v, err := u32(0)
			if err != nil {
				return nil, err
			}
			id = v
		}
		req.Payload = &rocsarv1.CommandRequest_Pico{
			Pico: &rocsarv1.PicoCommand{
				Payload: &rocsarv1.PicoCommand_Stop{Stop: &rocsarv1.StopCommand{ServoId: id}},
			},
		}

	case "pico-status":
		req.Payload = &rocsarv1.CommandRequest_Pico{
			Pico: &rocsarv1.PicoCommand{
				Payload: &rocsarv1.PicoCommand_StatusRequest{
					StatusRequest: &rocsarv1.StatusRequestCommand{},
				},
			},
		}

	case "sdr-probe":
		req.Payload = &rocsarv1.CommandRequest_SdrProbe{SdrProbe: &rocsarv1.SdrProbeCommand{}}

	case "sdr-connect":
		req.Payload = &rocsarv1.CommandRequest_SdrConnect{SdrConnect: &rocsarv1.SdrConnectCommand{}}

	case "sdr-reset-usb":
		req.Payload = &rocsarv1.CommandRequest_SdrResetUsb{SdrResetUsb: &rocsarv1.SdrResetUSBCommand{}}

	case "link":
		if err := need(1); err != nil {
			return nil, err
		}
		kbit, err := u32(0)
		if err != nil {
			return nil, err
		}
		if kbit == 0 {
			return nil, errors.New("link rate must be greater than zero")
		}
		req.Payload = &rocsarv1.CommandRequest_LinkSetLimit{
			LinkSetLimit: &rocsarv1.LinkSetLimitCommand{RateKbps: kbit},
		}

	case "reboot":
		req.Payload = &rocsarv1.CommandRequest_SystemReset{SystemReset: &rocsarv1.SystemResetCommand{}}

	default:
		return nil, fmt.Errorf("unknown command %q; run `gs_cli commands`", name)
	}
	return []*rocsarv1.CommandRequest{req}, nil
}

// parseOnOff accepts the words an operator would type. "1" is not accepted as a
// synonym for on: `heater 1 1` reads ambiguously and means nothing useful.
func parseOnOff(s string) (bool, error) {
	switch strings.ToLower(s) {
	case "on", "true", "yes":
		return true, nil
	case "off", "false", "no":
		return false, nil
	}
	return false, fmt.Errorf("heater state is %q, want on or off", s)
}

// Names returns every command name BuildRequests accepts.
//
// One implementation, consumed by gs_cli's `commands` listing and by cmd/gs's
// command palette, so the two cannot disagree about what exists.
func Names() []string {
	return []string{
		"query", "photo", "gnss", "gnss-rotate", "heading", "jog", "zero", "mount", "dir",
		"heater", "stop", "pico-status", "sdr-probe", "sdr-connect",
		"sdr-reset-usb", "link", "reboot",
	}
}
