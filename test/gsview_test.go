package test

// Replay and wire-level tests for internal/gsview.
//
// These live here rather than beside the code for one reason: they touch the
// wire. Parsing a captured frame needs the generated bindings and
// transport.DecodeTelemetry, and the layering rule forbids internal/gsview
// exactly those imports -- production code must never touch protobuf, but a
// replay test that cannot decode proves nothing. test/ is unscanned by every
// rule in layering_test.go, which is what makes it the right home, same as the
// codec tests in telemetry_test.go.
//
// The pure tests (Snapshot in, View out, no wire anywhere) live in
// internal/gsview/view_test.go.

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	rocsarv1 "github.com/rocsar/obc/api/rocsar/v1"
	"github.com/rocsar/obc/internal/domain"
	"github.com/rocsar/obc/internal/gsview"
	"github.com/rocsar/obc/internal/telemetry"
	"github.com/rocsar/obc/internal/transport"
)

// liveFrames reads the captured frames. Ten consecutive 1 Hz frames from the
// real vehicle (seq 4382-4391), kept with real coordinates by decision --
// scrubbing them while keeping fix_ok=true would be dishonest data, and a
// bench location is not a secret worth breaking the replay over.
func liveFrames(t *testing.T) []*rocsarv1.TelemetryFrame {
	t.Helper()
	path := filepath.Join("..", "internal", "gsview", "testdata", "live_frames.jsonl")
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open fixtures: %v", err)
	}
	defer f.Close()

	var out []*rocsarv1.TelemetryFrame
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		frame := &rocsarv1.TelemetryFrame{}
		if err := protojson.Unmarshal(sc.Bytes(), frame); err != nil {
			t.Fatalf("fixture is not a TelemetryFrame: %v", err)
		}
		out = append(out, frame)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read fixtures: %v", err)
	}
	return out
}

// The fixtures must decode through the same path as live frames and render
// sane values. If protojson ever rejects what encoding/json wrote, the capture
// format changed and the fixtures need re-capturing -- not a looser parser.
func TestLiveFixturesReplay(t *testing.T) {
	frames := liveFrames(t)
	if len(frames) != 10 {
		t.Fatalf("got %d fixture frames, want 10", len(frames))
	}

	var views []gsview.View
	for _, f := range frames {
		views = append(views, gsview.Build(transport.DecodeTelemetry(f)))
	}

	// Consecutive: the capture is ten straight seconds.
	for i := 1; i < len(views); i++ {
		if views[i].Sequence != views[i-1].Sequence+1 {
			t.Errorf("fixture %d has sequence %d after %d; the capture must be consecutive",
				i, views[i].Sequence, views[i-1].Sequence)
		}
	}

	v := views[0]

	// Receiver 1 holds a real fix; receiver 2 is selected without one; receiver
	// 3 carries nothing. This is the absence discipline against real bytes.
	got := v.GNSS[0].Position
	if got == nil {
		t.Fatal("live fix on receiver 1 rendered as no position")
	}
	if diff := got.LatitudeDeg - 41.178102260772704; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("live latitude rendered %v; degrees in, degrees out", got.LatitudeDeg)
	}
	for _, i := range []int{1, 2} {
		if v.GNSS[i].Position != nil {
			t.Errorf("receiver %d rendered a position with no fix: %+v", i+1, v.GNSS[i].Position)
		}
		if v.GNSS[i].FixAgeS != nil {
			t.Errorf("receiver %d rendered a fix age with no fix", i+1)
		}
	}
	if !v.GNSS[1].Selected {
		t.Error("live selection (receiver 2) lost in the view")
	}

	// Both servos MEASURED, with real loads -- including 105%, unclamped.
	if v.Pico == nil {
		t.Fatal("live Pico block rendered absent")
	}
	if !v.PicoConnected {
		t.Error("live link rendered disconnected")
	}
	if v.Pico.IMU != "MEASURED" {
		t.Errorf("live imu rendered %q", v.Pico.IMU)
	}
	if len(v.Pico.Antennas) != 2 {
		t.Fatalf("live axes rendered %d, want 2", len(v.Pico.Antennas))
	}
	if l := v.Pico.Antennas[1].Load; l == nil || *l != 105 {
		t.Errorf("live load rendered %v, want 105 unclamped", l)
	}

	// The rest of the frame: ack, camera with 2 photos, SDR disconnected with
	// nothing else, shaping off with its reason, no mocks and therefore no
	// banner and a healthy system.
	if v.PicoAck == nil || v.PicoAck.CommandSequence != 5 || !v.PicoAck.Success {
		t.Errorf("live ack rendered %+v, want seq 5 success", v.PicoAck)
	}
	if p := v.Camera.PhotosTaken; p == nil || *p != 2 {
		t.Errorf("live photo count rendered %v, want 2", p)
	}
	if v.SDR.State != "DISCONNECTED" || v.SDR.LastError != nil || v.SDR.LastLog != nil {
		t.Errorf("live SDR rendered %+v; disconnected carries nothing else", v.SDR)
	}
	if v.Link.ShapingActive || v.Link.InactiveReason == nil || v.Link.RateKbps != 0 {
		t.Errorf("live link rendered %+v", v.Link)
	}
	if len(v.System.Mocked) != 0 {
		t.Errorf("live frame carries mocks %v; the vehicle runs real hardware", v.System.Mocked)
	}
	if !v.Healthy || len(v.NotHealthyReasons) != 0 {
		t.Errorf("live frame rendered healthy=%v reasons=%v", v.Healthy, v.NotHealthyReasons)
	}
}

