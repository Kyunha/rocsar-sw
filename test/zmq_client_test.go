package test

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-zeromq/zmq4"
	"google.golang.org/protobuf/proto"

	rocsarv1 "github.com/rocsar/obc/api/rocsar/v1"
)

// The clients in this file are DEALER and SUB peers for the OBC's ROUTER and
// PUB sockets, as the Ground Station would be.
//
// Two properties of go-zeromq/zmq4 shape how they are written, and both were
// found by a flaky test rather than by reading the documentation:
//
//  1. Recv honours no timeout. There is no SetReadDeadline, and WithTimeout --
//     which looks like it would cover it -- only bounds Send. socket.go's Recv
//     derives a plain cancellable context with no deadline, so a Recv with
//     nothing to read blocks until the socket is closed. This is why the OBC
//     closes sockets to shut its receive loop down.
//
//  2. A socket must have exactly one reader. The first version of recvTelemetry
//     spawned a fresh goroutine per call and abandoned it on timeout; the
//     abandoned goroutine stayed blocked in Recv and then raced the NEXT call
//     for frames, which is the same thread-safety violation the production code
//     is careful to avoid. It failed roughly one run in eight with "no telemetry
//     arrived".
//
// So each client runs ONE reader goroutine for the life of the socket, feeding a
// buffered channel. Recv-on-a-timeout becomes a channel read with a timeout.

// dealerClient is a DEALER speaking to the OBC's ROUTER.
//
// DEALER rather than REQ because REQ enforces strict send/receive alternation
// and holds state, so two operators connecting to one OBC break each other. The
// ROUTER on the OBC side is what makes a second operator possible, and this is
// the other half of it.
type dealerClient struct {
	sock    zmq4.Socket
	replies chan *rocsarv1.CommandResponse

	closeOnce sync.Once
	done      chan struct{}
}

var idCounter int

func uniqueID() string {
	idCounter++
	return fmt.Sprintf("gs-%d-%d", time.Now().UnixNano(), idCounter)
}

func newDealer(endpoint string) (*dealerClient, error) {
	ctx := context.Background()
	s := zmq4.NewDealer(ctx,
		zmq4.WithID(zmq4.SocketIdentity(uniqueID())),
		zmq4.WithTimeout(4*time.Second))

	if err := s.Dial(endpoint); err != nil {
		return nil, fmt.Errorf("dial %s: %w", endpoint, err)
	}

	c := &dealerClient{
		sock:    s,
		replies: make(chan *rocsarv1.CommandResponse, 8),
		done:    make(chan struct{}),
	}

	// One reader for the life of the socket. Close() is what ends it.
	// The reader owns both channels' lifetime: it closes `done` on the way out
	// so close() knows when the socket is fully released, and closes `replies` so
	// a pending recv() unblocks rather than waiting out its timeout.
	go func() {
		defer close(c.done)
		for {
			msg, err := s.Recv()
			if err != nil {
				close(c.replies)
				return
			}
			resp, err := decodeReply(msg)
			if err != nil {
				continue
			}
			select {
			case c.replies <- resp:
			default:
			}
		}
	}()

	return c, nil
}

func (c *dealerClient) send(req *rocsarv1.CommandRequest) error {
	body, err := proto.Marshal(req)
	if err != nil {
		return err
	}
	return c.sendRaw(body)
}

// sendRaw writes bytes that may not be a valid message, to check that the OBC
// answers rather than drops.
func (c *dealerClient) sendRaw(body []byte) error {
	// The DEALER envelope a ROUTER expects: an empty delimiter then the payload.
	return c.sock.Send(zmq4.NewMsgFrom([]byte(""), body))
}

func (c *dealerClient) recv(timeout time.Duration) (*rocsarv1.CommandResponse, error) {
	select {
	case resp, ok := <-c.replies:
		if !ok || resp == nil {
			return nil, fmt.Errorf("the connection closed before a reply arrived")
		}
		return resp, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("no reply within %s", timeout)
	}
}

