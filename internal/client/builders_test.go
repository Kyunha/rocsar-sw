package client

import (
	"strconv"
	"strings"
	"testing"

	rocsarv1 "github.com/rocsar/obc/api/rocsar/v1"
)

// Moved here from tools/gs_cli unchanged when the client was promoted out of
// package main. The cases were written against real commands reaching real
// servos; none of them is decorative.

// Arguments are validated before a command is sent, not after.
//
// The reason is physical: a jog to tick 9999 is refused by the firmware, but it
// has already travelled to the flight controller and back. Catching it here means
// the operator finds out before anything moves.
func TestArgumentsAreRefusedBeforeSending(t *testing.T) {
	for _, tc := range []struct {
		name string
		cmd  string
		args []string
		want string
	}{
		{"tick above the ST3215 range", "jog", []string{"1", "4096"}, "ST3215 range"},
		{"tick below zero is not a whole number", "jog", []string{"1", "-1"}, "not a whole number"},
		{"heater id out of range", "heater", []string{"3", "on"}, "1 or 2"},
		{"heater state is not on or off", "heater", []string{"1", "1"}, "on or off"},
		{"direction is not +/-1", "dir", []string{"1", "0"}, "+1 or -1"},
		{"missing arguments", "jog", []string{"1"}, "needs 2"},
		{"non-numeric receiver", "gnss", []string{"x"}, "not a whole number"},
		{"receiver ids start at 1", "gnss", []string{"0"}, "start at 1"},
		{"zero link rate", "link", []string{"0"}, "greater than zero"},
		{"unknown command", "teleport", nil, "unknown command"},
		{"heading is not a number", "heading", []string{"north"}, "not a number"},
	} {
		_, err := BuildRequests(tc.cmd, tc.args)
		if err == nil {
			t.Errorf("%s: %s %v was accepted", tc.name, tc.cmd, tc.args)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q does not mention %q, so it does not say what to fix",
				tc.name, err, tc.want)
		}
	}
}

