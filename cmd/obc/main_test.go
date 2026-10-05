package main

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rocsar/obc/internal/config"
	"github.com/rocsar/obc/internal/pico"
)

// The composition root is the one place that decides whether a subsystem is real
// or a double, so it is the one place a fallback can hide. These tests exist
// because it did hide one: the flight-controller block seeded its variable with
// a mock and overwrote it on the happy path, so every "degraded" branch left the
// mock installed. An absent Pico then reported pico_connected: true and answered
// jog, heading and heater commands with "acknowledged by the flight controller"
// against no hardware -- and "pico" was missing from the mocked set, so telemetry
// did not name it either.
//
// ARCHITECTURE.md 9 requires the opposite ("There is no fallback-to-mock. If you
// want mock, you ask for it") and names this class of bug twice: once as what
// --mock-gnss did, and once as the reason the remaining three flags are wired by
// eye. These tests are the answer to the second half of that.

func testConfig(t *testing.T, port string) config.Config {
	t.Helper()
	cfg := config.Defaults()
	cfg.Pico.Port = port
	cfg.Pico.Baudrate = 115200
	return cfg
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// absentPort is a device path that cannot exist, which is the case that used to
// leave a mock installed.
func absentPort(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "ttyACM-absent")
}

// TestAMockIsInstalledOnlyWhenItIsAskedFor is the invariant, stated directly.
func TestAMockIsInstalledOnlyWhenItIsAskedFor(t *testing.T) {
	port := absentPort(t)

	asked, _, err := openPico(context.Background(), testConfig(t, port), quietLogger(), true)
	if err != nil {
		t.Fatalf("asking for a mock must not fail: %v", err)
	}
	if _, ok := asked.(*pico.Mock); !ok {
		t.Fatalf("--mock-pico installed %T, want *pico.Mock", asked)
	}

	notAsked, _, err := openPico(context.Background(), testConfig(t, port), quietLogger(), false)
	if err != nil {
		t.Fatalf("a missing device must degrade, not fail: %v", err)
	}
	if _, ok := notAsked.(*pico.Mock); ok {
		t.Fatal("a missing flight controller installed *pico.Mock; that is the " +
			"fallback-to-mock this project calls its worst failure mode")
	}
}

// TestDegradedPicoReportsItselfDisconnected is the property the operator sees.
// "Degrade" means DISCONNECTED in telemetry and an error on every command -- not
// a green light.
func TestDegradedPicoReportsItselfDisconnected(t *testing.T) {
	p, _, err := openPico(context.Background(), testConfig(t, absentPort(t)), quietLogger(), false)
	if err != nil {
		t.Fatalf("a missing device must degrade, not fail: %v", err)
	}

	if p.Connected() {
		t.Error("Connected() is true with no flight controller attached")
	}

	if _, ok := p.Telemetry(); ok {
		t.Error("Telemetry() reports a frame from a flight controller that was never reached")
	}

	// Every command the Ground Station can send, because every one of them used to
	// be acknowledged by the mock that was standing in for the absent device.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if _, err := p.SetTarget(ctx, 90); err == nil {
		t.Error("SetTarget was acknowledged with no flight controller")
	}
	if _, err := p.Jog(ctx, 1, 100); err == nil {
		t.Error("Jog was acknowledged with no flight controller")
	}
	if _, err := p.Zero(ctx, 1); err == nil {
		t.Error("Zero was acknowledged with no flight controller")
	}
	if _, err := p.Mount(ctx, 1, 0); err == nil {
		t.Error("Mount was acknowledged with no flight controller")
	}
	if _, err := p.SetDirection(ctx, 1, 1); err == nil {
		t.Error("SetDirection was acknowledged with no flight controller")
	}
	if _, err := p.SetHeater(ctx, 1, true); err == nil {
		t.Error("SetHeater was acknowledged with no flight controller")
	}
	if _, err := p.Stop(ctx, 1); err == nil {
		t.Error("Stop was acknowledged with no flight controller")
	}
}

// TestRequireHardwareRefusesToDegrade covers the flag that inverts the policy.
// Without this the default is the whole policy, and a typo in it fails open.
func TestRequireHardwareRefusesToDegrade(t *testing.T) {
	cfg := testConfig(t, absentPort(t))
	cfg.RequireHardware = true

	p, _, err := openPico(context.Background(), cfg, quietLogger(), false)
	if err == nil {
		t.Fatalf("--require-hardware returned a %T and no error; it must refuse to start", p)
	}
	if !strings.Contains(err.Error(), cfg.Pico.Port) {
		t.Errorf("error %q does not name the device it could not find (%s)", err, cfg.Pico.Port)
	}
}

// TestTheMockAnswersCommands is the contrast case, and it is a deliberate
// statement about what a mock is for: a bench tool that wants a Pico-shaped
// object without one. It is not a degraded-mode substitute, which is why it is
// reachable only through the flag.
func TestTheMockAnswersCommands(t *testing.T) {
	p, closeFn, err := openPico(context.Background(), testConfig(t, absentPort(t)), quietLogger(), true)
	if err != nil {
		t.Fatalf("asking for a mock must not fail: %v", err)
	}
	defer closeFn()

	if !p.Connected() {
		t.Error("the mock reports itself disconnected")
	}
	ack, err := p.SetTarget(context.Background(), 90)
	if err != nil {
		t.Fatalf("the mock refused a command: %v", err)
	}
	if !ack.Success {
		t.Error("the mock acknowledged a command as failed")
	}
}
