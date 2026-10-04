// Package transport is the boundary to the outside world: ZeroMQ sockets, HTTP,
// and the protobuf encoding that goes on them.
//
// This is where domain types become wire messages and back. internal/telemetry
// assembles a frame from providers and knows nothing about protobuf -- a layering
// rule that test/layering_test.go enforces by walking the import graph, and
// which caught this file when it was first written in internal/telemetry.
package transport

import (
	"time"

	"google.golang.org/protobuf/proto"

	rocsarv1 "github.com/rocsar/obc/api/rocsar/v1"
	"github.com/rocsar/obc/internal/domain"
	"github.com/rocsar/obc/internal/telemetry"
)

// Encode converts a snapshot into the wire message.
//
// This is the only function in the system that turns domain types into protobuf.
// Keeping the conversion in one auditable place is what stops a schema change
// from quietly changing what the operator sees -- which is the failure mode when
// the mapping is scattered: every publisher assigns a field slightly differently
// and no two frames agree.
func EncodeTelemetry(s telemetry.Snapshot) *rocsarv1.TelemetryFrame {
	f := &rocsarv1.TelemetryFrame{
		Sequence:        s.Sequence,
		GeneratedAtUnix: float64(s.GeneratedAt.UnixNano()) / 1e9,
		PicoConnected:   s.PicoConnected,
		System: &rocsarv1.SystemStatus{
			State:    toWireState(s.System.State),
			UptimeS:  s.Uptime.Seconds(),
			CpuTempC: s.System.CPUTemp,
		},
		Link: &rocsarv1.LinkStatus{
			State:         toWireState(s.Link.State),
			Device:        s.Link.Device,
			RateKbps:      s.Link.RateKbps,
			PriorityKbps:  s.Link.PriorityKbps,
			ShapingActive: s.Link.ShapingActive,
		},
		Sdr: &rocsarv1.SdrStatus{
			State:   toWireState(s.SDR.State),
			Running: s.SDR.Running,
			Pid:     s.SDR.PID,
		},
		Camera: &rocsarv1.CameraStatus{
			State: toWireState(s.Camera),
			PhotosTaken: func() *uint64 {
				if s.Camera == domain.SubsystemUnspecified {
					return nil
				}
				v := s.PhotosTaken
				return &v
			}(),
		},
	}

	if s.System.Mocked != nil {
		f.System.MockedSubsystems = s.MockedSubsystems()
	}
	if s.CameraDevice != "" {
		f.Camera.Device = proto.String(s.CameraDevice)
	}
	if s.LastPhoto != "" {
		f.Camera.LastPhotoName = proto.String(s.LastPhoto)
	}
	if s.Link.InactiveReason != "" {
		f.Link.InactiveReason = proto.String(s.Link.InactiveReason)
	}
	if s.SDR.LastLog != "" {
		f.Sdr.LastLog = proto.String(s.SDR.LastLog)
	}
	if s.SDR.LastError != "" {
		f.Sdr.LastError = proto.String(s.SDR.LastError)
	}

	// GNSS: every receiver, so the redundancy is visible and the operator knows
	// which one is trusted.
	//
	// There is no satellite count anywhere in this conversion, deliberately: the
	// 142-byte UDP_message does not carry one. See GnssReceiverStatus in
	// telemetry.proto.
	for _, r := range s.GNSS {
		g := &rocsarv1.GnssReceiverStatus{
			ReceiverId:      uint32(r.ReceiverID),
			Selected:        r.Selected,
			FixOk:           r.FixOK,
			LatitudeDeg:     r.Fix.LatitudeDeg,
			LongitudeDeg:    r.Fix.LongitudeDeg,
			AltitudeM:       r.Fix.AltitudeM,
			GroundSpeedMps:  r.Fix.GroundSpeedMP,
			CourseDeg:       r.Fix.CourseDeg,
			PacketsAccepted: r.Accepted,
			PacketsRejected: r.Rejected,
		}
		if r.HasFix {
			// FixAge is set only when there is a fix. Absent means "never",
			// which is different from "age zero" and is not collapsed.
			g.FixAgeS = r.FixAge.Seconds()
		}
		f.Gnss = append(f.Gnss, g)
	}

	// Pico telemetry only when the flight controller has ever answered.
	//
	// Absent means "we have never heard from it", which is different from "it
	// answered and every field is zero". Populating a zeroed PicoTelemetry on a
	// silent link is precisely how a console ends up showing a healthy-looking
	// 0 for a motor that has not moved.
	if s.HasPico {
		f.Pico = picoTelemetry(s.Pico)
	}

	if s.PicoAck != nil {
		f.PicoLastAck = &rocsarv1.PicoAck{
			CommandSequence: s.PicoAck.CommandSequence,
			Success:         s.PicoAck.Success,
			Error:           rocsarv1.ErrorCode(s.PicoAck.Error),
		}
	}

	return f
}

