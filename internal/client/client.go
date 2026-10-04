// Package client is the Ground Station's side of the link to the OBC.
//
// It was promoted out of tools/gs_cli, which was `package main` and therefore
// not importable. Both consoles now share it: the terminal one in tools/, and
// the Wails one in cmd/gs. Two clients against one wire format is one more
// thing that can disagree with the server, so there is one.
//
// What lives here and what does not is a deliberate line. Sockets, framing,
// timeouts, request correlation, argument validation and artefact transfer are
// here. Supervised behaviour -- reconnecting streams, the command queue, link
// health -- is in stream.go, queue.go and link.go, built on the one-shot
// primitives in this file so the envelope assertions exist exactly once.
// Deciding what a reply *means* -- formatting a telemetry frame as text,
// or as the structs a window draws -- is not, and is in internal/gsview or in
// the console that owns the pixels.
//
// The comments moved with the code. Several record defects that were found the
// hard way and are cheap to reintroduce:
//
//   - a REQ socket inserts an empty delimiter that the OBC's ROUTER does not
//     send, so the reply arrives one frame late;
//   - a SUB filter set after dialling loses the first frames invisibly;
//   - a resume that trusts a server which ignored the Range silently corrupts
//     the file;
//   - an artefact-name check too strict to accept the names the tool itself
//     printed one line earlier.
//
// See GUI_ARCHITECTURE.md sections 6 and 7.
package client

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/go-zeromq/zmq4"
	"google.golang.org/protobuf/proto"

	rocsarv1 "github.com/rocsar/obc/api/rocsar/v1"
)

// Config is where the three OBC endpoints live.
//
// All three, because they are three different transports and the split is not
// negotiable: commands and telemetry are ZeroMQ, artefact bytes are HTTP, and a
// command channel sized for kilobytes has no business carrying a photograph.
// See ARCHITECTURE.md 5.1.
//
// Resolution is the caller's job. cmd/gs reads these from the [client] section
// of rocsar.toml through internal/config, which is the same file and the same
// four-level precedence the OBC uses. Nothing here reads a file.
type Config struct {
	// Control is the OBC's ROUTER: tcp://host:5555.
	Control string
	// Telemetry is the OBC's PUB: tcp://host:5556.
	Telemetry string
	// HTTP is the OBC's artefact server: http://host:5557.
	HTTP string
	// Topic is the telemetry topic to subscribe to. It is "telemetry" and
	// nothing else.
	//
	// ARCHITECTURE.md 5.1 lists five topics -- telemetry.system, telemetry.gnss,
	// telemetry.pico, telemetry.sdr, telemetry.camera. None of them exist.
	// internal/qos/topics.go declares TopicTelemetry = "telemetry" and
	// AllTopics() returns exactly that one element; only the aggregate frame is
	// ever published, and TopicControl is declared and never used.
	//
	// A client built from the document subscribes to five topics, matches none,
	// and reports a dead OBC -- because a ZeroMQ filter mismatch and a dead OBC
	// are indistinguishable from this side of the socket.
	Topic string
}

// Client holds the three endpoints.
//
// Not safe for concurrent use by a caller that shares one socket. Each long-lived
// role gets its own socket and its own reader, which is the same invariant
// internal/transport runs: a ZeroMQ socket is not thread-safe, and the failure
// mode of sharing one is a corrupted message stream that presents as a network
// fault.
type Client struct {
	cfg Config
}

// New returns a client for the given endpoints. It opens nothing: dialling is
// the caller's decision, so a console can render a connection panel before
// anything is attempted.
func New(cfg Config) *Client {
	if cfg.Topic == "" {
		cfg.Topic = "telemetry"
	}
	return &Client{cfg: cfg}
}

// Config returns the endpoints this client was built with.
func (c *Client) Config() Config { return c.cfg }

// ---------------------------------------------------------------------------
// Telemetry
// ---------------------------------------------------------------------------

