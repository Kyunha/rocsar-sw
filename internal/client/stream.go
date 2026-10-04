package client

import (
	"context"
	"fmt"
	"sync"
	"time"

	rocsarv1 "github.com/rocsar/obc/api/rocsar/v1"
)

// SlowJoinerFrames is how many frames to discard after (re)subscribing.
//
// A SUB that connects mid-stream receives the queued backlog, so a client that
// measures rate from the first frame reports roughly ten times the true rate
// followed by silence. The requirement has been documented since the beginning
// (ARCHITECTURE.md 5.1: "the GS client implements the discard") and no such
// constant has ever existed, because there was no client. This is it.
//
// Discards are counted, not silent: they land in LinkState.FramesDiscarded, and
// the stream reports Measuring until the discard is done. Five is enough for a
// 1 Hz PUB/SUB handshake and small enough that a reconnect does not feel broken.
const SlowJoinerFrames = 5

// Frame is one accepted telemetry frame plus what the stream noticed about it.
//
// Gaps is the number of sequence numbers skipped immediately before this frame
// (0 in the nominal case). Restart means the OBC rebooted: the sequence went
// backwards, and every cumulative counter on both sides reset with it.
type Frame struct {
	Telemetry *rocsarv1.TelemetryFrame
	Gaps      uint64
	Restart   bool
}

// Stream is a supervised telemetry subscription.
//
// It owns one SUB socket and one reader goroutine for its lifetime. On a
// receive timeout or an undecodable frame it closes the socket, backs off
// (1 s, 2 s, 4 s, capped at 8 s, reset on the first good frame), rebuilds the
// socket and discards SlowJoinerFrames again. Reconnect is this type's whole
// job, stated once and visibly: the retry loop is here, bounded, and its state
// is on screen through State -- not scattered inside socket code, and not
// silent.
//
// One goroutine touches the socket, ever. Closing the socket is what unblocks a
// Recv already in progress (go-zeromq has no receive timeout); Close also
// cancels the loop, so shutdown never waits on the backoff.
type Stream struct {
	cfg         Config
	sockTimeout time.Duration

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	frames chan Frame

	mu sync.Mutex
	st streamCounters
	// cur is the live socket, if any. Close closes it, which is what releases
	// a Recv blocked inside run: cancelling the context alone cannot interrupt
	// a Recv, because go-zeromq has no receive timeout to interrupt with.
	cur *Subscription
}

// streamCounters is everything State and Measuring read. Guarded by mu;
// updated at transitions only, so State never blocks behind a Recv.
type streamCounters struct {
	connected bool
	measuring bool
	lastErr   string
	received  uint64
	gaps      uint64
	discarded uint64
	lastSeq   uint64
	haveLast  bool
	lastFrame time.Time
}

// frameQueueDepth bounds how far the reader may run ahead of the consumer. A
// slow consumer stalls the reader rather than growing memory: at 1 Hz nominal
// the queue never fills, and a filled queue means the consumer is gone, in
// which case blocking is the honest behaviour. The reader always selects on
// Close, so shutdown is never stuck behind a full queue.
const frameQueueDepth = 16

// NewStream opens a supervised subscription and starts delivering.
//
// Frames flow until Close. The first SlowJoinerFrames after every (re)connect
// are discarded and counted; the first delivered frame carries no gap history.
func (c *Client) NewStream(ctx context.Context) *Stream {
	return newStream(ctx, c.cfg, subTimeout)
}

// newStream is the test seam: same supervisor, shorter socket bound, so tests
// do not wait out the 10 s production timeout to observe a reconnect.
func newStream(ctx context.Context, cfg Config, sockTimeout time.Duration) *Stream {
	ctx, cancel := context.WithCancel(ctx)
	s := &Stream{
		cfg:         cfg,
		sockTimeout: sockTimeout,
		ctx:         ctx,
		cancel:      cancel,
		frames:      make(chan Frame, frameQueueDepth),
	}
	s.st.measuring = true
	s.wg.Add(1)
	go s.run()
	return s
}

// Frames delivers accepted telemetry. The channel is never closed by the
// stream; Close ends delivery, and the caller stops ranging when its own
// context ends.
func (s *Stream) Frames() <-chan Frame { return s.frames }

// Measuring reports whether the stream is still discarding the slow-joiner
// backlog after a (re)connect. While true, the UI shows a "measuring" state
// rather than a rate -- the rate measured from backlog frames is roughly ten
// times the true one.
func (s *Stream) Measuring() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.measuring
}

// State snapshots the telemetry half of the link. Control fields stay zero;
// Combine merges this with Queue.State.
func (s *Stream) State() LinkState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return LinkState{
		TelemetryConnected: s.st.connected,
		LastFrameAt:        s.st.lastFrame,
		LastFrameAge:       ageSince(s.st.lastFrame),
		FramesReceived:     s.st.received,
		SequenceGaps:       s.st.gaps,
		FramesDiscarded:    s.st.discarded,
		LastError:          s.st.lastErr,
	}
}

