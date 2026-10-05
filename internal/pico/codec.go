package pico

import (
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	rocsarv1 "github.com/rocsar/obc/api/rocsar/v1"
)

// This is the Go half of firmware/pico_wire.h. The two must agree byte for
// byte; test/pico_wire_test.go checks the Go encoder against the firmware's own C
// encoder, so a divergence is a failing test rather than a link that works on
// the bench and not in flight.

const (
	// MaxPayload bounds the protobuf payload.
	//
	// This is a hardware fact, not a preference. The RP2040's pico_wire.h uses
	// the same number for its transmit buffer, and the frame the OBC is willing
	// to receive has to fit the same envelope.
	MaxPayload = 256

	// MaxFrame is the largest frame including the trailing 0x00 delimiter.
	//
	// It must equal firmware/pico_wire.h's PICO_TX_FRAME_MAX, which is
	// COBS_MAX(PICO_TX_BUFFER) + 1 = (256 + 256/254 + 1) + 1 = 259. Written as a
	// literal because Go cannot call a function in a const expression;
	// TestMaxFrameMatchesTheFormula asserts it still agrees with
	// CobsEncodedSize(MaxPayload)+1, so the derivation cannot drift silently
	// when MaxPayload changes.
	MaxFrame = 259

	// Delimiter terminates every frame. It is the only 0x00 in the stream,
	// which is the property COBS exists to guarantee.
	Delimiter = 0x00
)

// Codec errors.
var (
	ErrPayloadTooLarge = errors.New("pico: payload exceeds the frame budget")
	ErrNoPayload       = errors.New("pico: message has no payload set")
)

// EncodeMessage serialises msg into a COBS frame with its trailing delimiter,
// appending to dst.
//
// The size check is explicit and returns an error rather than truncating. The
// firmware's cobs_encode does not bounds-check its own output buffer, so an
// oversized message that reached it would write past the end of a stack array --
// which on an RP2040 is a silent corruption of whatever follows, not a crash.
func EncodeMessage(dst []byte, msg *rocsarv1.PicoMessage) ([]byte, error) {
	if msg == nil {
		return nil, errors.New("pico: nil message")
	}
	if msg.GetPayload() == nil {
		// A PicoMessage with an empty oneof is meaningless -- it says nothing
		// and the receiver would have nothing to route. Sending it would look
		// like a live link carrying no data.
		return nil, ErrNoPayload
	}

	payload, err := proto.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	if len(payload) > MaxPayload {
		return nil, fmt.Errorf("%w: %d bytes, budget %d", ErrPayloadTooLarge, len(payload), MaxPayload)
	}

	frame := CobsEncode(dst, payload)
	if len(frame)+1 > MaxFrame {
		return nil, fmt.Errorf("%w: frame %d bytes, budget %d", ErrPayloadTooLarge, len(frame)+1, MaxFrame)
	}

	return append(frame, Delimiter), nil
}

