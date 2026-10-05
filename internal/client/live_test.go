package client

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLiveTelemetryReadOnly exercises Stream and List against the real OBC.
//
// Bucket 1 only: it reads telemetry and lists the artefact root. It sends no
// command, takes no photograph, and writes nothing anywhere -- which is why it
// is allowed to exist in the suite at all. Anything that actuates stays manual
// and stays behind an explicit approval, per the live-test envelope.
//
// Skipped unless ROCSAR_LIVE_OBC=1, so no checkout, no CI machine and no
// developer who did not ask for it ever touches the vehicle. The address is the
// OBC's server as deployed, not a fixture.
func TestLiveTelemetryReadOnly(t *testing.T) {
	if os.Getenv("ROCSAR_LIVE_OBC") == "" {
		t.Skip("set ROCSAR_LIVE_OBC=1 to read one live telemetry frame")
	}

	cfg := Config{
		Control:   "tcp://192.168.1.50:5555",
		Telemetry: "tcp://192.168.1.50:5556",
		HTTP:      "http://192.168.1.50:5557",
		Topic:     "telemetry",
	}
	c := New(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	s := newStream(ctx, cfg, 10*time.Second)
	defer s.Close()

	select {
	case fr := <-s.Frames():
		if fr.Telemetry == nil {
			t.Fatal("live stream delivered an empty frame")
		}
		if fr.Telemetry.GetSequence() == 0 {
			t.Error("live frame has sequence 0; the OBC numbers from 1")
		}
		if fr.Telemetry.System == nil {
			t.Error("live frame has no system block")
		}
		t.Logf("live frame: seq=%d gnss=%d pico=%v mocked=%v",
			fr.Telemetry.GetSequence(), len(fr.Telemetry.GetGnss()),
			fr.Telemetry.GetPicoConnected(),
			fr.Telemetry.GetSystem().GetMockedSubsystems())
	case <-ctx.Done():
		t.Fatal("no live frame within 30 s")
	}

	st := s.State()
	if !st.TelemetryConnected {
		t.Errorf("State after a delivered frame is not connected: %+v", st)
	}
	if st.FramesReceived == 0 {
		t.Error("State counts zero received frames after delivering one")
	}

	// The artefact root must decode with the real server on the far side, not
	// just the fake in the other tests.
	listing, err := c.List(ctx, "")
	if err != nil {
		t.Fatalf("live listing: %v", err)
	}
	t.Logf("live listing: %d entries under %q", len(listing.Files), listing.Path)
}
