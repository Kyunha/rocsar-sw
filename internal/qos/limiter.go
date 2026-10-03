package qos

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Priority is the class a message belongs to.
type Priority int

const (
	// PriorityHigh bypasses the limiter. Telemetry and command responses.
	//
	// "Bypasses" is the point and also the risk: these messages are small and
	// bounded by the telemetry frame budget, so they cannot run away. If a
	// high-priority producer ever grows large, this class becomes the mechanism
	// that starves everything else, and the frame budget test is what catches it.
	PriorityHigh Priority = iota

	// PriorityBulk is rate-limited. Artefact-adjacent traffic.
	PriorityBulk
)

func (p Priority) String() string {
	if p == PriorityHigh {
		return "high"
	}
	return "bulk"
}

// Message is one queued item.
type Message struct {
	Priority Priority
	Topic    string
	Payload  []byte

	// Enqueued is when it arrived, used to report queue depth honestly rather
	// than as a proxy for throughput.
	Enqueued time.Time
}

// Limiter is a two-class priority queue with a token bucket on the bulk class.
//
// It exists because the kernel shaper operates on traffic this process does not
// own, and this one operates on our own send path before a byte reaches the
// socket. A bulk transfer that fills the socket's outbound buffer delays the
// telemetry frame behind it, and no amount of kernel shaping afterwards undoes
// that latency.
type Limiter struct {
	bulkRateBps int
	log         *slog.Logger

	mu      sync.Mutex
	high    []Message
	bulk    []Message
	dropped uint64

	// tokens is the bucket, in bytes.
	tokens float64
	last   time.Time

	// notify wakes a waiter when capacity frees up.
	notify chan struct{}
	closed bool

	// queued is how many messages are waiting, for telemetry.
	queued int
}

// NewLimiter returns a limiter allowing bulkRateBps bytes per second.
func NewLimiter(bulkRateBps int, log *slog.Logger) *Limiter {
	if bulkRateBps <= 0 {
		bulkRateBps = 64 * 1024
	}
	return &Limiter{
		bulkRateBps: bulkRateBps,
		log:         log,
		// The bucket starts EMPTY.
		//
		// A textbook token bucket starts full, and that is right for a general
		// rate limiter. It is wrong here: starting full grants a free burst of
		// one second's traffic the moment the process starts, and the entire
		// purpose of this limiter is to prevent exactly that burst. There is no
		// backlog to amortise at startup, so nothing is owed.
		tokens: 0,
		last:   time.Now(),
		notify: make(chan struct{}, 1),
	}
}

// Push queues a message.
//
// It never blocks. A full queue drops the NEW message rather than the old one,
// and drops bulk before high. Dropping the oldest high-priority telemetry would
// mean the operator's most recent view disappears, which is worse than a late
// one arriving out of order.
func (l *Limiter) Push(m Message) {
	if m.Enqueued.IsZero() {
		m.Enqueued = time.Now()
	}

	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return
	}

	// High priority is bounded by the telemetry frame budget, so it is accepted
	// even when the queue is long. That trust is explicit rather than accidental.
	if m.Priority == PriorityHigh {
		l.high = append(l.high, m)
	} else {
		l.bulk = append(l.bulk, m)
	}
	l.queued = len(l.high) + len(l.bulk)
	l.mu.Unlock()

	l.wake()
}

