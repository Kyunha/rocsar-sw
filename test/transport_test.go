package test

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	rocsarv1 "github.com/rocsar/obc/api/rocsar/v1"
	"github.com/rocsar/obc/internal/transport"
)

// Real sockets, real protobuf, real framing.
//
// go-zeromq/zmq4 is a pure-Go implementation of ZMTP 3.1 speaking to libzmq's
// pyzmq on the Ground Station. That interop is unproven by us, and the previous
// attempt at this system had a REQ socket whose REP partner was wired to the
// wrong event, so nothing ever arrived. These tests bind real ports and use real
// sockets rather than a fake, precisely because the failure mode being guarded
// against is "the test passes and the link does not work".
//
// These are Go-to-Go. They prove OUR side is correct and the library works; they
// do not prove pyzmq interop. tools/gs_probe.py is the cross-language check and
// it has to be run on a machine with pyzmq.

// freePorts returns a pair of unused localhost ports.
//
// Asked of the OS rather than hardcoded, because a hardcoded port that is
// already in use fails the suite for a reason that has nothing to do with the
// code, and a suite that fails spuriously gets ignored.
func freePorts(t *testing.T, n int) []int {
	t.Helper()
	var out []int
	var lns []net.Listener
	for i := 0; i < n; i++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("reserving a port: %v", err)
		}
		lns = append(lns, ln)
		out = append(out, ln.Addr().(*net.TCPAddr).Port)
	}
	for _, ln := range lns {
		_ = ln.Close()
	}
	return out
}

