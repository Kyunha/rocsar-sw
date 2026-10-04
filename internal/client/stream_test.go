package client

import (
	"context"
	"testing"
	"time"
)

// The stream tests run against a fake OBC over real loopback sockets: the
// frame layout, the topic envelope and the async-connect behaviour are
// exercised, not stubbed. The socket bound is shortened through the newStream
// seam so a dead publisher surfaces in hundreds of milliseconds rather than
// after the 10 s production timeout.

// Five frames are discarded after every connect, and the discard is counted
// rather than silent.
//
// What is asserted is the count, not the starting number: a SUB misses frames
// sent before its subscription propagates, so the backlog need not start at 1.
// Publish twelve; exactly five are discarded and delivery is consecutive from
// wherever it starts.
func TestStreamDiscardsTheSlowJoinerBacklog(t *testing.T) {
	f := newFakeOBC(t)
	defer f.close()

	s := newStream(context.Background(), f.testConfig(), 500*time.Millisecond)
	defer s.Close()

	go f.emit(t, 12, 20*time.Millisecond)

	first := nextFrame(t, s, 10*time.Second)
	second := nextFrame(t, s, 10*time.Second)
	third := nextFrame(t, s, 10*time.Second)
	if first.Gaps != 0 || first.Restart {
		t.Errorf("first frame carries history it cannot have: %+v", first)
	}
	if second.Telemetry.GetSequence() != first.Telemetry.GetSequence()+1 ||
		third.Telemetry.GetSequence() != second.Telemetry.GetSequence()+1 {
		t.Errorf("delivery is not consecutive from the first frame: %d, %d, %d",
			first.Telemetry.GetSequence(), second.Telemetry.GetSequence(),
			third.Telemetry.GetSequence())
	}

	eventually(t, 10*time.Second, func() bool {
		return s.State().FramesDiscarded == SlowJoinerFrames
	}, "discard counter to reach SlowJoinerFrames")
}

// While the backlog is being discarded the stream reports Measuring, so the UI
// can show "measuring" instead of a rate computed from backlog frames -- which
// would read roughly ten times the true rate.
func TestStreamReportsMeasuringUntilTheBacklogIsGone(t *testing.T) {
	f := newFakeOBC(t)
	defer f.close()

	s := newStream(context.Background(), f.testConfig(), 500*time.Millisecond)
	defer s.Close()

	if !s.Measuring() {
		t.Fatal("a fresh stream does not report Measuring")
	}
	go f.emit(t, SlowJoinerFrames+2, 20*time.Millisecond)
	nextFrame(t, s, 10*time.Second)

	eventually(t, 10*time.Second, func() bool { return !s.Measuring() },
		"Measuring to clear after the backlog")
}

// Skipped sequence numbers accumulate in State.SequenceGaps and ride along on
// the frame that follows them.
func TestStreamCountsSequenceGaps(t *testing.T) {
	f := newFakeOBC(t)
	defer f.close()

	s := newStream(context.Background(), f.testConfig(), 500*time.Millisecond)
	defer s.Close()

	go func() {
		f.emit(t, SlowJoinerFrames, 10*time.Millisecond) // discarded
		f.emit(t, 2, 10*time.Millisecond)                // delivered: 6, 7
		f.skip(2)                                        // 8, 9 never sent
		f.emit(t, 1, 0)                                  // delivered: 10, gaps 2
	}()

	nextFrame(t, s, 10*time.Second) // 6
	nextFrame(t, s, 10*time.Second) // 7
	third := nextFrame(t, s, 10*time.Second)
	if got := third.Telemetry.GetSequence(); got != 10 {
		t.Fatalf("third frame is sequence %d, want 10", got)
	}
	if third.Gaps != 2 {
		t.Errorf("frame carries %d gaps, want 2", third.Gaps)
	}
	eventually(t, 10*time.Second, func() bool { return s.State().SequenceGaps == 2 },
		"gap counter to reach 2")
}

// A backwards sequence means the OBC rebooted: the flag is raised, the gap
// counter resets (the far side reset too), and delivery continues.
func TestStreamFlagsAnOBCRestart(t *testing.T) {
	f := newFakeOBC(t)
	defer f.close()

	s := newStream(context.Background(), f.testConfig(), 500*time.Millisecond)
	defer s.Close()

	go func() {
		f.emit(t, SlowJoinerFrames+2, 10*time.Millisecond)
		f.skip(3) // bank some gaps first, so the reset is observable
		f.emit(t, 1, 0)
		f.rewind()
		f.emit(t, 2, 10*time.Millisecond)
	}()

	nextFrame(t, s, 10*time.Second)
	nextFrame(t, s, 10*time.Second)
	pre := nextFrame(t, s, 10*time.Second) // post-gap frame
	if pre.Gaps != 3 {
		t.Fatalf("pre-restart frame carries %d gaps, want 3", pre.Gaps)
	}
	restarted := nextFrame(t, s, 10*time.Second)
	if !restarted.Restart {
		t.Errorf("post-restart frame does not carry the Restart flag: %+v", restarted)
	}
	eventually(t, 10*time.Second, func() bool { return s.State().SequenceGaps == 0 },
		"gap counter to reset on restart")
	next := nextFrame(t, s, 10*time.Second)
	if next.Restart || next.Telemetry.GetSequence() != 2 {
		t.Errorf("delivery did not continue normally after the restart: %+v", next)
	}
}

// Killing the publisher must show up in State -- connected false, error set --
// and reopening it must resume delivery with a fresh slow-joiner discard. The
// SUB and the DEALER reconnect independently, so this test owns no queue and
// asserts nothing about commands.
func TestStreamReconnectsWhenTelemetryReturns(t *testing.T) {
	f := newFakeOBC(t)
	defer f.close()

	s := newStream(context.Background(), f.testConfig(), 300*time.Millisecond)
	defer s.Close()

	go f.emit(t, SlowJoinerFrames+3, 20*time.Millisecond)
	nextFrame(t, s, 10*time.Second)
	eventually(t, 10*time.Second, func() bool { return s.State().TelemetryConnected },
		"stream to report connected")

	f.dropPub()
	eventually(t, 10*time.Second, func() bool {
		st := s.State()
		return !st.TelemetryConnected && st.LastError != ""
	}, "stream to report the outage with a reason")

	discardedBefore := s.State().FramesDiscarded
	f.reopenPub(t)
	go f.emit(t, SlowJoinerFrames+2, 20*time.Millisecond)

	got := nextFrame(t, s, 15*time.Second)
	if got.Telemetry == nil {
		t.Fatal("no frame after the publisher returned")
	}
	eventually(t, 10*time.Second, func() bool {
		st := s.State()
		return st.TelemetryConnected && st.FramesDiscarded >= discardedBefore+SlowJoinerFrames
	}, "reconnect to resume with a fresh slow-joiner discard")
}

// Close while blocked in Recv must return promptly, not after the socket
// timeout. The live socket is closed first, which is what unblocks the receive.
func TestStreamCloseIsPrompt(t *testing.T) {
	f := newFakeOBC(t)
	defer f.close()

	s := newStream(context.Background(), f.testConfig(), 30*time.Second)
	done := make(chan struct{})
	go func() { s.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return within 5 s with a 30 s socket bound")
	}
}