// Subscription is an open SUB socket.
//
// One reader, and the socket belongs to it. go-zeromq/zmq4 has NO receive
// timeout -- not SetReadTimeout, not RCVTIMEO, nothing -- so Recv blocks until a
// message arrives or the socket is closed. That is why the OBC runs one
// goroutine per socket and closes the socket to release a blocked receive, and
// why Close is the shutdown mechanism rather than a context.
type Subscription struct {
	sock zmq4.Socket
}

// Subscribe opens the telemetry SUB socket. One socket per call; see dialSub
// for the filter-before-dial ordering this shares with Stream.
func (c *Client) Subscribe() (*Subscription, error) {
	return dialSub(c.cfg.Telemetry, c.cfg.Topic, subTimeout)
}

// dialSub opens one SUB socket with its filter set before dialling.
//
// The filter is set BEFORE dialling, so the subscription is in place before the
// first frame can arrive. A SUB that matches nothing fails silently rather than
// reporting a mismatch, so a filter set late loses frames invisibly -- and the
// symptom appears much later as "the link went quiet".
//
// Shared by Subscribe (one socket per call) and Stream (one socket per
// reconnect), so the ordering cannot drift between the two.
func dialSub(endpoint, topic string, timeout time.Duration) (*Subscription, error) {
	ctx := context.Background()

	// The timeout is a construction option: this library has no per-call receive
	// timeout, which is why Recv blocks until a message arrives or the socket is
	// closed.
	sock := zmq4.NewSub(ctx, zmq4.WithTimeout(timeout))
	if err := sock.SetOption(zmq4.OptionSubscribe, topic); err != nil {
		_ = sock.Close()
		return nil, fmt.Errorf("client: subscribe to %q: %w", topic, err)
	}
	if err := sock.Dial(endpoint); err != nil {
		_ = sock.Close()
		return nil, fmt.Errorf("client: dial %s: %w", endpoint, err)
	}
	return &Subscription{sock: sock}, nil
}

// subTimeout bounds one receive. Ten seconds is long enough that a 1 Hz frame is
// never missed between polls and short enough that a dead OBC is reported rather
// than looked like a quiet link.
const subTimeout = 10 * time.Second

// SubTimeout is the socket-level receive bound.
func SubTimeout() time.Duration { return subTimeout }

// Recv returns the next telemetry frame.
//
// `timeout` is used only to phrase the error. The bound that actually applies is
// the socket-level one fixed in Subscribe, and it cannot be changed per call --
// which is the whole reason this signature looks redundant.
//
// The wire is [topic, frame]. The topic is frame zero and is not part of the
// message, which is what makes ZeroMQ's prefix filtering exact.
func (s *Subscription) Recv(timeout time.Duration) (*rocsarv1.TelemetryFrame, error) {
	msg, err := s.sock.Recv()
	if err != nil {
		return nil, fmt.Errorf("no telemetry within %s (is the OBC running?)", timeout)
	}
	if len(msg.Frames) < 2 {
		return nil, fmt.Errorf("telemetry arrived in %d frame(s), want [topic, frame]", len(msg.Frames))
	}
	// The topic is asserted rather than skipped. A wrong topic string and a dead
	// OBC look identical from here, and a SUB that matched nothing would fail
	// silently rather than reporting a mismatch -- so the one case this can catch
	// is worth catching loudly.
	if got := string(msg.Frames[0]); got != qosTopicTelemetry {
		return nil, fmt.Errorf("telemetry arrived on topic %q, want %q; the OBC is a different version", got, qosTopicTelemetry)
	}
	frame := &rocsarv1.TelemetryFrame{}
	if err := proto.Unmarshal(msg.Frames[1], frame); err != nil {
		return nil, fmt.Errorf("telemetry is not a valid TelemetryFrame: %w", err)
	}
	return frame, nil
}

// Close releases the socket, which is what unblocks a Recv already in progress.
func (s *Subscription) Close() error { return s.sock.Close() }

