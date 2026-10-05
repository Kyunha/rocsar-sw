package client

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-zeromq/zmq4"

	rocsarv1 "github.com/rocsar/obc/api/rocsar/v1"
)

// ErrNotConnected is returned when a command is submitted while the link is
// down.
//
// A sentinel, not a synthesised CommandResponse. NOT_CONNECTED is a wire error
// the OBC puts in a reply; manufacturing one locally for a reply the OBC never
// sent would be a lie about where the refusal came from. The operator-visible
// sentence is the same either way; the provenance is honest this way.
//
// A submission made while the link is down is dropped, never deferred. A jog
// issued when the link dies must not execute when the link returns, an
// indeterminate time later, at a bearing the operator has since forgotten.
var ErrNotConnected = fmt.Errorf("client: the link is down; the command was dropped, not queued")

// ErrQueueFull is returned when a command is submitted while one is already in
// flight and another is already queued.
//
// Refusing is deliberate. Silently queueing operator clicks turns "I pressed
// jog" into "jog happened at some point", and a GUI -- a slider, a row of
// buttons, each firing an event -- makes this easy to get wrong. See
// GUI_ARCHITECTURE.md 6.4.
var ErrQueueFull = fmt.Errorf("client: a command is already in flight and one is queued; the command was refused")

// Queue is a serialised command channel to the OBC.
//
// It owns one persistent DEALER and serialises everything through it: one
// command in flight, one queued behind it, the rest refused with ErrQueueFull.
// The OBC's ROUTER handles one command at a time and its slowest handler is the
// Pico acknowledgement timeout at a fixed 500 ms, so a second command sent
// back-to-back waits behind the first rather than running beside it -- and with
// a 20 s client timeout it may expire while the OBC is still working.
//
// Reconnect here is lazy, not supervised, and that is a deliberate difference
// from Stream. Telemetry must resume unprompted, so the SUB reconnects in the
// background. Commands are user-initiated: a background dial loop firing while
// nobody is commanding is the invisible retry storm ARCHITECTURE.md 6.2 refuses
// the Pico link, so this dials on the next Submit instead and reports the gap
// through State. One goroutine-equivalent touches the socket -- every socket
// operation happens under mu, in the submitting caller's goroutine.
type Queue struct {
	cfg Config
	// cmdTimeout bounds one exchange. CommandTimeout in production; shorter in
	// tests, which would otherwise wait out the full 20 s to observe a failure.
	cmdTimeout time.Duration

	// sem bounds the depth: one in flight plus one queued. Acquired
	// non-blockingly; a full semaphore is the refusal, not a wait.
	sem chan struct{}
	// mu serialises submissions. Held for the whole exchange.
	mu sync.Mutex
	// sockMu guards only the socket pointer, and only briefly. Close takes
	// sockMu and never mu, so closing while a Submit is inside never blocks
	// behind it: the socket is closed under the blocked Recv, which fails at
	// once instead of running to its timeout. Lock order, where both are
	// held, is always mu then sockMu, never the reverse.
	sockMu sync.Mutex
	// sock is nil when down.
	sock zmq4.Socket

	// bo spaces re-attempts after a failure: 1 s, 2 s, 4 s, capped at 8 s.
	// downUntil is when the current fail-fast window ends; lastErr is what
	// closed it. Both guarded by mu -- they only change inside Submit, which
	// already holds it.
	bo        backoff
	downUntil time.Time
	lastErr   string

	stMu sync.Mutex
	st   queueCounters
}

// queueCounters is everything State reads. Separate from mu because Submit
// holds mu for up to CommandTimeout while the UI polls State -- sharing one
// mutex would make health reporting wait behind a wedged command.
type queueCounters struct {
	up       bool
	inFlight int
	lastErr  string
}

// queueDepth is the whole policy: one in flight, one queued.
const queueDepth = 2

// NewQueue returns a command queue. It opens nothing; the first Submit dials.
//
// No background goroutine and no context of its own: there is nothing to
// supervise here. Reconnect is lazy -- a failed exchange drops the socket and
// the next Submit redials -- because a dial loop firing while nobody is
// commanding would be retries with no operator behind them.
func (c *Client) NewQueue() *Queue {
	return newQueue(c.cfg, CommandTimeout)
}

// newQueue is the test seam: same serialiser, shorter exchange bound, so
// tests do not wait out the full 20 s to observe a failure.
func newQueue(cfg Config, cmdTimeout time.Duration) *Queue {
	return &Queue{
		cfg:        cfg,
		cmdTimeout: cmdTimeout,
		sem:        make(chan struct{}, queueDepth),
	}
}