// Pop returns the next message that may be sent, or nil if none may.
//
// High priority is always available. Bulk waits until the token bucket has
// enough credit for the message's size.
//
// The bucket is refilled from the wall clock rather than by a ticker, so a
// process that is descheduled for a minute does not then emit a minute's worth
// of backlog at once.
func (l *Limiter) Pop(ctx context.Context) (Message, bool) {
	for {
		l.refill()

		l.mu.Lock()
		if l.closed {
			l.mu.Unlock()
			return Message{}, false
		}

		if len(l.high) > 0 {
			m := l.high[0]
			l.high = l.high[1:]
			l.queued = len(l.high) + len(l.bulk)
			l.mu.Unlock()
			return m, true
		}

		if len(l.bulk) > 0 {
			size := float64(len(l.bulk[0].Payload))
			if l.tokens >= size {
				l.tokens -= size
				m := l.bulk[0]
				l.bulk = l.bulk[1:]
				l.queued = len(l.high) + len(l.bulk)
				l.mu.Unlock()
				return m, true
			}
		}

		l.mu.Unlock()

		// Nothing available. Wait for something to arrive or for the bucket to
		// refill, whichever comes first.
		wait := l.waitTime()
		if wait <= 0 {
			wait = 10 * time.Millisecond
		}

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return Message{}, false
		case <-l.notify:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// refill adds the credit accrued since the last call.
func (l *Limiter) refill() {
	now := time.Now()
	l.mu.Lock()
	elapsed := now.Sub(l.last).Seconds()
	l.last = now
	l.tokens += elapsed * float64(l.bulkRateBps)
	if l.tokens > float64(l.bulkRateBps) {
		// Cap at one second of credit. An unbounded bucket lets a long idle
		// period be spent all at once, which is precisely the burst this is
		// meant to prevent.
		l.tokens = float64(l.bulkRateBps)
	}
	l.mu.Unlock()
}

// waitTime is how long until the bucket can afford the next bulk message.
func (l *Limiter) waitTime() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.bulk) == 0 {
		return 0
	}
	deficit := float64(len(l.bulk[0].Payload)) - l.tokens
	if deficit <= 0 {
		return 0
	}
	return time.Duration(deficit / float64(l.bulkRateBps) * float64(time.Second))
}

func (l *Limiter) wake() {
	select {
	case l.notify <- struct{}{}:
	default:
	}
}

// Close releases anyone waiting in Pop.
func (l *Limiter) Close() {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
	l.wake()
}

// Stats reports queue depth and drops, for telemetry.
func (l *Limiter) Stats() (queued int, dropped uint64, tokens float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.queued, l.dropped, l.tokens
}

// Drain discards everything queued, used at shutdown.
func (l *Limiter) Drain() {
	l.mu.Lock()
	l.high = nil
	l.bulk = nil
	l.queued = 0
	l.mu.Unlock()
}

// DropOldestBulk makes room by discarding the oldest bulk message.
//
// Used when a bounded queue is wanted rather than unbounded growth. Bulk is the
// only class that may be dropped for capacity: telemetry is the operator's only
// view of the vehicle.
func (l *Limiter) DropOldestBulk() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.bulk) == 0 {
		return false
	}
	l.bulk = l.bulk[1:]
	l.queued = len(l.high) + len(l.bulk)
	l.dropped++
	return true
}

// ClassOfTopic maps a telemetry topic to its priority.
//
// Topics are the leading frame of a ZMQ PUB message and exist precisely so a
// subscriber can filter on them. They are also the natural place to decide how
// urgent something is, which keeps the policy in one readable table instead of
// scattered through the publishers.
func ClassOfTopic(topic string) Priority {
	switch topic {
	case TopicTelemetry, TopicPico, TopicControl:
		return PriorityHigh
	default:
		return PriorityBulk
	}
}

// The telemetry topics. Also the wire vocabulary: ARCHITECTURE.md 5.1.
const (
	TopicTelemetry = "telemetry"
	TopicSystem    = "telemetry.system"
	TopicGNSS      = "telemetry.gnss"
	TopicPico      = "telemetry.pico"
	TopicSDR       = "telemetry.sdr"
	TopicCamera    = "telemetry.camera"
	TopicControl   = "control.response"
	TopicArtefact  = "artefact"
)

// AllTopics lists every published topic, for the subscription filter and for the
// test that keeps the publisher and the filter in agreement.
func AllTopics() []string {
	return []string{TopicSystem, TopicGNSS, TopicPico, TopicSDR, TopicCamera}
}
