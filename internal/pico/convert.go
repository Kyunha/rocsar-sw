package pico

import (
	"time"

	rocsarv1 "github.com/rocsar/obc/api/rocsar/v1"
	"github.com/rocsar/obc/internal/domain"
)

// telemetryToDomain converts the wire message into the domain type.
//
// The conversion lives here rather than in the transport so that domain stays
// free of the generated bindings, and so that the mapping is one auditable
// function instead of a field-by-field assignment scattered through the link.
//
// Two conversions deserve their reasoning:
//
//   - FeedbackState is an int32 on the wire and an enum here. An unknown value
//     from a future firmware decodes to a plain integer instead of failing the
//     frame, so an out-of-range value is mapped to UNKNOWN rather than clamped
//     to the nearest known state. Clamping would report MEASURED for a servo
//     that said something we do not understand, which is the one thing this
//     field exists to prevent.
//
//   - Antenna order is preserved. Axis N is antenna N, and a console that
//     relabels them between frames is worse than one that shows nothing.
func telemetryToDomain(m *rocsarv1.PicoTelemetry, now time.Time) domain.PicoTelemetry {
	d := domain.PicoTelemetry{
		GondolaHeadingDeg: float64(m.GetGondolaHeadingDeg()),
		TargetHeadingDeg:  float64(m.GetTargetHeadingDeg()),
		Heater1State:      m.GetHeater1State(),
		Heater2State:      m.GetHeater2State(),
		IMUPresent:        m.GetImuPresent(),
		ObservedAt:        now,
	}

	if n := len(m.GetAntennas()); n > 0 {
		d.Axes = make([]domain.Axis, 0, n)
		for _, a := range m.GetAntennas() {
			d.Axes = append(d.Axes, domain.Axis{
				ServoID:         a.GetServoId(),
				ManualMode:      a.GetManualMode(),
				CurrentTick:     a.GetCurrentTick(),
				CurrentAngleDeg: float64(a.GetCurrentAngleDeg()),
				Load:            a.GetLoad(),
				TemperatureC:    a.GetTemperatureC(),
				CenterTick:      a.GetCenterTick(),
				MountOffsetDeg:  float64(a.GetMountOffsetDeg()),
				DirMultiplier:   float64(a.GetDirMultiplier()),
				FeedbackState:   feedbackState(a.GetFeedbackState()),
				FeedbackError:   a.GetFeedbackError(),
			})
		}
	}

	return d
}

// feedbackState maps the wire int32 to the domain enum.
func feedbackState(v int32) domain.FeedbackState {
	switch v {
	case 1:
		return domain.FeedbackMeasured
	case 2:
		return domain.FeedbackHeld
	default:
		// Includes 0 (never answered) and any value a future firmware invents.
		return domain.FeedbackUnknown
	}
}

// feedbackStateToWire is the inverse, for tests and for the tools that build a
// synthetic telemetry frame.
func feedbackStateToWire(s domain.FeedbackState) int32 {
	switch s {
	case domain.FeedbackMeasured:
		return 1
	case domain.FeedbackHeld:
		return 2
	default:
		return 0
	}
}

// ackToDomain converts a wire ACK.
func ackToDomain(a *rocsarv1.PicoAck, now time.Time) *domain.Ack {
	if a == nil {
		return nil
	}
	return &domain.Ack{
		CommandSequence: a.GetCommandSequence(),
		Success:         a.GetSuccess(),
		Error:           domain.ErrorCode(a.GetError()),
		At:              now,
	}
}

// errorCodeToWire converts a domain error back to the wire enum, for the tools
// that build synthetic ACKs.
func errorCodeToWire(e domain.ErrorCode) rocsarv1.ErrorCode {
	return rocsarv1.ErrorCode(e)
}