// A command sent by a DEALER must reach the handler and come back to the same
// peer, with the request_id echoed.
func TestControlRoundTripOverRealSockets(t *testing.T) {
	ports := freePorts(t, 2)
	control := addr(ports[0])
	telemetry := addr(ports[1])

	received := make(chan *rocsarv1.CommandRequest, 4)
	z := transport.NewZMQ(quietLogger(), func(req *rocsarv1.CommandRequest) *rocsarv1.CommandResponse {
		received <- req
		return &rocsarv1.CommandResponse{Success: true, Message: "ok"}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer z.Stop()

	if err := z.Start(ctx, control, telemetry); err != nil {
		t.Fatalf("Start: %v", err)
	}

	client, err := newDealer(control)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.close()

	req := &rocsarv1.CommandRequest{
		RequestId: "btn-jog-7",
		Payload: &rocsarv1.CommandRequest_Pico{
			Pico: &rocsarv1.PicoCommand{
				Sequence: 3,
				Payload: &rocsarv1.PicoCommand_Jog{
					Jog: &rocsarv1.JogCommand{ServoId: 1, Tick: 2048},
				},
			},
		},
	}

	if err := client.send(req); err != nil {
		t.Fatalf("send: %v", err)
	}

	resp, err := client.recv(3 * time.Second)
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	if !resp.GetSuccess() {
		t.Errorf("success = false: %s", resp.GetMessage())
	}
	if resp.GetRequestId() != req.GetRequestId() {
		t.Errorf("request_id = %q, want %q -- the reply cannot be matched to the control that caused it",
			resp.GetRequestId(), req.GetRequestId())
	}

	select {
	case got := <-received:
		if got.GetPico().GetJog().GetTick() != 2048 {
			t.Errorf("the handler saw tick %d, want 2048", got.GetPico().GetJog().GetTick())
		}
		if got.GetPico().GetSequence() != 3 {
			t.Errorf("the handler saw sequence %d, want 3", got.GetPico().GetSequence())
		}
	case <-time.After(time.Second):
		t.Fatal("the handler never saw the command")
	}
}

// A command the handler cannot satisfy must still get a reply. A client in a
// blocking send with no reply waits its whole timeout, and an operator watching
// a spinner learns nothing.
func TestEveryCommandGetsAReply(t *testing.T) {
	ports := freePorts(t, 2)
	control, telemetry := addr(ports[0]), addr(ports[1])

	failing := func(req *rocsarv1.CommandRequest) *rocsarv1.CommandResponse {
		return &rocsarv1.CommandResponse{
			Success: false,
			Error:   rocsarv1.ErrorCode_ERROR_NOT_CONNECTED,
			Message: "no flight controller",
		}
	}
	z := transport.NewZMQ(quietLogger(), failing)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer z.Stop()

	if err := z.Start(ctx, control, telemetry); err != nil {
		t.Fatalf("Start: %v", err)
	}
	client, err := newDealer(control)
	if err != nil {
		t.Fatal(err)
	}
	defer client.close()

	if err := client.send(&rocsarv1.CommandRequest{RequestId: "x"}); err != nil {
		t.Fatal(err)
	}
	resp, err := client.recv(3 * time.Second)
	if err != nil {
		t.Fatalf("a failing command got no reply: %v", err)
	}
	if resp.GetSuccess() {
		t.Error("a failing command reported success")
	}
	if resp.GetError() != rocsarv1.ErrorCode_ERROR_NOT_CONNECTED {
		t.Errorf("error = %s, want ERROR_NOT_CONNECTED", resp.GetError())
	}
}

// An undecodable payload must be answered, not dropped.
func TestGarbageCommandIsAnsweredNotDropped(t *testing.T) {
	ports := freePorts(t, 2)
	control, telemetry := addr(ports[0]), addr(ports[1])

	z := transport.NewZMQ(quietLogger(), func(req *rocsarv1.CommandRequest) *rocsarv1.CommandResponse {
		t.Error("the handler was called for an undecodable command")
		return &rocsarv1.CommandResponse{Success: true}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer z.Stop()

	if err := z.Start(ctx, control, telemetry); err != nil {
		t.Fatalf("Start: %v", err)
	}
	client, err := newDealer(control)
	if err != nil {
		t.Fatal(err)
	}
	defer client.close()

	// 0xFF as a leading byte is field 31 with wire type 7, which does not exist.
	if err := client.sendRaw([]byte{0xFF, 0xFF, 0xFF, 0xFF}); err != nil {
		t.Fatal(err)
	}
	resp, err := client.recv(3 * time.Second)
	if err != nil {
		t.Fatalf("an undecodable command got no reply: %v", err)
	}
	if resp.GetSuccess() {
		t.Error("an undecodable command reported success")
	}
	if resp.GetError() != rocsarv1.ErrorCode_ERROR_INVALID_COMMAND {
		t.Errorf("error = %s, want ERROR_INVALID_COMMAND", resp.GetError())
	}
}

// Telemetry must reach a SUB subscriber as [topic, frame].
func TestTelemetryPublishesWithItsTopicFrame(t *testing.T) {
	ports := freePorts(t, 2)
	control, telemetry := addr(ports[0]), addr(ports[1])

	z := transport.NewZMQ(quietLogger(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer z.Stop()

	if err := z.Start(ctx, control, telemetry); err != nil {
		t.Fatalf("Start: %v", err)
	}

	sub, err := newSub(telemetry)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.close()
	sub.subscribe("")

	frame := &rocsarv1.TelemetryFrame{
		Sequence: 99,
		System:   &rocsarv1.SystemStatus{UptimeS: 12.5, CpuTempC: 44.25},
	}
	body, err := proto.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}

	// A PUB/SUB subscriber that connects mid-stream receives the queued backlog,
	// so publishing is repeated until one arrives. Discarding the first N frames
	// is a protocol requirement on the Ground Station, not a workaround here.
	deadline := time.Now().Add(5 * time.Second)
	var got *rocsarv1.TelemetryFrame
	var gotTopic string
	for time.Now().Before(deadline) && got == nil {
		z.Publish("telemetry.system", body)
		gotTopic, got = sub.recv(200 * time.Millisecond)
	}
	if got == nil {
		t.Fatal("no telemetry arrived on the PUB socket")
	}
	if gotTopic != "telemetry.system" {
		t.Errorf("topic frame = %q, want telemetry.system", gotTopic)
	}
	if got.GetSequence() != 99 || got.GetSystem().GetCpuTempC() != 44.25 {
		t.Errorf("telemetry arrived damaged: %v", got)
	}
}

// The topic must be its own leading frame, so a SUBSCRIBE filter on it is exact.
// ZeroMQ filters on frame PREFIX, and a topic that shares a frame with the
// payload cannot be filtered exactly.
func TestTopicIsItsOwnLeadingFrame(t *testing.T) {
	ports := freePorts(t, 2)
	control, telemetry := addr(ports[0]), addr(ports[1])

	z := transport.NewZMQ(quietLogger(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer z.Stop()
	if err := z.Start(ctx, control, telemetry); err != nil {
		t.Fatalf("Start: %v", err)
	}

	sub, err := newSub(telemetry)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.close()

	// Subscribe narrowly. If the topic shared a frame with the payload, this
	// filter would also match every other topic and the test would see frames
	// it did not ask for.
	sub.subscribe("telemetry.sdr")

	z.Publish("telemetry.system", []byte("system"))
	z.Publish("telemetry.sdr", []byte("sdr"))

	deadline := time.Now().Add(5 * time.Second)
	var topics []string
	for time.Now().Before(deadline) && len(topics) < 1 {
		z.Publish("telemetry.sdr", []byte("sdr"))
		if topic, _ := sub.recv(150 * time.Millisecond); topic != "" {
			topics = append(topics, topic)
		}
	}
	for _, topic := range topics {
		if topic != "telemetry.sdr" {
			t.Errorf("a subscriber filtered to telemetry.sdr received %q; the topic is not a standalone frame", topic)
		}
	}
}

// Binding the same port twice must fail loudly. An OBC that silently failed to
// bind would leave an operator with no link and no error.
func TestBindingAnAlreadyUsedPortFails(t *testing.T) {
	ports := freePorts(t, 2)
	control, telemetry := addr(ports[0]), addr(ports[1])

	z := transport.NewZMQ(quietLogger(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer z.Stop()

	if err := z.Start(ctx, control, telemetry); err != nil {
		t.Fatalf("first Start: %v", err)
	}

	second := transport.NewZMQ(quietLogger(), nil)
	defer second.Stop()
	if err := second.Start(ctx, control, telemetry); err == nil {
		t.Fatal("binding an already-used port succeeded; an OBC with no link and no error is the worst outcome")
	}
}

// Shutdown must be prompt. Without a receive timeout the sockets are closed to
// unblock Recv, and if that path deadlocks the OBC never exits.
func TestShutdownIsPrompt(t *testing.T) {
	ports := freePorts(t, 2)
	z := transport.NewZMQ(quietLogger(), func(*rocsarv1.CommandRequest) *rocsarv1.CommandResponse {
		return &rocsarv1.CommandResponse{Success: true}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := z.Start(ctx, addr(ports[0]), addr(ports[1])); err != nil {
		t.Fatalf("Start: %v", err)
	}

	done := make(chan struct{})
	go func() { z.Stop(); close(done) }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return within 5s; the socket goroutines are wedged")
	}
	if z.Running() {
		t.Error("Running() is true after Stop")
	}
}

func addr(port int) string { return "tcp://127.0.0.1:" + itoa(port) }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// Publishing before Start, or after Stop, must be a no-op rather than a panic or
// a send on a closed socket.
func TestPublishOutsideTheLifecycleIsSafe(t *testing.T) {
	z := transport.NewZMQ(quietLogger(), nil)
	z.Publish("telemetry.system", []byte("before start"))

	ports := freePorts(t, 2)
	ctx, cancel := context.WithCancel(context.Background())
	if err := z.Start(ctx, addr(ports[0]), addr(ports[1])); err != nil {
		t.Fatalf("Start: %v", err)
	}
	z.Stop()
	z.Publish("telemetry.system", []byte("after stop"))

	cancel()
}