func picoTelemetry(p domain.PicoTelemetry) *rocsarv1.PicoTelemetry {
	t := &rocsarv1.PicoTelemetry{
		GondolaHeadingDeg: float32(p.GondolaHeadingDeg),
		TargetHeadingDeg:  float32(p.TargetHeadingDeg),
		Heater1State:      p.Heater1State,
		Heater2State:      p.Heater2State,
		ImuPresent:        p.IMUPresent,
	}
	for _, a := range p.Axes {
		t.Antennas = append(t.Antennas, &rocsarv1.AntennaTelemetry{
			ServoId:         a.ServoID,
			ManualMode:      a.ManualMode,
			CurrentTick:     a.CurrentTick,
			CurrentAngleDeg: float32(a.CurrentAngleDeg),
			Load:            a.Load,
			TemperatureC:    a.TemperatureC,
			CenterTick:      a.CenterTick,
			MountOffsetDeg:  float32(a.MountOffsetDeg),
			DirMultiplier:   float32(a.DirMultiplier),
			FeedbackState:   int32(a.FeedbackState),
			FeedbackError:   a.FeedbackError,
		})
	}
	return t
}

// EncodedSize returns the serialised size of a snapshot's frame.
func EncodedTelemetrySize(s telemetry.Snapshot) int {
	body, err := proto.Marshal(EncodeTelemetry(s))
	if err != nil {
		return -1
	}
	return len(body)
}

// subsystemState maps the domain enum onto the wire.
//
// Exhaustive on both sides with no default branch, so adding a state to domain
// without mapping it here is a compile error rather than a value the Ground
// Station cannot interpret.
func toWireState(s domain.SubsystemState) rocsarv1.SubsystemState {
	switch s {
	case domain.SubsystemDisconnected:
		return rocsarv1.SubsystemState_SUBSYSTEM_DISCONNECTED
	case domain.SubsystemReady:
		return rocsarv1.SubsystemState_SUBSYSTEM_READY
	case domain.SubsystemBusy:
		return rocsarv1.SubsystemState_SUBSYSTEM_BUSY
	case domain.SubsystemError:
		return rocsarv1.SubsystemState_SUBSYSTEM_ERROR
	default:
		return rocsarv1.SubsystemState_SUBSYSTEM_UNSPECIFIED
	}
}

