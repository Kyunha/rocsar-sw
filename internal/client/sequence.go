package client

// classifySeq compares an arriving telemetry sequence number against the last
// accepted one.
//
// TelemetryFrame.sequence is monotonic per boot (telemetry.proto:96). Three
// outcomes, and the console must distinguish all of them:
//
//	next     the frame advances the stream; gaps counts the frames skipped
//	         immediately before it (0 in the nominal case)
//	duplicate the same sequence number twice; swallowed, not rendered, and the
//	         accepted number does not move
//	restart  the number went backwards: the OBC rebooted
//
// A duplicate and a restart are different cases because only one of them is an
// event. PUB/SUB over TCP does not duplicate, and none has ever been observed;
// if one ever arrives, rendering it twice would double-draw a 1 Hz panel while
// advancing the counter past the frame that follows it. Swallowing it is the
// safe direction. A restart, by contrast, resets the PID, the uptime and every
// cumulative counter on the OBC side, so it resets ours and raises the flag --
// an operator watching a stale uptime is being lied to by a number.
//
// Pure: no sockets, no clock. The supervisor feeds it; the table test below is
// the whole of its verification.
type seqKind int

const (
	seqNext seqKind = iota
	seqDuplicate
	seqRestart
)

func classifySeq(last uint64, haveLast bool, got uint64) (kind seqKind, gaps uint64) {
	if !haveLast {
		return seqNext, 0
	}
	switch {
	case got > last:
		return seqNext, got - last - 1
	case got == last:
		return seqDuplicate, 0
	default:
		return seqRestart, 0
	}
}
