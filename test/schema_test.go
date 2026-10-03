package test

import (
	"testing"

	"google.golang.org/protobuf/proto"

	rocsarv1 "github.com/rocsar/obc/api/rocsar/v1"
)

// The schema compiles and both messages round-trip. This is the smallest test
// that would catch a generator misconfiguration or a field-numbering mistake,
// and it is the first thing that runs.
func TestPicoMessageRoundTrip(t *testing.T) {
	in := &rocsarv1.PicoMessage{
		Sequence:     42,
		TimestampUs:  1_700_000_000_000_000,
		Payload: &rocsarv1.PicoMessage_Telemetry{
			Telemetry: &rocsarv1.PicoTelemetry{
				GondolaHeadingDeg: 123.5,
				TargetHeadingDeg:  90.0,
				ImuPresent:        true,
				Heater1State:      true,
				Antennas: []*rocsarv1.AntennaTelemetry{
					{ServoId: 1, CurrentTick: 2048, FeedbackState: 1, Load: 12},
					{ServoId: 2, CurrentTick: 1024, FeedbackState: 2, Load: -3},
				},
			},
		},
	}

	b, err := proto.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	out := &rocsarv1.PicoMessage{}
	if err := proto.Unmarshal(b, out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !proto.Equal(in, out) {
		t.Fatalf("round trip changed the message:\n in: %v\nout: %v", in, out)
	}

	// The feedback tri-state is int32 on the wire, not the enum, so that an
	// unknown value from a future firmware decodes to a plain integer instead
	// of failing the frame. If this ever becomes an enum this stops compiling,
	// which is the point.
	if got := out.GetTelemetry().GetAntennas()[1].GetFeedbackState(); got != 2 {
		t.Fatalf("feedback_state = %d, want 2 (FEEDBACK_HELD)", got)
	}
}

func TestCommandRequestRoundTrip(t *testing.T) {
	in := &rocsarv1.CommandRequest{
		RequestId: "btn-7",
		Payload: &rocsarv1.CommandRequest_SdrSetParams{
			SdrSetParams: &rocsarv1.SdrSetParamsCommand{
				Params: &rocsarv1.SdrParams{PrfHz: proto.Float64(2750)},
			},
		},
	}

	b, err := proto.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out := &rocsarv1.CommandRequest{}
	if err := proto.Unmarshal(b, out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !proto.Equal(in, out) {
		t.Fatal("round trip changed the message")
	}

	// An absent optional field must stay absent. SdrParams is a partial update:
	// absent means "leave it alone", so conflating unset with the proto3 zero
	// would let a partial update zero every parameter it does not mention.
	if out.GetSdrSetParams().GetParams().SampleRateHz != nil {
		t.Fatal("absent optional fs_hz became present")
	}
	if got := out.GetSdrSetParams().GetParams().GetPrfHz(); got != 2750 {
		t.Fatalf("prf_hz = %v, want 2750", got)
	}
}
