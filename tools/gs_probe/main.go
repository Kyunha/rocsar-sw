// Command gs_probe is the Ground Station's side of the link, without the UI.
//
// It is the cross-check that the automated tests cannot make: everything in
// test/ is Go talking to Go, which proves OUR side is correct but says nothing
// about whether pyzmq can talk to this ZeroMQ implementation. tools/gs_probe.py
// is the same probe in Python and is the one that settles it.
//
// Two things it deliberately does NOT do:
//
//   - It does not pretend to be the Ground Station. There is no PySide6 here,
//     no view model, no reconnect policy. It is a probe, not a client.
//   - It does not read a topic-prefixed frame positionally without saying so.
//     ZeroMQ filters on frame PREFIX, which is why the OBC puts the topic in its
//     own leading frame; this reads the payload from the last frame because the
//     topic's presence as a separate frame is exactly what is being checked.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/go-zeromq/zmq4"
	"google.golang.org/protobuf/proto"

	rocsarv1 "github.com/rocsar/obc/api/rocsar/v1"
)

func main() {
	control := flag.String("control", "tcp://127.0.0.1:5555", "OBC control endpoint (ROUTER)")
	telemetry := flag.String("telemetry", "tcp://127.0.0.1:5556", "OBC telemetry endpoint (PUB)")
	frames := flag.Int("frames", 5, "how many telemetry frames to wait for")
	seconds := flag.Int("seconds", 10, "give up after this long")
	topic := flag.String("topic", "", "subscribe only to this topic prefix (default: everything)")
	photo := flag.Bool("take-photo", false, "send a take_photo command once telemetry arrives")
	flag.Parse()

	if err := probe(*control, *telemetry, *topic, *frames, *seconds, *photo); err != nil {
		fmt.Fprintf(os.Stderr, "gs_probe: %v\n", err)
		os.Exit(1)
	}
}

func probe(control, telemetry, topic string, frames, seconds int, photo bool) error {
	ctx := context.Background()

	sub := zmq4.NewSub(ctx, zmq4.WithTimeout(time.Duration(seconds)*time.Second))
	if err := sub.Dial(telemetry); err != nil {
		return fmt.Errorf("dial telemetry: %w", err)
	}
	defer sub.Close()
	if err := sub.SetOption(string(zmq4.OptionSubscribe), topic); err != nil {
		return fmt.Errorf("subscribe %q: %w", topic, err)
	}

	dealer := zmq4.NewDealer(ctx,
		zmq4.WithID(zmq4.SocketIdentity(fmt.Sprintf("gs-probe-%d", time.Now().UnixNano()))),
		zmq4.WithTimeout(time.Duration(seconds)*time.Second))
	if err := dealer.Dial(control); err != nil {
		return fmt.Errorf("dial control: %w", err)
	}
	defer dealer.Close()

	type reply struct {
		resp *rocsarv1.CommandResponse
		err  error
	}
	replies := make(chan reply, 4)
	go func() {
		for {
			msg, err := dealer.Recv()
			if err != nil {
				close(replies)
				return
			}
			r := &rocsarv1.CommandResponse{}
			if err := proto.Unmarshal(msg.Frames[len(msg.Frames)-1], r); err != nil {
				continue
			}
			replies <- reply{resp: r}
		}
	}()

	fmt.Printf("subscribed to %s\n  control:   %s\n  telemetry: %s\n", topic, control, telemetry)

	deadline := time.Now().Add(time.Duration(seconds) * time.Second)
	seen := 0
	sentPhoto := false

	for time.Now().Before(deadline) && seen < frames {
		msg, err := sub.Recv()
		if err != nil {
			return fmt.Errorf("no telemetry within %ds", seconds)
		}
		if len(msg.Frames) < 2 {
			continue
		}

		gotTopic := string(msg.Frames[0])
		f := &rocsarv1.TelemetryFrame{}
		if err := proto.Unmarshal(msg.Frames[len(msg.Frames)-1], f); err != nil {
			continue
		}
		seen++

		fmt.Printf("[%d] topic=%-18q seq=%-4d gnss=%d pico=%v mocked=%v cpu=%.1f%%C link=%v\n",
			seen, gotTopic, f.GetSequence(), len(f.GetGnss()), f.GetPicoConnected(),
			f.GetSystem().GetMockedSubsystems(), f.GetSystem().GetCpuTempC(),
			f.GetLink().GetShapingActive())

		for _, g := range f.GetGnss() {
			marker := " "
			if g.GetSelected() {
				marker = "*"
			}
			fmt.Printf("      %s gnss %d  fix=%-5v lat=%9.5f lon=%10.5f alt=%7.1f spd=%5.1f  ok=%d rej=%d\n",
				marker, g.GetReceiverId(), g.GetFixOk(), g.GetLatitudeDeg(),
				g.GetLongitudeDeg(), g.GetAltitudeM(), g.GetGroundSpeedMps(),
				g.GetPacketsAccepted(), g.GetPacketsRejected())
		}
		if p := f.GetPico(); p != nil {
			fmt.Printf("        pico heading=%.2f target=%.2f imu=%v heaters=%v/%v\n",
				p.GetGondolaHeadingDeg(), p.GetTargetHeadingDeg(), p.GetImuPresent(),
				p.GetHeater1State(), p.GetHeater2State())
			for _, a := range p.GetAntennas() {
				fmt.Printf("          axis %d tick=%-5d angle=%8.3f load=%5d temp=%3dC feedback=%d error=%d\n",
					a.GetServoId(), a.GetCurrentTick(), a.GetCurrentAngleDeg(),
					a.GetLoad(), a.GetTemperatureC(), a.GetFeedbackState(), a.GetFeedbackError())
			}
		}

		if photo && !sentPhoto {
			sentPhoto = true
			req := &rocsarv1.CommandRequest{
				RequestId: "probe-take-photo",
				Payload:   &rocsarv1.CommandRequest_TakePhoto{TakePhoto: &rocsarv1.TakePhotoCommand{}},
			}
			body, err := proto.Marshal(req)
			if err != nil {
				return err
			}
			// The DEALER envelope a ROUTER expects: empty delimiter, payload.
			if err := dealer.Send(zmq4.NewMsgFrom([]byte(""), body)); err != nil {
				return fmt.Errorf("send take_photo: %w", err)
			}
			fmt.Println("        -> sent take_photo")
		}
	}

	if photo {
		select {
		case r, ok := <-replies:
			if !ok {
				return fmt.Errorf("the control connection closed before a reply arrived")
			}
			fmt.Printf("\nreply %q success=%v error=%s\n", r.resp.GetRequestId(),
				r.resp.GetSuccess(), r.resp.GetError())
			if r.resp.GetArtefactName() != "" {
				fmt.Printf("  artefact: %s (%d bytes, %s) -- fetch over HTTP :5557\n",
					r.resp.GetArtefactName(), r.resp.GetArtefactSizeBytes(), r.resp.GetArtefactKind())
			}
			if msg := r.resp.GetMessage(); msg != "" {
				fmt.Printf("  message:  %s\n", msg)
			}
		case <-time.After(5 * time.Second):
			return fmt.Errorf("take_photo was sent but never answered")
		}
	}

	fmt.Printf("\nOK: %d telemetry frames\n", seen)
	return nil
}
