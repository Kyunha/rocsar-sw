package client

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/go-zeromq/zmq4"
	"google.golang.org/protobuf/proto"

	rocsarv1 "github.com/rocsar/obc/api/rocsar/v1"
)

// freePort asks the OS for an unused localhost port.
//
// Asked, not hardcoded: a hardcoded port that is already in use fails the suite
// for a reason that has nothing to do with the code, and a suite that fails
// spuriously gets ignored. Same idiom as test/transport_test.go.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// fakeOBC speaks the OBC's side of the wire for tests: a ROUTER that answers
// commands and a PUB that emits telemetry. Both on 127.0.0.1, both on
// OS-allocated ports, both torn down by Close.
//
// It is a fake, not a mock: it runs the real go-zeromq sockets over the real
// loopback, so the frame layout, the envelope and the async-connect behaviour
// are all exercised rather than stubbed.
type fakeOBC struct {
	t       *testing.T
	control string
	publish string

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu     sync.Mutex
	router zmq4.Socket
	pub    zmq4.Socket
	seq    uint64

	// onCommand answers one decoded request. It runs on the ROUTER reader
	// goroutine; keep it fast or make it block deliberately.
	onCommand func(*rocsarv1.CommandRequest) *rocsarv1.CommandResponse

	// received records every request the ROUTER answered, in order.
	received []*rocsarv1.CommandRequest
}

func newFakeOBC(t *testing.T) *fakeOBC {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeOBC{
		t:       t,
		control: fmt.Sprintf("tcp://127.0.0.1:%d", freePort(t)),
		publish: fmt.Sprintf("tcp://127.0.0.1:%d", freePort(t)),
		ctx:     ctx,
		cancel:  cancel,
		onCommand: func(req *rocsarv1.CommandRequest) *rocsarv1.CommandResponse {
			return &rocsarv1.CommandResponse{
				RequestId: req.GetRequestId(),
				Success:   true,
				Error:     rocsarv1.ErrorCode_ERROR_NONE,
				Message:   "fake obc",
			}
		},
	}
	f.listenRouter(t)
	f.listenPub(t)
	f.wg.Add(1)
	go f.serveRouter()
	return f
}

func (f *fakeOBC) listenRouter(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	r := zmq4.NewRouter(f.ctx)
	if err := r.Listen(f.control); err != nil {
		t.Fatalf("fake obc listen %s: %v", f.control, err)
	}
	f.router = r
}

func (f *fakeOBC) listenPub(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	p := zmq4.NewPub(f.ctx)
	if err := p.Listen(f.publish); err != nil {
		t.Fatalf("fake obc listen %s: %v", f.publish, err)
	}
	f.pub = p
}

// serveRouter answers commands until the context ends. The payload is the last
// frame and the frames before it are envelope -- the same contract
// internal/transport documents, read from the other end.
func (f *fakeOBC) serveRouter() {
	defer f.wg.Done()
	for {
		f.mu.Lock()
		r := f.router
		f.mu.Unlock()
		if r == nil {
			return
		}
		msg, err := r.Recv()
		if err != nil {
			select {
			case <-f.ctx.Done():
				return
			default:
				continue
			}
		}
		if len(msg.Frames) < 2 {
			continue
		}
		req := &rocsarv1.CommandRequest{}
		if err := proto.Unmarshal(msg.Frames[len(msg.Frames)-1], req); err != nil {
			continue
		}
		f.mu.Lock()
		f.received = append(f.received, req)
		f.mu.Unlock()
		resp := f.onCommand(req)
		body, err := proto.Marshal(resp)
		if err != nil {
			continue
		}
		_ = r.Send(zmq4.NewMsgFrom(msg.Frames[0], body))
	}
}

// emitOne publishes a single frame with the next sequence number.
func (f *fakeOBC) emitOne(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	f.seq++
	seq := f.seq
	p := f.pub
	f.mu.Unlock()
	if p == nil {
		t.Fatal("emit with no PUB listener; the publisher is down")
	}
	frame := &rocsarv1.TelemetryFrame{Sequence: seq}
	body, err := proto.Marshal(frame)
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	if err := p.Send(zmq4.NewMsgFrom([]byte("telemetry"), body)); err != nil {
		t.Fatalf("publish: %v", err)
	}
}