func TestValidArgumentsBuildTheRightCommand(t *testing.T) {
	for _, tc := range []struct {
		name string
		cmd  string
		args []string
		want func(*testing.T, *rocsarv1.CommandRequest)
	}{
		{"jog", "jog", []string{"2", "3000"}, func(t *testing.T, r *rocsarv1.CommandRequest) {
			j, ok := r.GetPayload().(*rocsarv1.CommandRequest_Pico)
			if !ok {
				t.Fatalf("payload is %T, want a Pico command", r.GetPayload())
			}
			jog, ok := j.Pico.GetPayload().(*rocsarv1.PicoCommand_Jog)
			if !ok {
				t.Fatalf("Pico payload is %T, want Jog", j.Pico.GetPayload())
			}
			if jog.Jog.GetServoId() != 2 || jog.Jog.GetTick() != 3000 {
				t.Errorf("jog servo=%d tick=%d, want servo 2 tick 3000",
					jog.Jog.GetServoId(), jog.Jog.GetTick())
			}
		}},
		{"dir", "dir", []string{"1", "-1"}, func(t *testing.T, r *rocsarv1.CommandRequest) {
			j := r.GetPayload().(*rocsarv1.CommandRequest_Pico)
			d, ok := j.Pico.GetPayload().(*rocsarv1.PicoCommand_Dir)
			if !ok {
				t.Fatalf("Pico payload is %T, want Dir", j.Pico.GetPayload())
			}
			if d.Dir.GetMultiplier() != -1 {
				t.Errorf("multiplier %v, want -1", d.Dir.GetMultiplier())
			}
		}},
		{"heater", "heater", []string{"2", "on"}, func(t *testing.T, r *rocsarv1.CommandRequest) {
			j := r.GetPayload().(*rocsarv1.CommandRequest_Pico)
			h, ok := j.Pico.GetPayload().(*rocsarv1.PicoCommand_Heater)
			if !ok {
				t.Fatalf("Pico payload is %T, want Heater", j.Pico.GetPayload())
			}
			if h.Heater.GetHeaterId() != 2 || !h.Heater.GetState() {
				t.Errorf("heater id=%d state=%v, want 2 on", h.Heater.GetHeaterId(), h.Heater.GetState())
			}
		}},
		{"gnss", "gnss", []string{"3"}, func(t *testing.T, r *rocsarv1.CommandRequest) {
			g, ok := r.GetPayload().(*rocsarv1.CommandRequest_GnssSelect)
			if !ok {
				t.Fatalf("payload is %T, want GnssSelect", r.GetPayload())
			}
			if g.GnssSelect.GetReceiverId() != 3 {
				t.Errorf("receiver %d, want 3", g.GnssSelect.GetReceiverId())
			}
		}},
		{"gnss-rotate", "gnss-rotate", nil, func(t *testing.T, r *rocsarv1.CommandRequest) {
			g, ok := r.GetPayload().(*rocsarv1.CommandRequest_GnssSelect)
			if !ok {
				t.Fatalf("payload is %T, want GnssSelect", r.GetPayload())
			}
			if !g.GnssSelect.GetRotate() {
				t.Error("rotate flag not set; the window's next-receiver button needs it")
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reqs, err := BuildRequests(tc.cmd, tc.args)
			if err != nil {
				t.Fatalf("%s %v: %v", tc.cmd, tc.args, err)
			}
			if len(reqs) != 1 {
				t.Fatalf("%s produced %d requests, want 1", tc.cmd, len(reqs))
			}
			tc.want(t, reqs[0])
			if reqs[0].GetRequestId() == "" {
				t.Error("request_id is empty; a reply could not be matched to this command")
			}
		})
	}
}

// A partial SDR update names only the fields it changes; absent stays absent,
// because a PRF of zero is not a request to set the PRF to zero. An all-nil
// patch is refused rather than sent as a no-op: the operator pressed the
// button expecting a change.
func TestSetParamsPatchIsPartial(t *testing.T) {
	prf := 2750.0
	req, err := BuildSetParamsRequest(SdrParamsPatch{PRFHz: &prf})
	if err != nil {
		t.Fatalf("partial patch refused: %v", err)
	}
	got, ok := req.GetPayload().(*rocsarv1.CommandRequest_SdrSetParams)
	if !ok {
		t.Fatalf("payload is %T, want SdrSetParams", req.GetPayload())
	}
	params := got.SdrSetParams.GetParams()
	if params.GetPrfHz() != 2750.0 {
		t.Errorf("prf = %v, want 2750", params.GetPrfHz())
	}
	if params.SampleRateHz != nil || params.TxFreqHz != nil ||
		params.NormalizedGainTx != nil || params.NormalizedGainRx != nil ||
		params.BandwidthHz != nil || params.SessionDurationS != nil {
		t.Errorf("unset fields became present: %+v", params)
	}
	if req.GetRequestId() == "" {
		t.Error("request_id is empty; a reply could not be matched to this command")
	}

	if _, err := BuildSetParamsRequest(SdrParamsPatch{}); err == nil {
		t.Error("an empty patch was accepted; it changes nothing and reports success")
	}
}

// `zero` centres both axes, which is two commands. Collapsing it into one would
// centre a single axis and report success, which is worse than doing nothing.
func TestZeroIsOneRequestPerAxis(t *testing.T) {
	reqs, err := BuildRequests("zero", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(reqs) != 2 {
		t.Fatalf("zero produced %d requests, want one per axis", len(reqs))
	}
	seen := map[uint32]bool{}
	for _, r := range reqs {
		p, ok := r.GetPayload().(*rocsarv1.CommandRequest_Pico)
		if !ok {
			t.Fatalf("payload is %T, want Pico", r.GetPayload())
		}
		z, ok := p.Pico.GetPayload().(*rocsarv1.PicoCommand_Zero)
		if !ok {
			t.Fatalf("Pico payload is %T, want Zero", p.Pico.GetPayload())
		}
		if seen[z.Zero.GetServoId()] {
			t.Errorf("servo %d was sent twice", z.Zero.GetServoId())
		}
		seen[z.Zero.GetServoId()] = true
	}
	if !seen[1] || !seen[2] {
		t.Errorf("zero addressed %v, want both axis 1 and 2", seen)
	}

	// And a named axis sends exactly one.
	reqs, err = BuildRequests("zero", []string{"2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(reqs) != 1 {
		t.Errorf("zero 2 produced %d requests, want 1", len(reqs))
	}
}

// Each request needs a distinct id, or two commands in flight cannot be told
// apart when their replies arrive.
func TestRequestIDsAreDistinct(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		reqs, err := BuildRequests("query", nil)
		if err != nil {
			t.Fatal(err)
		}
		id := reqs[0].GetRequestId()
		if seen[id] {
			t.Fatalf("request_id %q was reused", id)
		}
		seen[id] = true
	}
}

// TestStopAllIsRefusedRatherThanSentAsServoZero guards a bug that shipped.
//
// `stop all` used to be encoded as StopCommand{ServoId: 0}. The firmware
// resolves an axis with findAntenna(), which matches on the id the axis was
// built with, and 0 is not one of them -- so the operator typed "stop
// everything" and got ERROR_INVALID_SERVO, naming a servo they never mentioned.
//
// It cannot be fixed by substituting {1, 2} here. The axis ids come from the
// firmware's ANTENNA_0_SERVO_ID / ANTENNA_1_SERVO_ID, which are overridable at
// build time, so a fixed list would stop the wrong axes on a bench build and look
// like it worked. The builder refuses; the caller expands against what the board
// reports.
func TestStopAllIsRefusedRatherThanSentAsServoZero(t *testing.T) {
	for _, arg := range []string{"all", "ALL", "All"} {
		reqs, err := BuildRequests("stop", []string{arg})
		if err == nil {
			t.Fatalf("stop %q produced %d request(s); it must be refused", arg, len(reqs))
		}
		if !strings.Contains(err.Error(), "axis ids") {
			t.Errorf("stop %q: error %q does not say the caller has to expand it", arg, err)
		}
		for _, r := range reqs {
			if id := r.GetPico().GetStop().GetServoId(); id == 0 {
				t.Errorf("stop %q still put servo_id 0 on the wire", arg)
			}
		}
	}
}

// The refusal must not have cost the per-axis form, which is how both consoles
// actually stop everything.
func TestStopOneAxisStillWorks(t *testing.T) {
	for _, id := range []uint32{1, 2, 3} {
		reqs, err := BuildRequests("stop", []string{strconv.FormatUint(uint64(id), 10)})
		if err != nil {
			t.Fatalf("stop %d: %v", id, err)
		}
		if got := reqs[0].GetPico().GetStop().GetServoId(); got != id {
			t.Errorf("stop %d encoded servo_id %d", id, got)
		}
	}
}