// Close ends delivery and releases the socket. It does not wait for the
// backoff, and it does not wait for a blocked Recv either: the live socket is
// closed first, which is what unblocks it.
func (s *Stream) Close() {
	s.cancel()
	s.mu.Lock()
	cur := s.cur
	s.mu.Unlock()
	if cur != nil {
		_ = cur.Close()
	}
	s.wg.Wait()
}

func (s *Stream) run() {
	defer s.wg.Done()
	bo := &backoff{}
	for {
		if !s.connect(bo) {
			return
		}
	}
}

// connect runs one socket lifetime: dial, discard, deliver until the socket
// fails or the stream closes. It reports whether to try again.
func (s *Stream) connect(bo *backoff) bool {
	sub, err := dialSub(s.cfg.Telemetry, s.cfg.Topic, s.sockTimeout)
	if err != nil {
		// Dialling does not prove anything -- but failing to dial proves the
		// far side is not there, which is worth one backoff step rather than a
		// hot loop.
		s.setDown(err.Error())
		return s.sleep(bo.next())
	}
	s.mu.Lock()
	s.cur = sub
	s.mu.Unlock()

	s.setUp()
	for i := 0; i < SlowJoinerFrames; i++ {
		outcome, derr := s.discardOne(sub)
		switch outcome {
		case discardClosed:
			s.clearCur(sub)
			_ = sub.Close()
			return false
		case discardFailed:
			s.clearCur(sub)
			_ = sub.Close()
			s.setDown(fmt.Sprintf("socket failed during slow-joiner discard: %s", derr))
			return s.sleep(bo.next())
		}
	}
	s.setMeasuring(false)

	for {
		frame, err := sub.Recv(s.sockTimeout)
		if err != nil {
			s.clearCur(sub)
			_ = sub.Close()
			s.setDown(err.Error())
			return s.sleep(bo.next())
		}
		bo.reset()
		if !s.accept(frame) {
			s.clearCur(sub)
			_ = sub.Close()
			return false
		}
	}
}

// clearCur forgets the live socket. Call it before closing: Close closes
// whatever cur points at, and closing an already-closed socket must not race
// a fresh dial.
func (s *Stream) clearCur(sub *Subscription) {
	s.mu.Lock()
	if s.cur == sub {
		s.cur = nil
	}
	s.mu.Unlock()
}

// discardOutcome is what one slow-joiner discard produced.
type discardOutcome int

const (
	// discardDropped: a backlog frame was dropped and counted.
	discardDropped discardOutcome = iota
	// discardFailed: the socket failed mid-discard; back off and redial.
	discardFailed
	// discardClosed: the stream closed while waiting.
	discardClosed
)

// discardOne drops a single backlog frame and counts it.
func (s *Stream) discardOne(sub *Subscription) (discardOutcome, error) {
	select {
	case <-s.ctx.Done():
		return discardClosed, nil
	default:
	}
	if _, err := sub.Recv(s.sockTimeout); err != nil {
		select {
		case <-s.ctx.Done():
			return discardClosed, nil
		default:
			return discardFailed, err
		}
	}
	s.mu.Lock()
	s.st.discarded++
	s.mu.Unlock()
	return discardDropped, nil
}

// accept classifies one frame, updates the counters and delivers it. False
// means the stream closed while delivering.
func (s *Stream) accept(frame *rocsarv1.TelemetryFrame) bool {
	kind, gaps := func() (seqKind, uint64) {
		s.mu.Lock()
		defer s.mu.Unlock()
		kind, gaps := classifySeq(s.st.lastSeq, s.st.haveLast, frame.GetSequence())
		switch kind {
		case seqNext:
			s.st.lastSeq = frame.GetSequence()
			s.st.haveLast = true
			s.st.received++
			s.st.gaps += gaps
			s.st.lastFrame = time.Now()
			s.st.connected = true
			s.st.lastErr = ""
		case seqDuplicate:
			// Swallowed: counted nowhere, rendered nowhere. See sequence.go.
		case seqRestart:
			s.st.lastSeq = frame.GetSequence()
			s.st.haveLast = true
			s.st.received++
			s.st.gaps = 0
			s.st.lastFrame = time.Now()
			s.st.connected = true
			s.st.lastErr = ""
		}
		return kind, gaps
	}()

	if kind == seqDuplicate {
		return true
	}
	out := Frame{Telemetry: frame, Gaps: gaps, Restart: kind == seqRestart}
	select {
	case s.frames <- out:
		return true
	case <-s.ctx.Done():
		return false
	}
}

// sleep waits out a backoff step, interruptible by Close. False means closed.
func (s *Stream) sleep(d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-s.ctx.Done():
		return false
	}
}

func (s *Stream) setUp() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.connected = true
	s.st.measuring = true
}

func (s *Stream) setMeasuring(m bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.measuring = m
}

func (s *Stream) setDown(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.connected = false
	s.st.measuring = false
	s.st.lastErr = fmt.Sprintf("telemetry: %s", reason)
}
