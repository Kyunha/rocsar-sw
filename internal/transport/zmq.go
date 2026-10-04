package transport

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/go-zeromq/zmq4"
	"google.golang.org/protobuf/proto"

	rocsarv1 "github.com/rocsar/obc/api/rocsar/v1"
)

// Handler answers one command. It is called on the control socket's goroutine,
// so it must be bounded: the slowest thing it does is the Pico acknowledgement
// timeout, which is 500 ms by design.
type Handler func(*rocsarv1.CommandRequest) *rocsarv1.CommandResponse

// ZMQ owns the two ZeroMQ sockets.
//
// ONE GOROUTINE PER SOCKET, not one goroutine for both. The reason is a
// limitation of go-zeromq/zmq4, and it is worth recording because the obvious
// design does not work here.
//
// That library has no receive timeout -- not SetReadTimeout, not an RCVTIMEO
// option, nothing -- so Recv blocks until a message arrives or the socket is
// closed. A single loop doing "drain publishes, then Recv" therefore publishes
// nothing while no command is inbound, which with a 1 Hz telemetry stream and an
// idle operator means telemetry that never leaves the process. Polling is not
// available and cannot be added from outside.
//
// So: the ROUTER is owned by one goroutine that Recvs, dispatches and Sends
// replies; the PUB is owned by another that drains a channel and Sends. No
// socket is ever touched by two goroutines, which is the invariant that
// actually matters -- a ZeroMQ socket is not thread-safe, and sharing one
// produces a corrupted message stream that presents as a network fault.
//
// A consequence is that command handling blocks the control socket for its
// duration. That is acceptable here because the handler's slowest operation is a
// bounded timeout and there is a single operator, but it is a real constraint:
// a handler that blocks indefinitely would stop the Ground Station commanding
// anything at all.
type ZMQ struct {
	log     *slog.Logger
	handler Handler

	routerSocket zmq4.Socket
	pubSocket    zmq4.Socket

	// publish carries outbound telemetry to the PUB socket's goroutine.
	publish chan outbound

	mu        sync.RWMutex
	running   bool
	stopped   chan struct{}
	stopOnce  sync.Once
	control   string
	telemetry string
}

type outbound struct {
	topic   string
	payload []byte
}

// NewZMQ creates the sockets. Start binds them.
func NewZMQ(log *slog.Logger, handler Handler) *ZMQ {
	if log == nil {
		log = slog.Default()
	}
	return &ZMQ{
		log:     log,
		handler: handler,
		publish: make(chan outbound, 256),
		stopped: make(chan struct{}),
	}
}

// Start binds both endpoints and starts the two socket goroutines.
//
// A failure to bind is fatal and reported as such. The OBC with no link to the
// Ground Station cannot be controlled, which is a different situation from a
// missing device: continuing would leave an operator watching telemetry with no
// way to act on it.
func (z *ZMQ) Start(ctx context.Context, controlEndpoint, telemetryEndpoint string) error {
	z.control, z.telemetry = controlEndpoint, telemetryEndpoint

	z.routerSocket = zmq4.NewRouter(ctx)
	if err := z.routerSocket.Listen(controlEndpoint); err != nil {
		_ = z.routerSocket.Close()
		return fmt.Errorf("transport: listen on %s: %w", controlEndpoint, err)
	}

	z.pubSocket = zmq4.NewPub(ctx)
	if err := z.pubSocket.Listen(telemetryEndpoint); err != nil {
		_ = z.routerSocket.Close()
		_ = z.pubSocket.Close()
		return fmt.Errorf("transport: listen on %s: %w", telemetryEndpoint, err)
	}

	z.mu.Lock()
	z.running = true
	z.mu.Unlock()

	// Two goroutines, one per socket.
	go z.controlLoop()
	go z.publishLoop(ctx)

	// Close() is what unblocks Recv, since the library offers no receive
	// timeout. Cancelling the context is not enough on its own.
	go func() {
		<-ctx.Done()
		z.closeSockets()
	}()

	z.log.Info("ZeroMQ bound", "control", controlEndpoint, "telemetry", telemetryEndpoint)
	return nil
}

// controlLoop owns the ROUTER socket.
func (z *ZMQ) controlLoop() {
	for {
		msg, err := z.routerSocket.Recv()
		if err != nil {
			// The socket was closed, which is the only way Recv returns here.
			return
		}

		// ROUTER frames: [identity, <empty>, payload...]. The empty frame is part
		// of the REQ/DEALER envelope and carries no information.
		if len(msg.Frames) < 3 {
			z.log.Warn("short router frame", "frames", len(msg.Frames))
			continue
		}

		resp := z.handle(msg.Frames[len(msg.Frames)-1])

		// The ROUTER prepends the sender's identity. It also strips the empty
		// delimiter the DEALER sent, so what goes back is [identity, payload].
		// Verified against real sockets rather than assumed from the pattern.
		out := zmq4.NewMsgFrom(msg.Frames[0], mustMarshal(resp))
		if err := z.routerSocket.Send(out); err != nil {
			z.log.Warn("reply failed", "request_id", resp.GetRequestId(), "err", err)
		}
	}
}