// Submit sends one request and waits for its reply.
//
// ctx is honoured before starting: an already-cancelled context returns
// without touching the socket. It cannot interrupt a submit already in flight,
// because go-zeromq has no cancellable receive -- the bound that applies is
// CommandTimeout, same as Send. That limitation is stated here rather than
// discovered in a window.
func (q *Queue) Submit(ctx context.Context, req *rocsarv1.CommandRequest) (*rocsarv1.CommandResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case q.sem <- struct{}{}:
		defer func() { <-q.sem }()
	default:
		return nil, ErrQueueFull
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	q.setInFlight(1)
	defer q.setInFlight(0)

	// Fail fast inside the window opened by the last failure. Each click
	// during the window returns at once instead of burning a full exchange
	// timeout against a link that is already known to be down -- and, just as
	// importantly, none of them is queued for when the link returns.
	if time.Now().Before(q.downUntil) {
		return nil, fmt.Errorf("%w (last failure: %s)", ErrNotConnected, q.lastErr)
	}

	if !q.ensureDialedLocked() {
		return nil, fmt.Errorf("%w: %s", ErrNotConnected, q.lastErr)
	}

	resp, err := roundTrip(q.socket(), req, q.cmdTimeout)
	if err != nil {
		// The socket is suspect now: a failed exchange may have left a partial
		// envelope on either side, and the next command must not read this
		// one's reply. Drop it; the next Submit redials.
		q.dropSocket()
		q.failLocked(err.Error())
		return nil, err
	}
	q.setUp()
	q.bo.reset()
	return resp, nil
}

// ensureDialedLocked dials when down. False means the dial itself failed, in
// which case lastErr names it and a fail-fast window is already open. Call
// with mu held.
func (q *Queue) ensureDialedLocked() bool {
	q.sockMu.Lock()
	defer q.sockMu.Unlock()
	if q.sock != nil {
		return true
	}
	sock := zmq4.NewDealer(context.Background(), zmq4.WithTimeout(q.cmdTimeout))
	if err := dialBounded(sock, q.cfg.Control, q.cmdTimeout); err != nil {
		// Close the abandoned handle: a handshake that completes late must
		// land on a closed socket, never join this one as a second connection.
		_ = sock.Close()
		q.failLocked(err.Error())
		return false
	}
	q.sock = sock
	return true
}

// socket returns the live socket, or nil. Brief lock; the caller must not hold
// it across the exchange.
func (q *Queue) socket() zmq4.Socket {
	q.sockMu.Lock()
	defer q.sockMu.Unlock()
	return q.sock
}

// dropSocket closes and forgets the live socket, if any.
func (q *Queue) dropSocket() {
	q.sockMu.Lock()
	defer q.sockMu.Unlock()
	if q.sock != nil {
		_ = q.sock.Close()
		q.sock = nil
	}
}

// failLocked opens a fail-fast window. Call with mu held.
//
// A refused dial fails fast on its own -- go-zeromq attempts the connect
// synchronously -- but a blackholed address does not: the dial succeeds and
// the exchange burns a full timeout instead. Without the window, every click
// during such an outage would cost a full exchange timeout and pile up behind
// the semaphore. With it, the first failure costs one timeout and the rest fail
// at once, until the window expires and the next click probes again.
func (q *Queue) failLocked(reason string) {
	q.downUntil = time.Now().Add(q.bo.next())
	q.lastErr = reason
	q.setDown(reason)
}

// State snapshots the control half of the link. Telemetry fields stay zero;
// Combine merges this with Stream.State.
func (q *Queue) State() LinkState {
	q.stMu.Lock()
	defer q.stMu.Unlock()
	return LinkState{
		ControlConnected: q.st.up,
		CommandsInFlight: q.st.inFlight,
		LastError:        q.st.lastErr,
	}
}

// Close releases the socket. It never blocks behind a Submit: the socket is
// closed under any exchange in progress, whose Recv fails at once instead of
// running to its timeout. A test that fails mid-submit must still tear down,
// and a Close that hangs behind the command it is trying to abandon is a
// deadlock wearing a method signature.
func (q *Queue) Close() {
	q.dropSocket()
}

func (q *Queue) setInFlight(n int) {
	q.stMu.Lock()
	defer q.stMu.Unlock()
	q.st.inFlight = n
}

func (q *Queue) setUp() {
	q.stMu.Lock()
	defer q.stMu.Unlock()
	q.st.up = true
	q.st.lastErr = ""
}

func (q *Queue) setDown(reason string) {
	q.stMu.Lock()
	defer q.stMu.Unlock()
	q.st.up = false
	q.st.lastErr = fmt.Sprintf("control: %s", reason)
}
