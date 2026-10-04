package client

import (
	"strings"
	"testing"
	"time"
)

// The sequence table is the whole of the gap/restart contract. TelemetryFrame.
// sequence is monotonic per boot; the three outcomes below are what
// GUI_ARCHITECTURE.md 6.6 requires the console to distinguish.
func TestClassifySeq(t *testing.T) {
	for _, tc := range []struct {
		name     string
		last     uint64
		haveLast bool
		got      uint64
		wantKind seqKind
		wantGaps uint64
	}{
		{"first frame is accepted with no history", 0, false, 1, seqNext, 0},
		{"first frame need not start at one", 0, false, 41, seqNext, 0},
		{"nominal advance", 7, true, 8, seqNext, 0},
		{"one skipped frame", 7, true, 9, seqNext, 1},
		{"many skipped frames", 7, true, 12, seqNext, 4},
		{"duplicate is swallowed, not advanced", 7, true, 7, seqDuplicate, 0},
		{"backwards is a restart", 100, true, 3, seqRestart, 0},
		{"zero after uptime is a restart, not a duplicate", 100, true, 0, seqRestart, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kind, gaps := classifySeq(tc.last, tc.haveLast, tc.got)
			if kind != tc.wantKind || gaps != tc.wantGaps {
				t.Errorf("classifySeq(%d, %v, %d) = (%v, %d), want (%v, %d)",
					tc.last, tc.haveLast, tc.got, kind, gaps, tc.wantKind, tc.wantGaps)
			}
		})
	}
}

// The backoff names durations; nothing here sleeps. 1, 2, 4, 8, then capped.
func TestBackoffSequence(t *testing.T) {
	b := &backoff{}
	for i, want := range []time.Duration{
		time.Second, 2 * time.Second, 4 * time.Second,
		8 * time.Second, 8 * time.Second, 8 * time.Second,
	} {
		if got := b.next(); got != want {
			t.Fatalf("attempt %d waits %s, want %s", i, got, want)
		}
	}

	// Reset on the first good frame, not on a successful dial: a socket that
	// connects and yields nothing is not progress.
	b.reset()
	if got := b.next(); got != time.Second {
		t.Fatalf("after reset the wait is %s, want 1s", got)
	}
}

// A zero LinkState reads as down, never as healthy. Every boolean defaults to
// false and LastFrameAt to zero, so "never connected" cannot be mistaken for
// "connected with no news".
func TestZeroLinkStateIsDown(t *testing.T) {
	var s LinkState
	if s.ControlConnected || s.TelemetryConnected {
		t.Error("zero state reports a connection")
	}
	if s.LastFrameAge != 0 {
		t.Errorf("zero state has age %s", s.LastFrameAge)
	}
}

func TestCombineKeepsEachHalf(t *testing.T) {
	telemetry := LinkState{
		TelemetryConnected: true,
		FramesReceived:     10,
		SequenceGaps:       2,
		LastError:          "telemetry dropped once",
	}
	control := LinkState{
		ControlConnected: true,
		CommandsInFlight: 1,
	}

	got := Combine(telemetry, control)
	if !got.TelemetryConnected || !got.ControlConnected {
		t.Errorf("combined state lost a connection: %+v", got)
	}
	if got.FramesReceived != 10 || got.SequenceGaps != 2 || got.CommandsInFlight != 1 {
		t.Errorf("combined state lost a counter: %+v", got)
	}
	// A healthy control half must not erase the record of a telemetry failure.
	if !strings.Contains(got.LastError, "telemetry") {
		t.Errorf("combined state lost the telemetry error: %q", got.LastError)
	}

	// And a control failure must surface even when telemetry is fine.
	control.LastError = "no reply within 20s"
	if got := Combine(telemetry, control); !strings.Contains(got.LastError, "no reply") {
		t.Errorf("combined state lost the control error: %q", got.LastError)
	}
}

func TestAgeSinceZeroIsZero(t *testing.T) {
	if got := ageSince(time.Time{}); got != 0 {
		t.Errorf("age of never is %s, want 0", got)
	}
}
