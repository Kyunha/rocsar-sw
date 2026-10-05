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

	join := backgroundEmit(t, f, 12, 20*time.Millisecond)
	defer join()

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
	join := backgroundEmit(t, f, SlowJoinerFrames+2, 20*time.Millisecond)
	defer join()
	nextFrame(t, s, 10*time.Second)

	eventually(t, 10*time.Second, func() bool { return !s.Measuring() },
		"Measuring to clear after the backlog")
}

// Skipped sequence numbers accumulate in State.SequenceGaps and ride along on
// the frame that follows them.
//
// Deterministic by construction: delivery is established first (so the discard
// is over and history exists), the background publisher is stopped and the
// socket drained (so nothing is in flight), and only then is a gap
// manufactured. Every number below is derived from what was actually delivered,
// never assumed.
func TestStreamCountsSequenceGaps(t *testing.T) {
	f := newFakeOBC(t)
	defer f.close()

	s := newStream(context.Background(), f.testConfig(), 500*time.Millisecond)
	defer s.Close()

	stop := make(chan struct{})
	go f.emitUntil(t, 5*time.Millisecond, stop)

	first := nextFrame(t, s, 10*time.Second)
	second := nextFrame(t, s, 10*time.Second)
	if second.Telemetry.GetSequence() != first.Telemetry.GetSequence()+1 {
		t.Fatalf("nominal delivery is not consecutive: %d then %d",
			first.Telemetry.GetSequence(), second.Telemetry.GetSequence())
	}

	close(stop)
	base := second.Telemetry.GetSequence()
	if d := drainFrames(t, s, 300*time.Millisecond); d > base {
		base = d
	}

	f.skip(2)
	f.emitOne(t)

	gapped := nextFrame(t, s, 10*time.Second)
	if got := gapped.Telemetry.GetSequence(); got != base+3 {
		t.Fatalf("post-gap frame is sequence %d, want %d", got, base+3)
	}
	if gapped.Gaps != 2 {
		t.Errorf("frame carries %d gaps, want 2", gapped.Gaps)
	}
	eventually(t, 10*time.Second, func() bool { return s.State().SequenceGaps == 2 },
		"gap counter to reach 2")
}

// A backwards sequence means the OBC rebooted: the flag is raised, the gap
// counter resets (the far side reset too), and delivery continues.
//
// Same determinism as the gap test: establish, stop, drain, then manufacture
// the gap and the restart from known numbers.
func TestStreamFlagsAnOBCRestart(t *testing.T) {
	f := newFakeOBC(t)
	defer f.close()

	s := newStream(context.Background(), f.testConfig(), 500*time.Millisecond)
	defer s.Close()

	stop := make(chan struct{})
	go f.emitUntil(t, 5*time.Millisecond, stop)
	nextFrame(t, s, 10*time.Second)
	second := nextFrame(t, s, 10*time.Second)
	close(stop)
	base := second.Telemetry.GetSequence()
	if d := drainFrames(t, s, 300*time.Millisecond); d > base {
		base = d
	}

	// Bank some gaps first, so the reset is observable rather than assumed.
	f.skip(3)
	f.emitOne(t)
	pre := nextFrame(t, s, 10*time.Second)
	if pre.Gaps != 3 || pre.Telemetry.GetSequence() != base+4 {
		t.Fatalf("pre-restart frame is seq %d with %d gaps, want seq %d with 3",
			pre.Telemetry.GetSequence(), pre.Gaps, base+4)
	}

	f.rewind()
	f.emitOne(t)
	f.emitOne(t)
	restarted := nextFrame(t, s, 10*time.Second)
	if !restarted.Restart || restarted.Telemetry.GetSequence() != 1 {
		t.Errorf("post-restart frame is not flagged as sequence 1: %+v", restarted)
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
//
// The publisher runs until told to stop, never a fixed burst: after the
// resubscription, frames emitted before it arrived are lost to the backlog, so
// a fixed burst could be consumed entirely by the discard and the test would
// wait on silence.
func TestStreamReconnectsWhenTelemetryReturns(t *testing.T) {
	f := newFakeOBC(t)
	defer f.close()

	s := newStream(context.Background(), f.testConfig(), 300*time.Millisecond)
	defer s.Close()

	stop := make(chan struct{})
	go f.emitUntil(t, 20*time.Millisecond, stop)
	nextFrame(t, s, 10*time.Second)
	eventually(t, 10*time.Second, func() bool { return s.State().TelemetryConnected },
		"stream to report connected")

	close(stop)
	// The emitter checks stop between frames, so by the time the publisher
	// drops there is at most one frame still in flight -- and it lands in the
	// discard or not at all, never in the assertions below.
	time.Sleep(100 * time.Millisecond)
	f.dropPub()
	eventually(t, 10*time.Second, func() bool {
		st := s.State()
		return !st.TelemetryConnected && st.LastError != ""
	}, "stream to report the outage with a reason")

	discardedBefore := s.State().FramesDiscarded
	f.reopenPub(t)
	stop2 := make(chan struct{})
	defer close(stop2)
	go f.emitUntil(t, 20*time.Millisecond, stop2)

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