// Decode converts a wire message back into a Snapshot.
//
// Only the Ground Station and the tools need this; the OBC assembles from
// providers. It exists so a captured frame can be replayed through the same
// view logic that renders a live one, which is how a recorded flight is
// inspected after the fact.
func DecodeTelemetry(f *rocsarv1.TelemetryFrame) telemetry.Snapshot {
	s := telemetry.Snapshot{
		Sequence:      f.GetSequence(),
		GeneratedAt:   time.Unix(0, int64(f.GetGeneratedAtUnix()*1e9)),
		PicoConnected: f.GetPicoConnected(),
	}
	if sys := f.GetSystem(); sys != nil {
		s.System = telemetry.SystemSnapshot{
			State:   fromWireState(sys.GetState()),
			Uptime:  time.Duration(sys.GetUptimeS() * float64(time.Second)),
			CPUTemp: sys.GetCpuTempC(),
			Mocked:  sys.GetMockedSubsystems(),
		}
	}
	if l := f.GetLink(); l != nil {
		s.Link = domain.LinkStatus{
			State:          fromWireState(l.GetState()),
			Device:         l.GetDevice(),
			RateKbps:       l.GetRateKbps(),
			PriorityKbps:   l.GetPriorityKbps(),
			ShapingActive:  l.GetShapingActive(),
			InactiveReason: l.GetInactiveReason(),
		}
	}
	if sd := f.GetSdr(); sd != nil {
		s.SDR = telemetry.SDRSnapshot{
			State:     fromWireState(sd.GetState()),
			Running:   sd.GetRunning(),
			PID:       sd.GetPid(),
			LastLog:   sd.GetLastLog(),
			LastError: sd.GetLastError(),
		}
	}
	if cam := f.GetCamera(); cam != nil {
		s.Camera = fromWireState(cam.GetState())
		s.CameraDevice = cam.GetDevice()
		s.LastPhoto = cam.GetLastPhotoName()
		if cam.PhotosTaken != nil {
			s.PhotosTaken = cam.GetPhotosTaken()
		}
	}

	for _, g := range f.GetGnss() {
		r := domain.ReceiverStatus{
			ReceiverID: int(g.GetReceiverId()),
			Selected:   g.GetSelected(),
			FixOK:      g.GetFixOk(),
			Accepted:   g.GetPacketsAccepted(),
			Rejected:   g.GetPacketsRejected(),
		}
		if g.GetFixAgeS() > 0 {
			r.HasFix = true
			r.FixAge = time.Duration(g.GetFixAgeS() * float64(time.Second))
			r.Fix = domain.Fix{
				LatitudeDeg:   g.GetLatitudeDeg(),
				LongitudeDeg:  g.GetLongitudeDeg(),
				AltitudeM:     g.GetAltitudeM(),
				GroundSpeedMP: g.GetGroundSpeedMps(),
				CourseDeg:     g.GetCourseDeg(),
			}
		}
		s.GNSS = append(s.GNSS, r)
	}

	if p := f.GetPico(); p != nil {
		s.HasPico = true
		s.Pico = domain.PicoTelemetry{
			GondolaHeadingDeg: float64(p.GetGondolaHeadingDeg()),
			TargetHeadingDeg:  float64(p.GetTargetHeadingDeg()),
			Heater1State:      p.GetHeater1State(),
			Heater2State:      p.GetHeater2State(),
			IMUPresent:        p.GetImuPresent(),
		}
		for _, a := range p.GetAntennas() {
			s.Pico.Axes = append(s.Pico.Axes, domain.Axis{
				ServoID:         a.GetServoId(),
				ManualMode:      a.GetManualMode(),
				CurrentTick:     a.GetCurrentTick(),
				CurrentAngleDeg: float64(a.GetCurrentAngleDeg()),
				Load:            a.GetLoad(),
				TemperatureC:    a.GetTemperatureC(),
				CenterTick:      a.GetCenterTick(),
				MountOffsetDeg:  float64(a.GetMountOffsetDeg()),
				DirMultiplier:   float64(a.GetDirMultiplier()),
				FeedbackState:   domain.FeedbackState(a.GetFeedbackState()),
				FeedbackError:   a.GetFeedbackError(),
			})
		}
	}

	if a := f.GetPicoLastAck(); a != nil {
		s.PicoAck = &domain.Ack{
			CommandSequence: a.GetCommandSequence(),
			Success:         a.GetSuccess(),
			Error:           domain.ErrorCode(a.GetError()),
		}
	}

	return s
}

func fromWireState(s rocsarv1.SubsystemState) domain.SubsystemState {
	switch s {
	case rocsarv1.SubsystemState_SUBSYSTEM_DISCONNECTED:
		return domain.SubsystemDisconnected
	case rocsarv1.SubsystemState_SUBSYSTEM_READY:
		return domain.SubsystemReady
	case rocsarv1.SubsystemState_SUBSYSTEM_BUSY:
		return domain.SubsystemBusy
	case rocsarv1.SubsystemState_SUBSYSTEM_ERROR:
		return domain.SubsystemError
	default:
		return domain.SubsystemUnspecified
	}
}