// DecodeMessage parses the protobuf payload of one already-deframed block.
//
// cobs must be the bytes BETWEEN delimiters, delimiter excluded.
func DecodeMessage(cobs []byte) (*rocsarv1.PicoMessage, error) {
	payload, err := CobsDecode(cobs)
	if err != nil {
		return nil, fmt.Errorf("deframe: %w", err)
	}

	msg := &rocsarv1.PicoMessage{}
	if err := proto.Unmarshal(payload, msg); err != nil {
		// A frame that survives COBS but fails protobuf is a sender bug or a
		// desynchronised stream. Either way it is not a command, so it is
		// reported and never partially applied.
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	if msg.GetPayload() == nil {
		return nil, ErrNoPayload
	}
	return msg, nil
}

// EncodeCommand serialises a PicoCommand directly into a COBS frame.
//
// Not wrapped in PicoMessage. The envelope is the Pico-to-Pi direction only --
// it carries what the flight controller reports and acknowledges. The Pi's
// commands are bare PicoCommand payloads, which is what the sketch decodes:
// `pb_decode(&stream, rocsar_v1_PicoCommand_fields, &cmd)` straight onto the
// deframed buffer, with no envelope and no timestamp.
//
// A command frame carries no timestamp because the RP2040 has no reason to
// trust one from the Pi: it timestamps its own telemetry with micros() and
// correlates on the sequence number alone.
func EncodeCommand(cmd *rocsarv1.PicoCommand) ([]byte, error) {
	if cmd == nil {
		return nil, errors.New("pico: nil command")
	}
	if cmd.GetPayload() == nil {
		return nil, ErrNoPayload
	}
	if cmd.GetSequence() == 0 {
		// Sequence 0 is a valid uint32 but the counter starts at 1. A frame
		// carrying 0 means the caller forgot to assign one, and an ACK for
		// sequence 0 would be indistinguishable from a wrapped-around counter.
		return nil, errors.New("pico: command sequence must be set by the caller")
	}

	payload, err := proto.Marshal(cmd)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	if len(payload) > MaxPayload {
		return nil, fmt.Errorf("%w: %d bytes, budget %d", ErrPayloadTooLarge, len(payload), MaxPayload)
	}

	frame := CobsEncode(nil, payload)
	if len(frame)+1 > MaxFrame {
		return nil, fmt.Errorf("%w: frame %d bytes, budget %d", ErrPayloadTooLarge, len(frame)+1, MaxFrame)
	}
	return append(frame, Delimiter), nil
}

// DecodeCommand parses the protobuf payload of one already-deframed command
// block.
//
// The exact mirror of EncodeCommand: cobs must be the bytes BETWEEN delimiters,
// delimiter excluded, and the payload is a bare PicoCommand.
//
// DecodeMessage is NOT a substitute, and using it here is a bug that reads like
// a dead link. The two payloads share field 1 (sequence), so decoding a command
// as an envelope succeeds at the protobuf layer and then fails the envelope's
// own payload check -- "message has no payload set" -- which is what Mock.respond
// did. Every command through a mocked flight controller failed, and
// `obc --mock-pico` could not command anything.
func DecodeCommand(cobs []byte) (*rocsarv1.PicoCommand, error) {
	payload, err := CobsDecode(cobs)
	if err != nil {
		return nil, fmt.Errorf("deframe: %w", err)
	}

	cmd := &rocsarv1.PicoCommand{}
	if err := proto.Unmarshal(payload, cmd); err != nil {
		// A block that survives COBS but fails protobuf is a sender bug or a
		// desynchronised stream. Either way it is not a command, so it is
		// reported and never partially applied.
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	if cmd.GetPayload() == nil {
		// Symmetric with EncodeCommand. An empty oneof names no command, and the
		// firmware answers one with ERROR_INVALID_COMMAND rather than doing
		// nothing, so the decoder refuses it here where it can still be named.
		return nil, ErrNoPayload
	}
	return cmd, nil
}

// EncodeTelemetryRequest asks the flight controller to prove the link.
//
// The name is historical: it asks for nothing. The sketch's status_request arm
// changes no state -- it acknowledges, and the acknowledgement is the proof -- so
// this is a liveness ping, not a request for a report. State arrives on the
// flight controller's own 50 Hz telemetry stream either way.
//
// Open() uses it because opening a serial port successfully is not evidence that
// a device is on the other end of it: an unplugged Pico leaves the port openable
// on Linux for a while.
func EncodeTelemetryRequest(sequence uint32) ([]byte, error) {
	return EncodeCommand(&rocsarv1.PicoCommand{
		Sequence: sequence,
		Payload:  &rocsarv1.PicoCommand_StatusRequest{StatusRequest: &rocsarv1.StatusRequestCommand{}},
	})
}

// AckFromMessage extracts the ACK, or nil if the frame carried telemetry.
//
// Distinguishing "not an ack" from "a malformed ack" matters: a telemetry frame
// arriving where an ACK was expected means the sequence numbers have
// desynchronised, which is a different fault from a corrupt payload.
func AckFromMessage(msg *rocsarv1.PicoMessage) *rocsarv1.PicoAck {
	if msg == nil {
		return nil
	}
	return msg.GetAck()
}

// TelemetryFromMessage extracts telemetry, or nil if the frame carried an ACK.
func TelemetryFromMessage(msg *rocsarv1.PicoMessage) *rocsarv1.PicoTelemetry {
	if msg == nil {
		return nil
	}
	return msg.GetTelemetry()
}
