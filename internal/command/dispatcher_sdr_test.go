package command

import (
	"context"
	"encoding/json"
	"testing"

	rocsarv1 "github.com/rocsar/obc/api/rocsar/v1"
	"github.com/rocsar/obc/internal/sdr"
)

// sdr_get_params is the read side of sdr_set_params: the Ground Station shows
// current values as placeholder hints while blank inputs keep meaning "leave
// alone". The reply carries the params as JSON in message, following the
// sdr_probe verbatim-output precedent -- not new response fields.
func TestSdrGetParamsReturnsCurrentValuesAsJSON(t *testing.T) {
	d := New(Deps{SDR: sdr.NewMock(), Log: discardLog()})

	resp := d.Handle(context.Background(), &rocsarv1.CommandRequest{
		RequestId: "req-params-1",
		Payload: &rocsarv1.CommandRequest_SdrGetParams{
			SdrGetParams: &rocsarv1.SdrGetParamsCommand{},
		},
	})
	if !resp.GetSuccess() {
		t.Fatalf("sdr_get_params failed: %s %s", resp.GetError(), resp.GetMessage())
	}
	if resp.GetRequestId() != "req-params-1" {
		t.Errorf("request_id = %q, want it echoed", resp.GetRequestId())
	}

	// The keys are the params.json keys, not a second vocabulary. A GUI that
	// maps PRFHz itself has two names for one value and they will drift.
	var got map[string]any
	if err := json.Unmarshal([]byte(resp.GetMessage()), &got); err != nil {
		t.Fatalf("message is not JSON params: %v (message: %q)", err, resp.GetMessage())
	}
	if got["PRF"] != 2750.0 {
		t.Errorf("PRF = %v, want 2750 (the mock's deterministic value)", got["PRF"])
	}
	if got["TX_FREQ"] != 5.8e9 {
		t.Errorf("TX_FREQ = %v, want 5.8e9", got["TX_FREQ"])
	}
}

// Without an SDR the command fails distinctly from a timeout: this is "there
// is nothing to talk to", not "it is present but not answering".
func TestSdrGetParamsWithoutSDRIsNotConnected(t *testing.T) {
	d := New(Deps{Log: discardLog()})

	resp := d.Handle(context.Background(), &rocsarv1.CommandRequest{
		RequestId: "req-params-2",
		Payload: &rocsarv1.CommandRequest_SdrGetParams{
			SdrGetParams: &rocsarv1.SdrGetParamsCommand{},
		},
	})
	if resp.GetSuccess() {
		t.Fatal("sdr_get_params succeeded with no SDR configured")
	}
	if resp.GetError() != rocsarv1.ErrorCode_ERROR_NOT_CONNECTED {
		t.Errorf("error = %s, want ERROR_NOT_CONNECTED", resp.GetError())
	}
}