// emit publishes count frames with consecutive sequence numbers, pausing gap
// every interval. Gaps in the numbering must be produced by emitting and
// skipping -- there is no other honest way to make a skipped sequence number
// appear on the wire.
func (f *fakeOBC) emit(t *testing.T, count int, interval time.Duration) {
	t.Helper()
	for i := 0; i < count; i++ {
		f.emitOne(t)
		if interval > 0 {
			time.Sleep(interval)
		}
	}
}

// backgroundEmit runs a fixed burst in the background and returns a join
// function. The join must be deferred before Close: an emitter still running
// when the PUB closes fails its Send and calls t.Fatal from a goroutine whose
// test has already completed, which panics the whole binary -- taking every
// other test's results with it.
func backgroundEmit(t *testing.T, f *fakeOBC, n int, interval time.Duration) (join func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.emit(t, n, interval)
	}()
	return func() {
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("background emitter did not finish; the burst was lost")
		}
	}
}

// emitUntil publishes one frame every interval until stop is closed. The stop
// is checked before every emit, so at most one frame is ever in flight after
// it closes -- which is what makes drainFrames deterministic.
func (f *fakeOBC) emitUntil(t *testing.T, interval time.Duration, stop <-chan struct{}) {
	t.Helper()
	for {
		select {
		case <-stop:
			return
		default:
		}
		f.emitOne(t)
		select {
		case <-stop:
			return
		case <-time.After(interval):
		}
	}
}

// drainFrames reads until quiet has passed with no frame, and returns the last
// sequence number seen, or 0 if none arrived.
//
// Used after stopping a background publisher: with the publisher stopped and
// the socket quiet well beyond PUB/SUB latency, nothing is still in flight, so
// the stream's accepted number is exactly what was last seen here. That turns
// "skip then emit" into a deterministic gap instead of a race.
func drainFrames(t *testing.T, s *Stream, quiet time.Duration) uint64 {
	t.Helper()
	var last uint64
	for {
		select {
		case fr := <-s.Frames():
			last = fr.Telemetry.GetSequence()
		case <-time.After(quiet):
			return last
		}
	}
}

// skip advances the sequence counter without emitting, so the next emit
// produces a gap the client must count.
func (f *fakeOBC) skip(n uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq += n
}

// rewind resets the sequence counter, simulating an OBC restart.
func (f *fakeOBC) rewind() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq = 0
}

// dropPub closes the telemetry listener, simulating the link dying. The port
// stays reserved by the address, not by a socket, so reopenPub rebinds it.
func (f *fakeOBC) dropPub() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pub != nil {
		_ = f.pub.Close()
		f.pub = nil
	}
}

// reopenPub rebinds the telemetry listener on the same address.
func (f *fakeOBC) reopenPub(t *testing.T) {
	t.Helper()
	f.listenPub(t)
}

// receivedCount reports how many commands the ROUTER has answered.
func (f *fakeOBC) receivedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.received)
}

func (f *fakeOBC) close() {
	f.cancel()
	f.mu.Lock()
	if f.router != nil {
		_ = f.router.Close()
		f.router = nil
	}
	if f.pub != nil {
		_ = f.pub.Close()
		f.pub = nil
	}
	f.mu.Unlock()
	f.wg.Wait()
}

// testConfig points a client at the fake.
func (f *fakeOBC) testConfig() Config {
	return Config{Control: f.control, Telemetry: f.publish, Topic: "telemetry"}
}

// eventually polls cond until it holds or the timeout expires. Polling, not
// sleeping: a fixed sleep is either too short on a loaded machine or too long
// everywhere else, and both failure modes look like flakes.
func eventually(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for: %s", timeout, msg)
}

// nextFrame waits for one delivered frame.
func nextFrame(t *testing.T, s *Stream, timeout time.Duration) Frame {
	t.Helper()
	select {
	case f := <-s.Frames():
		return f
	case <-time.After(timeout):
		t.Fatalf("no frame within %s", timeout)
		return Frame{}
	}
}