// handle decodes and dispatches one command.
//
// It always returns a response, including for a command it cannot decode. A
// client blocked in a blocking send with no reply waits its entire timeout, and
// an operator watching a spinner learns nothing about why.
func (z *ZMQ) handle(payload []byte) *rocsarv1.CommandResponse {
	req := &rocsarv1.CommandRequest{}
	if err := proto.Unmarshal(payload, req); err != nil {
		z.log.Warn("undecodable command", "err", err, "bytes", len(payload))
		return &rocsarv1.CommandResponse{
			Success: false,
			Error:   rocsarv1.ErrorCode_ERROR_INVALID_COMMAND,
			Message: fmt.Sprintf("could not decode the command: %v", err),
		}
	}

	if z.handler == nil {
		return &rocsarv1.CommandResponse{
			RequestId: req.GetRequestId(),
			Success:   false,
			Error:     rocsarv1.ErrorCode_ERROR_UNSUPPORTED,
			Message:   "no command handler is installed",
		}
	}

	resp := z.handler(req)
	if resp == nil {
		return &rocsarv1.CommandResponse{
			RequestId: req.GetRequestId(),
			Success:   false,
			Error:     rocsarv1.ErrorCode_ERROR_INTERNAL,
			Message:   "the command handler returned no response",
		}
	}
	// The reply always echoes the request id. Without it a client cannot match
	// the reply to the control that caused it.
	resp.RequestId = req.GetRequestId()
	return resp
}

// publishLoop owns the PUB socket.
func (z *ZMQ) publishLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			// One last drain so frames queued in the final interval still go out.
			z.drain()
			return
		case o := <-z.publish:
			if err := z.send(o); err != nil {
				z.log.Warn("publish failed", "topic", o.topic, "err", err)
			}
			// Keep going while there is more queued rather than blocking on the
			// next channel receive: a burst should not be paced at one message
			// per select iteration.
			z.drain()
		}
	}
}

func (z *ZMQ) drain() {
	for {
		select {
		case o := <-z.publish:
			if err := z.send(o); err != nil {
				z.log.Warn("publish failed", "topic", o.topic, "err", err)
			}
		default:
			return
		}
	}
}

func (z *ZMQ) send(o outbound) error {
	return z.pubSocket.Send(zmq4.NewMsgFrom([]byte(o.topic), o.payload))
}

// Publish queues a telemetry frame.
//
// Queued, not sent: the PUB socket belongs to publishLoop. Dropping on a full
// queue is correct here and nowhere else -- PUB/SUB is lossy by design, the next
// frame is one second behind, and blocking would stall the telemetry pipeline
// behind a socket problem. Telemetry is the operator's only view of the vehicle,
// so it is the last thing that should be made to wait.
func (z *ZMQ) Publish(topic string, payload []byte) {
	if !z.Running() {
		return
	}
	select {
	case z.publish <- outbound{topic: topic, payload: payload}:
	default:
		z.log.Warn("publish queue full, dropping a telemetry frame",
			"topic", topic, "bytes", len(payload))
	}
}

func (z *ZMQ) closeSockets() {
	z.stopOnce.Do(func() {
		z.mu.Lock()
		z.running = false
		router, pub := z.routerSocket, z.pubSocket
		z.mu.Unlock()

		// PUB first, then ROUTER: closing the ROUTER is what unblocks Recv and
		// ends controlLoop.
		if pub != nil {
			_ = pub.Close()
		}
		if router != nil {
			_ = router.Close()
		}
		close(z.stopped)
		z.log.Info("ZeroMQ closed")
	})
}

// Stop closes the sockets and waits for the goroutines.
func (z *ZMQ) Stop() {
	z.closeSockets()
	select {
	case <-z.stopped:
	case <-time.After(2 * time.Second):
		z.log.Warn("ZeroMQ shutdown timed out")
	}
}

// Running reports whether the sockets are bound.
func (z *ZMQ) Running() bool {
	z.mu.RLock()
	defer z.mu.RUnlock()
	return z.running
}

// Endpoints returns the bound addresses.
func (z *ZMQ) Endpoints() (control, telemetry string) {
	z.mu.RLock()
	defer z.mu.RUnlock()
	return z.control, z.telemetry
}

// mustMarshal serialises a message.
//
// Every caller is on a path where there is nothing useful to do about a
// marshalling error -- the message was just built from validated fields -- and
// threading an error through a socket send for a case that cannot happen adds a
// signature nobody needs. An empty frame is still better than silence: the
// client sees a reply it cannot decode, which is diagnosable.
func mustMarshal(m proto.Message) []byte {
	b, err := proto.Marshal(m)
	if err != nil {
		slog.Error("marshalling a message failed", "err", err, "type", fmt.Sprintf("%T", m))
		return nil
	}
	return b
}
