package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-zeromq/zmq4"
	"google.golang.org/protobuf/proto"

	rocsarv1 "github.com/rocsar/obc/api/rocsar/v1"
)

// client holds the three endpoints.
//
// Three sockets because the OBC has three: commands and telemetry are ZeroMQ and
// artefacts are HTTP, and the split is not negotiable. Artefact bytes are never
// carried in a command reply -- a command channel sized for kilobytes has no
// business carrying a photograph.
type client struct {
	control   string
	telemetry string
	http      string
	topic     string
}

// watch follows telemetry until interrupted.
func (c *client) watch(ctx context.Context, raw bool) error {
	s, err := c.subscribe()
	if err != nil {
		return err
	}
	defer s.close()

	fmt.Printf("following %s; Ctrl-C to stop\n\n", c.telemetry)
	return c.pump(ctx, s, raw, true)
}

// status prints one frame.
func (c *client) status(ctx context.Context, raw bool) error {
	s, err := c.subscribe()
	if err != nil {
		return err
	}
	defer s.close()

	return c.pump(ctx, s, raw, false)
}

func (c *client) subscribe() (*sub, error) {
	// A DEALER, never a REQ.
	//
	// The OBC side is a ROUTER and sends [identity, payload] -- no empty
	// delimiter. A REQ socket always inserts one, so the reply arrives one frame
	// late and the payload is read from the wrong place. This cost an afternoon
	// once: the OBC was dropping every command with "short router frame" while its
	// own tests passed, because the test client hand-rolled the envelope.
	ctx := context.Background()
	// The timeout is a construction option: this library has no per-call receive
	// timeout, which is why the OBC runs one goroutine per socket and closes the
	// socket to release a blocked receive.
	t := zmq4.NewSub(ctx, zmq4.WithTimeout(subTimeout))
	// Set the filter BEFORE dialling, so the subscription is in place before the
	// first frame can arrive. A SUB that matches nothing fails silently rather
	// than reporting a mismatch, so a filter set late loses frames invisibly.
	if err := t.SetOption(zmq4.OptionSubscribe, c.topic); err != nil {
		return nil, fmt.Errorf("subscribe to %q: %w", c.topic, err)
	}
	if err := t.Dial(c.telemetry); err != nil {
		return nil, fmt.Errorf("dial %s: %w", c.telemetry, err)
	}
	return &sub{sock: t}, nil
}

type sub struct{ sock zmq4.Socket }

// subTimeout bounds one receive. Ten seconds is long enough that a 1 Hz frame is
// never missed between polls and short enough that a dead OBC is reported rather
// than looked like a quiet link.
const subTimeout = 10 * time.Second

func (s *sub) recv(timeout time.Duration) (*rocsarv1.TelemetryFrame, error) {
	msg, err := s.sock.Recv()
	if err != nil {
		return nil, fmt.Errorf("no telemetry within %s (is the OBC running?)", timeout)
	}
	// [topic, frame]. The topic is frame zero and is not part of the message.
	if len(msg.Frames) < 2 {
		return nil, fmt.Errorf("telemetry arrived in %d frame(s), want [topic, frame]", len(msg.Frames))
	}
	frame := &rocsarv1.TelemetryFrame{}
	if err := proto.Unmarshal(msg.Frames[1], frame); err != nil {
		return nil, fmt.Errorf("telemetry is not a valid TelemetryFrame: %w", err)
	}
	return frame, nil
}

func (s *sub) close() { _ = s.sock.Close() }

func (c *client) pump(ctx context.Context, s *sub, raw, forever bool) error {
	for {
		frame, err := s.recv(10 * time.Second)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if raw {
			b, _ := json.Marshal(frame)
			fmt.Println(string(b))
		} else {
			printTelemetry(frame)
		}
		if !forever {
			return nil
		}
	}
}

// command sends one request and waits for its reply.
func (c *client) command(ctx context.Context, name string, args []string) error {
	reqs, err := buildRequests(name, args)
	if err != nil {
		return err
	}

	zc := zmq4.NewDealer(ctx, zmq4.WithTimeout(commandTimeout))
	defer zc.Close()
	if err := zc.Dial(c.control); err != nil {
		return fmt.Errorf("dial %s: %w", c.control, err)
	}

	// The reply is matched on request_id. Without it there is no way to tell a
	// reply to this command from a late reply to another, and on a link where the
	// operator can send two commands quickly, that is not a theoretical problem.
	for i, req := range reqs {
		// One part. The ROUTER prepends nothing it needs; an extra empty frame is
		// the REQ/REP habit and it breaks the frame count on the far side.
		if err := sendAndWait(ctx, zc, req); err != nil {
			if i == 0 {
				return err
			}
			// A multi-axis command that got partway must say which axis it reached,
			// or the operator retries `zero` and cannot tell what is already centred.
			return fmt.Errorf("%w (axis %d of %d had already been sent)", err, i, len(reqs))
		}
		resp := lastResponse
		fmt.Printf("%s: success=%v error=%s\n", name, resp.GetSuccess(), resp.GetError())
		if m := resp.GetMessage(); m != "" {
			fmt.Printf("  %s\n", m)
		}
		if n := resp.GetArtefactName(); n != "" {
			fmt.Printf("  artefact   %s (%d bytes) -- gs_cli fetch %s\n",
				n, resp.GetArtefactSizeBytes(), n)
		}
		if !resp.GetSuccess() {
			return fmt.Errorf("the OBC refused: %s", resp.GetError())
		}
	}
	return nil
}

