package client

import "time"

// backoff names the delay between reconnect attempts: 1 s, 2 s, 4 s, 8 s,
// capped at 8 s. Reset on the first good frame after a reconnect, not on a
// successful dial -- a socket that connects and then yields nothing is not
// progress, and treating it as progress restarts the backoff into a fast
// retry loop against a deaf OBC.
//
// No jitter. There is one operator and one OBC; there is no thundering herd
// to decorrelate, and a randomised delay would make the "measuring" state
// unpredictable for no reason.
//
// The type names durations; the supervisor sleeps. That split is what makes
// this testable without sleeping: the sequence 1,2,4,8,8 is asserted directly,
// and no test waits on a real timer.
type backoff struct {
	fails int
}

// maxBackoff caps the delay. Eight seconds is eight missed 1 Hz frames, which is
// long enough to be unmistakably "the link is down" on screen and short enough
// that a returning OBC is noticed promptly.
const maxBackoff = 8 * time.Second

func (b *backoff) next() time.Duration {
	d := time.Second << b.fails
	if d <= 0 || d > maxBackoff {
		d = maxBackoff
	}
	b.fails++
	return d
}

func (b *backoff) reset() { b.fails = 0 }