func decodeReply(msg zmq4.Msg) (*rocsarv1.CommandResponse, error) {
	// The frame layout of a ROUTER socket is asymmetric, and both directions
	// were verified against real sockets rather than assumed from the ZMTP
	// pattern -- two wrong guesses in a row here is what "short reply: 2 frames"
	// and then "short reply: 1 frames" were.
	//
	//	Receiving (ROUTER side): [sender identity, <empty>, payload]
	//	                       -- the identity is prepended on arrival.
	//	Sending   (ROUTER side): [destination identity, payload]
	//	                       -- the FIRST frame is consumed as routing and is
	//	                          NOT put on the wire.
	//
	// So a DEALER on the far end receives just the payload, and it is read from
	// the last frame rather than a fixed index.
	if len(msg.Frames) < 1 {
		return nil, fmt.Errorf("empty reply")
	}
	payload := msg.Frames[len(msg.Frames)-1]
	resp := &rocsarv1.CommandResponse{}
	if err := proto.Unmarshal(payload, resp); err != nil {
		return nil, fmt.Errorf("undecodable reply (%d bytes): %w", len(payload), err)
	}
	return resp, nil
}

func (c *dealerClient) close() {
	c.closeOnce.Do(func() { _ = c.sock.Close() })
	<-c.done
}

// subClient is a SUB reading the OBC's PUB socket.
type subClient struct {
	sock     zmq4.Socket
	messages chan subMessage
	done     chan struct{}
	closeOne sync.Once
}

type subMessage struct {
	topic string
	frame *rocsarv1.TelemetryFrame
}

func newSub(endpoint string) (*subClient, error) {
	ctx := context.Background()
	s := zmq4.NewSub(ctx, zmq4.WithTimeout(4*time.Second))
	if err := s.Dial(endpoint); err != nil {
		return nil, fmt.Errorf("dial %s: %w", endpoint, err)
	}

	c := &subClient{
		sock:     s,
		messages: make(chan subMessage, 64),
		done:     make(chan struct{}),
	}

	// One reader for the life of the socket, as on the dealer side.
	go func() {
		defer close(c.done)
		for {
			msg, err := s.Recv()
			if err != nil {
				close(c.messages)
				return
			}
			m := decodeTelemetry(msg)
			if m.frame == nil && m.topic == "" {
				continue
			}
			select {
			case c.messages <- m:
			default:
			}
		}
	}()

	return c, nil
}

// subscribe sets the topic filter.
//
// ZeroMQ filters on frame PREFIX, not equality. That is why the OBC puts the
// topic in its own leading frame -- otherwise "telemetry.s" would also match
// "telemetry.system".
func (s *subClient) subscribe(topic string) {
	if err := s.sock.SetOption(string(zmq4.OptionSubscribe), topic); err != nil {
		panic(fmt.Sprintf("subscribe %q: %v", topic, err))
	}
}

// recv returns the next message, or ("", nil) if none arrives in time.
func (s *subClient) recv(timeout time.Duration) (string, *rocsarv1.TelemetryFrame) {
	select {
	case m, ok := <-s.messages:
		if !ok {
			return "", nil
		}
		return m.topic, m.frame
	case <-time.After(timeout):
		return "", nil
	}
}

func decodeTelemetry(msg zmq4.Msg) subMessage {
	if len(msg.Frames) == 0 {
		return subMessage{}
	}
	// [topic, payload] -- a PUB socket prepends nothing, so the payload is the
	// last frame.
	payload := msg.Frames[len(msg.Frames)-1]
	frame := &rocsarv1.TelemetryFrame{}
	if err := proto.Unmarshal(payload, frame); err != nil {
		if len(msg.Frames) >= 2 {
			return subMessage{topic: string(msg.Frames[0])}
		}
		return subMessage{}
	}
	if len(msg.Frames) >= 2 {
		return subMessage{topic: string(msg.Frames[0]), frame: frame}
	}
	return subMessage{frame: frame}
}

func (s *subClient) close() {
	s.closeOne.Do(func() { _ = s.sock.Close() })
	<-s.done
}
