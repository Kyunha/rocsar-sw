package client

import "time"

// LinkState is the console's whole knowledge of the link's health.
//
// One struct, polled by the UI and pushed on change. See GUI_ARCHITECTURE.md
// 6.7. The two halves are produced independently -- Stream fills the telemetry
// half, Queue fills the control half -- because the SUB and the DEALER
// reconnect independently: telemetry can resume while commands are still
// failing, and the UI must say exactly that. Combine merges them.
//
// A zero LinkState means "never connected", not "everything is fine". Every
// boolean defaults to false and every counter to zero, so a state that was
// never filled in reads as down rather than as healthy.
type LinkState struct {
	// ControlConnected is true when the command socket is dialled and its last
	// exchange succeeded.
	ControlConnected bool
	// TelemetryConnected is true when the subscription is open and frames are
	// arriving within the socket bound.
	TelemetryConnected bool
	// LastFrameAt is when the last telemetry frame arrived. Zero means never.
	LastFrameAt time.Time
	// LastFrameAge is how old that frame is now. Zero with a zero LastFrameAt
	// means "no frame yet", which is distinct from a fresh frame.
	LastFrameAge time.Duration
	// FramesReceived counts accepted telemetry frames, excluding slow-joiner
	// discards.
	FramesReceived uint64
	// SequenceGaps counts skipped sequence numbers cumulatively. Reset when the
	// OBC restarts, because the counter on the far side reset too.
	SequenceGaps uint64
	// FramesDiscarded counts slow-joiner discards. A reconnect re-applies the
	// discard, so this moves on every reconnect by design.
	FramesDiscarded uint64
	// CommandsInFlight is 0 or 1. It exists so the UI can show that a command
	// was accepted and is waiting, rather than leaving the operator to wonder
	// whether the click registered.
	CommandsInFlight int
	// LastError is the most recent failure in plain words, or empty.
	LastError string
}

// Combine merges a telemetry-side state with a control-side state.
//
// Named parameters rather than (a, b): the two halves are produced by different
// types and the call site must say which is which. Telemetry contributes
// everything except the control fields; control contributes its connection flag,
// its in-flight count, and its error -- but only when it has one, so a healthy
// command socket does not erase the record of a telemetry failure.
func Combine(telemetry, control LinkState) LinkState {
	out := telemetry
	out.ControlConnected = control.ControlConnected
	out.CommandsInFlight = control.CommandsInFlight
	if control.LastError != "" {
		out.LastError = control.LastError
	}
	return out
}

// ageSince reports how long ago t was, or zero when t is zero. time.Since on a
// zero time is a large duration, not zero, and "no frame yet" must not render
// as "last frame 2562047 hours ago".
func ageSince(t time.Time) time.Duration {
	if t.IsZero() {
		return 0
	}
	return time.Since(t)
}
