package pico

import (
	"errors"
	"testing"

	rocsarv1 "github.com/rocsar/obc/api/rocsar/v1"
)

// The host sends bare PicoCommand payloads with no PicoMessage envelope; the
// sketch decodes them with rocsar_v1_PicoCommand_fields straight onto the
// deframed buffer. The envelope is the other direction only.
//
// That asymmetry is the whole reason this file exists. PicoCommand and
// PicoMessage share field 1, so decoding a command with DecodeMessage does not
// fail at the protobuf layer -- it produces an envelope with no payload set,
// which reads exactly like a dead link. Mock.respond made that mistake and every
// command through a mocked flight controller failed, unnoticed, because no test
// drove a command through the mock.

func TestEncodeCommandRoundTripsThroughDecodeCommand(t *testing.T) {
	cases := []struct {
		name string
		cmd  *rocsarv1.PicoCommand
	}{
		{"set_target", &rocsarv1.PicoCommand{
			Sequence: 7,
			Payload:  &rocsarv1.PicoCommand_SetTarget{SetTarget: &rocsarv1.SetTargetCommand{TargetHeadingDeg: 123.5}},
		}},
		{"jog", &rocsarv1.PicoCommand{
			Sequence: 8,
			Payload:  &rocsarv1.PicoCommand_Jog{Jog: &rocsarv1.JogCommand{ServoId: 2, Tick: 4095}},
		}},
		{"zero", &rocsarv1.PicoCommand{
			Sequence: 9,
			Payload:  &rocsarv1.PicoCommand_Zero{Zero: &rocsarv1.ZeroCommand{ServoId: 1}},
		}},
		{"mount", &rocsarv1.PicoCommand{
			Sequence: 10,
			Payload:  &rocsarv1.PicoCommand_Mount{Mount: &rocsarv1.MountCommand{ServoId: 1, OffsetDeg: -2.25}},
		}},
		{"dir", &rocsarv1.PicoCommand{
			Sequence: 11,
			Payload:  &rocsarv1.PicoCommand_Dir{Dir: &rocsarv1.DirCommand{ServoId: 1, Multiplier: -1}},
		}},
		{"heater", &rocsarv1.PicoCommand{
			Sequence: 12,
			Payload:  &rocsarv1.PicoCommand_Heater{Heater: &rocsarv1.HeaterCommand{HeaterId: 2, State: true}},
		}},
		{"stop", &rocsarv1.PicoCommand{
			Sequence: 13,
			Payload:  &rocsarv1.PicoCommand_Stop{Stop: &rocsarv1.StopCommand{ServoId: 2}},
		}},
		{"status_request", &rocsarv1.PicoCommand{
			Sequence: 14,
			Payload:  &rocsarv1.PicoCommand_StatusRequest{StatusRequest: &rocsarv1.StatusRequestCommand{}},
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			frame, err := EncodeCommand(tc.cmd)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if frame[len(frame)-1] != Delimiter {
				t.Fatalf("frame is not delimited")
			}

			got, err := DecodeCommand(frame[:len(frame)-1])
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got.GetSequence() != tc.cmd.GetSequence() {
				t.Errorf("sequence %d came back as %d", tc.cmd.GetSequence(), got.GetSequence())
			}
			if !samePayload(got, tc.cmd) {
				t.Errorf("payload did not survive: got %T, want %T", got.GetPayload(), tc.cmd.GetPayload())
			}
		})
	}
}