// Feeding the view every absent optional at once must render absence everywhere,
// never a zero. This is the wire-level half of the §7.1 contract: the pure
// half lives in internal/gsview/view_test.go, and this half proves the encoder
// and decoder preserve the absence all the way from a Snapshot.
func TestViewNeverInventsAZero(t *testing.T) {
	s := telemetry.Snapshot{
		Sequence: 1,
		System:   telemetry.SystemSnapshot{State: domain.SubsystemUnspecified},
		GNSS: []domain.ReceiverStatus{
			{ReceiverID: 1, Selected: true},
		},
		// No pico, no ack, no fix, no camera count, no logs, no reason.
		SDR:    telemetry.SDRSnapshot{State: domain.SubsystemUnspecified, PID: -1},
		Camera: domain.SubsystemUnspecified,
		Link:   domain.LinkStatus{},
	}

	v := gsview.Build(transport.DecodeTelemetry(transport.EncodeTelemetry(s)))

	if v.Pico != nil {
		t.Errorf("Pico rendered without a flight controller: %+v", v.Pico)
	}
	if v.PicoAck != nil {
		t.Errorf("ack rendered without an acknowledgement: %+v", v.PicoAck)
	}
	g := v.GNSS[0]
	if g.Position != nil || g.FixAgeS != nil {
		t.Errorf("receiver rendered position=%v age=%v with no fix", g.Position, g.FixAgeS)
	}
	if v.Camera.PhotosTaken != nil || v.Camera.Device != nil || v.Camera.LastPhoto != nil {
		t.Errorf("camera rendered %+v with no camera", v.Camera)
	}
	if v.SDR.LastLog != nil || v.SDR.LastError != nil {
		t.Errorf("sdr rendered %+v with no logs", v.SDR)
	}
	if v.Link.InactiveReason != nil {
		t.Errorf("link rendered a reason where none was given: %q", *v.Link.InactiveReason)
	}

	// And the serialised shape carries explicit nulls, not missing keys: a
	// frontend switching on presence must find the key to switch on.
	body, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"pico":null`, `"position":null`, `"photos_taken":null`} {
		if !strings.Contains(string(body), key) {
			t.Errorf("rendered view lacks %s; absence must be explicit, not omitted", key)
		}
	}
}