// lastResponse is set by sendAndWait. A package-level variable would be wrong;
// a return value threaded through the loop would be clearer, and this keeps the
// loop body readable while staying single-threaded -- which this tool is.
var lastResponse *rocsarv1.CommandResponse

func sendAndWait(ctx context.Context, zc zmq4.Socket, req *rocsarv1.CommandRequest) error {
	body, err := proto.Marshal(req)
	if err != nil {
		return err
	}
	if err := zc.Send(zmq4.NewMsg(body)); err != nil {
		return fmt.Errorf("send: %w", err)
	}
	msg, err := zc.Recv()
	if err != nil {
		return fmt.Errorf("no reply within %s: %w", commandTimeout, err)
	}
	if len(msg.Frames) != 1 {
		return fmt.Errorf("reply has %d frames, want 1: %q", len(msg.Frames), frameHeads(msg.Frames))
	}
	resp := &rocsarv1.CommandResponse{}
	if err := proto.Unmarshal(msg.Frames[0], resp); err != nil {
		return fmt.Errorf("reply is not a CommandResponse: %w", err)
	}
	if resp.GetRequestId() != req.GetRequestId() {
		return fmt.Errorf("reply is for %q but we asked for %q",
			resp.GetRequestId(), req.GetRequestId())
	}
	lastResponse = resp
	return nil
}

// commandTimeout bounds one command round trip.
const commandTimeout = 20 * time.Second

func frameHeads(frames [][]byte) []string {
	out := make([]string, 0, len(frames))
	for _, f := range frames {
		if len(f) > 16 {
			out = append(out, fmt.Sprintf("%x...", f[:16]))
		} else {
			out = append(out, fmt.Sprintf("%x", f))
		}
	}
	return out
}

// buildRequest turns a command name and its arguments into a CommandRequest.
//
// Arguments are validated here rather than by the OBC, because the alternative is
// a command that reaches the servos with a nonsense value and is refused after
// the fact. These run in the same process as the operator's intent, so a mistake
// is caught before it is sent.
// buildRequests returns one request per command that has to be sent.
//
// A slice, because some operations are genuinely more than one command -- `zero`
// centres each axis separately -- and pretending otherwise would either send one
// command and quietly do half the work, or smuggle a loop through a builder whose
// name says otherwise.
func buildRequests(name string, args []string) ([]*rocsarv1.CommandRequest, error) {
	// request_id is chosen here and echoed back, so a reply can be matched to the
	// command that caused it.
	req := &rocsarv1.CommandRequest{
		RequestId: fmt.Sprintf("gs-cli-%d-%s", time.Now().UnixNano(), name),
	}

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
		// but refusing it here means the operator finds out before it is sent.
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
		var out []*rocsarv1.CommandRequest
		for _, id := range ids {
			out = append(out, &rocsarv1.CommandRequest{
				RequestId: fmt.Sprintf("gs-cli-%d-zero-%d", time.Now().UnixNano(), id),
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

func (c *client) list(ctx context.Context) error {
	resp, err := c.httpGet(ctx, "/")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("listing: HTTP %d", resp.StatusCode)
	}
	// The listing is {"files": [...]} with directories included, not a bare
	// array. Directories are shown and marked, because an operator looking for
	// where the SDR put its output needs to see that `raw/` exists at all.
	var listing struct {
		Files []struct {
			Name      string `json:"name"`
			Kind      string `json:"kind"`
			SizeBytes uint64 `json:"size_bytes"`
			Directory bool   `json:"directory"`
		} `json:"files"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listing); err != nil {
		return fmt.Errorf("listing is not the JSON this tool expects: %w", err)
	}
	if len(listing.Files) == 0 {
		fmt.Println("no artefacts")
		return nil
	}
	for _, e := range listing.Files {
		if e.Directory {
			fmt.Printf("%-44s %-8s %10s\n", e.Name+"/", "dir", "-")
			continue
		}
		fmt.Printf("%-44s %-8s %10d\n", e.Name, e.Kind, e.SizeBytes)
	}
	return nil
}

func (c *client) httpGet(ctx context.Context, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.http+path, nil)
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(req)
}

var _ = os.Stdout