// samePayload reports whether two commands name the same oneof arm with the same
// body. Comparing the concrete wrapper types is what makes a mis-routed arm --
// a jog decoded as a zero, say -- a failure rather than a silently equal frame.
func samePayload(a, b *rocsarv1.PicoCommand) bool {
	switch pa := a.GetPayload().(type) {
	case *rocsarv1.PicoCommand_SetTarget:
		pb, ok := b.GetPayload().(*rocsarv1.PicoCommand_SetTarget)
		return ok && pa.SetTarget.GetTargetHeadingDeg() == pb.SetTarget.GetTargetHeadingDeg()
	case *rocsarv1.PicoCommand_Jog:
		pb, ok := b.GetPayload().(*rocsarv1.PicoCommand_Jog)
		return ok && pa.Jog.GetServoId() == pb.Jog.GetServoId() && pa.Jog.GetTick() == pb.Jog.GetTick()
	case *rocsarv1.PicoCommand_Zero:
		pb, ok := b.GetPayload().(*rocsarv1.PicoCommand_Zero)
		return ok && pa.Zero.GetServoId() == pb.Zero.GetServoId()
	case *rocsarv1.PicoCommand_Mount:
		pb, ok := b.GetPayload().(*rocsarv1.PicoCommand_Mount)
		return ok && pa.Mount.GetServoId() == pb.Mount.GetServoId() &&
			pa.Mount.GetOffsetDeg() == pb.Mount.GetOffsetDeg()
	case *rocsarv1.PicoCommand_Dir:
		pb, ok := b.GetPayload().(*rocsarv1.PicoCommand_Dir)
		return ok && pa.Dir.GetServoId() == pb.Dir.GetServoId() &&
			pa.Dir.GetMultiplier() == pb.Dir.GetMultiplier()
	case *rocsarv1.PicoCommand_Heater:
		pb, ok := b.GetPayload().(*rocsarv1.PicoCommand_Heater)
		return ok && pa.Heater.GetHeaterId() == pb.Heater.GetHeaterId() && pa.Heater.GetState() == pb.Heater.GetState()
	case *rocsarv1.PicoCommand_Stop:
		pb, ok := b.GetPayload().(*rocsarv1.PicoCommand_Stop)
		return ok && pa.Stop.GetServoId() == pb.Stop.GetServoId()
	case *rocsarv1.PicoCommand_StatusRequest:
		_, ok := b.GetPayload().(*rocsarv1.PicoCommand_StatusRequest)
		return ok
	}
	return false
}

// TestDecodeMessageDoesNotAcceptACommand is the guard on the mistake itself.
// DecodeMessage is the right decoder for the Pico-to-Pi direction and the wrong
// one for this direction, and the two are easy to swap because both take a
// deframed block.
func TestDecodeMessageDoesNotAcceptACommand(t *testing.T) {
	frame, err := EncodeCommand(&rocsarv1.PicoCommand{
		Sequence: 3,
		Payload:  &rocsarv1.PicoCommand_SetTarget{SetTarget: &rocsarv1.SetTargetCommand{TargetHeadingDeg: 1}},
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if _, err := DecodeMessage(frame[:len(frame)-1]); !errors.Is(err, ErrNoPayload) {
		t.Fatalf("decoding a command as an envelope returned %v, want ErrNoPayload", err)
	}
}

func TestEncodeCommandRefusesWhatItCannotCarry(t *testing.T) {
	if _, err := EncodeCommand(nil); err == nil {
		t.Error("a nil command was encoded")
	}
	if _, err := EncodeCommand(&rocsarv1.PicoCommand{Sequence: 1}); !errors.Is(err, ErrNoPayload) {
		t.Errorf("a command with no payload returned %v, want ErrNoPayload", err)
	}
	// Sequence 0 means the caller forgot to assign one, and an ACK for 0 would be
	// indistinguishable from a wrapped counter.
	if _, err := EncodeCommand(&rocsarv1.PicoCommand{
		Payload: &rocsarv1.PicoCommand_Stop{Stop: &rocsarv1.StopCommand{ServoId: 1}},
	}); err == nil {
		t.Error("a command with sequence 0 was encoded")
	}
}

func TestDecodeCommandRefusesAnEmptyOneof(t *testing.T) {
	// What the firmware would answer with ERROR_INVALID_COMMAND, refused here
	// where the reason can still be named.
	frame := CobsEncode(nil, []byte{0x08, 0x01}) // field 1 varint 1, nothing else
	if _, err := DecodeCommand(frame); !errors.Is(err, ErrNoPayload) {
		t.Errorf("a block with no oneof returned %v, want ErrNoPayload", err)
	}
}

// The mock exists so bench work can use a Pico-shaped object without one. It is
// not a stand-in for an absent device -- see openPico in cmd/obc -- so the one
// thing it must get right is that it answers.
func TestMockAnswersEveryCommandItAccepts(t *testing.T) {
	m := NewMock()

	acks := map[string]func() error{
		"set_target": func() error { _, err := m.SetTarget(t.Context(), 90); return err },
		"jog":        func() error { _, err := m.Jog(t.Context(), 1, 100); return err },
		"zero":       func() error { _, err := m.Zero(t.Context(), 1); return err },
		"mount":      func() error { _, err := m.Mount(t.Context(), 1, 0); return err },
		"dir":        func() error { _, err := m.SetDirection(t.Context(), 1, 1); return err },
		"heater":     func() error { _, err := m.SetHeater(t.Context(), 1, true); return err },
		"stop":       func() error { _, err := m.Stop(t.Context(), 1); return err },
	}

	for name, call := range acks {
		if err := call(); err != nil {
			t.Errorf("%s through the mock: %v", name, err)
		}
	}
}