// qosTopicTelemetry is the one topic the OBC publishes.
//
// It is duplicated here rather than imported from internal/qos on purpose:
// internal/qos is the server's package, and the client reaching into the server
// for a string constant is the dependency this promotion exists to avoid. Both
// sides are asserted against their own copy, and a mismatch is a loud error from
// Recv rather than a silent disconnect. If the vocabulary ever grows, this and
// internal/qos/topics.go change together or the console goes quiet.
const qosTopicTelemetry = "telemetry"

// ---------------------------------------------------------------------------
// Commands
// ---------------------------------------------------------------------------

// Send one request and wait for its reply.
//
// DEALER, never REQ. The OBC side is a ROUTER and sends [identity, payload] --
// no empty delimiter. A REQ socket always inserts one, so the reply arrives one
// frame late and the payload is read from the wrong place. This cost an
// afternoon once: the OBC was dropping every command with "short router frame"
// while its own tests passed, because the test client hand-rolled the envelope.
//
// The reply is matched on request_id. Without it there is no way to tell a reply
// to this command from a late reply to another, and on a link where the operator
// can send two commands quickly, that is not a theoretical problem.
//
// A fresh DEALER per call. That is right for a console that sends a command and
// waits, and wrong for one that keeps a link open -- see GUI_ARCHITECTURE.md 6.4
// for the single-in-flight queue this becomes.
func (c *Client) Send(ctx context.Context, req *rocsarv1.CommandRequest) (*rocsarv1.CommandResponse, error) {
	zc := zmq4.NewDealer(ctx, zmq4.WithTimeout(CommandTimeout))
	defer zc.Close()
	if err := zc.Dial(c.cfg.Control); err != nil {
		return nil, fmt.Errorf("client: dial %s: %w", c.cfg.Control, err)
	}
	return roundTrip(zc, req, CommandTimeout)
}

// roundTrip sends one request on an already-dialled DEALER and waits for its
// reply.
//
// Shared by Send (fresh socket per call) and Queue (one persistent socket).
// There is exactly one implementation of the envelope assertions -- one part
// out, one frame back, matched on request_id -- because two copies would be two
// places for the frame count to drift. See the comment on Send for why the
// envelope looks the way it does.
//
// timeout bounds the exchange and phrases the error. It is a parameter rather
// than the constant so the message never claims a bound that was not applied.
func roundTrip(zc zmq4.Socket, req *rocsarv1.CommandRequest, timeout time.Duration) (*rocsarv1.CommandResponse, error) {
	body, err := proto.Marshal(req)
	if err != nil {
		return nil, err
	}
	// One part. The ROUTER prepends the sender identity; an extra empty frame is
	// the REQ/REP habit and it breaks the frame count on the far side.
	if err := zc.Send(zmq4.NewMsg(body)); err != nil {
		return nil, fmt.Errorf("client: send: %w", err)
	}
	msg, err := zc.Recv()
	if err != nil {
		return nil, fmt.Errorf("client: no reply within %s: %w", timeout, err)
	}
	if len(msg.Frames) != 1 {
		return nil, fmt.Errorf("client: reply has %d frames, want 1: %q", len(msg.Frames), frameHeads(msg.Frames))
	}
	resp := &rocsarv1.CommandResponse{}
	if err := proto.Unmarshal(msg.Frames[0], resp); err != nil {
		return nil, fmt.Errorf("client: reply is not a CommandResponse: %w", err)
	}
	if resp.GetRequestId() != req.GetRequestId() {
		return nil, fmt.Errorf("client: reply is for %q but we asked for %q",
			resp.GetRequestId(), req.GetRequestId())
	}
	return resp, nil
}

// CommandTimeout bounds one command round trip.
//
// Generous, because the OBC's slowest handler is the Pico acknowledgement
// timeout at a fixed 500ms (internal/pico/framing.go) and everything else is
// microseconds. The number that actually matters is not this one but how many
// commands are in flight at once: the OBC's ROUTER handles one at a time, so a
// second command waits behind the first rather than running beside it. See
// GUI_ARCHITECTURE.md 6.4.
const CommandTimeout = 20 * time.Second

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

// httpGet is the one way this package reaches the artefact server.
func (c *Client) httpGet(ctx context.Context, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.HTTP+path, nil)
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(req)
}
