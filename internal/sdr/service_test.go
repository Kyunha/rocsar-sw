package sdr

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rocsar/obc/internal/domain"
)

// seeded builds a Service over a temp tree with a valid params.json, and
// returns the program directory.
//
// The directory is named sdr-ettus-b200mini because Connect refuses anything
// else: the vendored C++ resolves its own config path relative to its working
// directory and only reaches this tree's parameters/ for that name.
func seeded(t *testing.T, body string) *Service {
	t.Helper()
	dir := t.TempDir()
	prog := filepath.Join(dir, "sdr-ettus-b200mini")
	if err := os.MkdirAll(filepath.Join(prog, "parameters"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prog, "parameters", "params.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	// Connect resolves the program before it starts anything, so the binary has
	// to exist even when the tests inject their own start function and never run
	// it. An empty shell script is enough: nothing here executes it.
	if err := os.WriteFile(filepath.Join(prog, programName), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return NewService(prog, t.TempDir(), discardLogger())
}

// validParams is a params.json with every key config.hpp reads.
const validParams = `{
  "PRF": 2750.0,
  "FS": 31251000.0,
  "SESSION_DURATION": 1,
  "TX_FREQ": 5800000000.0,
  "NORMALIZED_GAIN_TX": 0.5,
  "NORMALIZED_GAIN_RX": 0.5,
  "TX_ANTENNA": "TX/RX",
  "RX_ANTENNA": "TX/RX",
  "T_MIN_US": 0,
  "T_MAX_US": 200,
  "START_OFFSET_S": 0.1,
  "PULSE_DURATION": 3.64e-05,
  "BW": 25000000.0
}`

func readRaw(t *testing.T, s *Service) map[string]any {
	t.Helper()
	body, err := os.ReadFile(s.ParamsPath())
	if err != nil {
		t.Fatal(err)
	}
	raw := map[string]any{}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("params.json is not valid JSON after the write: %v", err)
	}
	return raw
}

// ---------------------------------------------------------------------------
// The acquisition lifecycle, and the zombie that used to own it
// ---------------------------------------------------------------------------

// A short-lived child must be observable as dead, and Connect must be available
// again.
//
// This is the bug the whole lifecycle test exists for. startDetached used to
// call Start and return the PID with nothing ever calling Wait, so an exited
// child stayed a zombie for the life of the OBC process, and signal 0 succeeds
// against a zombie. Running() and Connect() both asked the PID, both were told
// the acquisition was alive, and one crashed connect held the SDR for good.
//
// The child here is /bin/true: it exits immediately, which is the exact case the
// old code mishandled.
func TestConnectIsAvailableAgainAfterTheChildExits(t *testing.T) {
	s := seeded(t, validParams)
	s.start = func(_ context.Context, _ string, name string, _ []string, out *os.File) (int, <-chan exitInfo, error) {
		return startDetached(context.Background(), "", name, nil, out)
	}

	// `true` exits 0 at once, so this stands in for a session that completed.
	if err := s.Connect(context.Background()); err != nil {
		t.Fatalf("first connect: %v", err)
	}
	if !s.Running() {
		t.Error("a just-started acquisition reports not running")
	}

	waitFor(t, 5*time.Second, "the acquisition to be reported dead", func() bool {
		return !s.Running()
	})

	// The whole point: the device is free again. The old code refused here with
	// "an acquisition is already running: pid N since <the first start>", naming
	// a PID and a start time that were both true and both misleading.
	if err := s.Connect(context.Background()); err != nil {
		t.Fatalf("a second Connect after the child exited = %v, want it accepted: "+
			"the device is free and this is the refusal operators were stuck behind", err)
	}
	waitFor(t, 5*time.Second, "the second acquisition to finish too", func() bool {
		return !s.Running()
	})
}

// Connect refuses a second acquisition while one is genuinely alive, and says
// which PID is holding it. The counterpart to the test above: the fix must not
// have made the guard toothless.
func TestConnectRefusesWhileTheChildLives(t *testing.T) {
	s := seeded(t, validParams)
	// `sleep` outlives the assertions.
	s.start = func(_ context.Context, _ string, _ string, _ []string, out *os.File) (int, <-chan exitInfo, error) {
		return startDetached(context.Background(), "", "sleep", []string{"30"}, out)
	}

	if err := s.Connect(context.Background()); err != nil {
		t.Fatalf("first connect: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop(context.Background()) })

	err := s.Connect(context.Background())
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second Connect = %v, want ErrAlreadyRunning", err)
	}
	if !strings.Contains(err.Error(), "pid") {
		t.Errorf("refusal %q does not name the PID holding the device", err)
	}
}

// A child that exits non-zero must report WHY, and must not read as idle.
//
// The last_error field existed in the view, in the proto and in the encoder, and
// no provider filled it, so a crashed acquisition reached the operator as a SDR
// that was simply not running.
func TestAFailedAcquisitionReportsItsExitReason(t *testing.T) {
	s := seeded(t, validParams)
	// `false` exits 1 at once.
	s.start = func(_ context.Context, _ string, _ string, _ []string, out *os.File) (int, <-chan exitInfo, error) {
		return startDetached(context.Background(), "", "false", nil, out)
	}

	if err := s.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	waitFor(t, 5*time.Second, "the failure to be recorded", func() bool {
		return s.LastError() != ""
	})

	if got := s.State(); got != domain.SubsystemError {
		t.Errorf("state after a failed acquisition is %s, want ERROR", got)
	}
	msg := s.LastError()
	if !strings.Contains(msg, "pid") {
		t.Errorf("last_error %q does not name the process that died", msg)
	}
	// The exit code is the fact an operator needs: 1 and a crash are different.
	if !strings.Contains(msg, "exit status 1") {
		t.Errorf("last_error = %q, want the exit status in it", msg)
	}
	// The log is where the program's own diagnosis is, so naming it turns a
	// bare exit code into something the operator can act on.
	if !strings.Contains(msg, s.LastLog()) {
		t.Errorf("last_error = %q, want it to point at the log %q", msg, s.LastLog())
	}
}

// A session that runs its full SESSION_DURATION and returns is the NORMAL end
// of a capture, not a failure. Reading the nil error as trouble would make every
// completed capture report ERROR and put "sdr error" in the health banner.
func TestACompletedSessionIsNotAnError(t *testing.T) {
	s := seeded(t, validParams)
	s.start = func(_ context.Context, _ string, _ string, _ []string, out *os.File) (int, <-chan exitInfo, error) {
		return startDetached(context.Background(), "", "true", nil, out)
	}

	if err := s.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	waitFor(t, 5*time.Second, "the session to finish", func() bool {
		return !s.Running()
	})

	if got := s.LastError(); got != "" {
		t.Errorf("a session that completed reported last_error %q, want empty", got)
	}
	if got := s.State(); got != domain.SubsystemReady {
		t.Errorf("state after a completed session is %s, want READY", got)
	}
}

// A Stop we asked for must not be reported as a death nobody caused.
func TestStopIsNotReportedAsAFailure(t *testing.T) {
	s := seeded(t, validParams)
	s.start = func(_ context.Context, _ string, _ string, _ []string, out *os.File) (int, <-chan exitInfo, error) {
		return startDetached(context.Background(), "", "sleep", []string{"30"}, out)
	}

	if err := s.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitFor(t, 5*time.Second, "the stopped acquisition to settle", func() bool {
		return !s.Running()
	})

	if got := s.LastError(); got != "" {
		t.Errorf("a deliberate Stop reported last_error %q, want empty: the operator asked for it", got)
	}
	if got := s.State(); got != domain.SubsystemReady {
		t.Errorf("state after Stop is %s, want READY", got)
	}
}

// A reap belonging to a superseded acquisition must not overwrite the state of
// the one that replaced it. Otherwise a connect refused during the narrow window
// between one run dying and its reap landing would report the new run as not
// running and show the old run's error.
func TestALateReapDoesNotClobberANewerAcquisition(t *testing.T) {
	s := seeded(t, validParams)

	first := make(chan exitInfo, 1)
	s.start = func(context.Context, string, string, []string, *os.File) (int, <-chan exitInfo, error) {
		return 4242, first, nil
	}
	if err := s.Connect(context.Background()); err != nil {
		t.Fatalf("first connect: %v", err)
	}

	// Pretend the operator stopped it out of band, so the next Connect is allowed.
	s.mu.Lock()
	s.running = false
	s.mu.Unlock()

	second := make(chan exitInfo, 1)
	s.start = func(context.Context, string, string, []string, *os.File) (int, <-chan exitInfo, error) {
		return 5555, second, nil
	}
	if err := s.Connect(context.Background()); err != nil {
		t.Fatalf("second connect: %v", err)
	}

	// The FIRST run's reap lands late, with an error in it.
	first <- exitInfo{pid: 4242, err: os.ErrProcessDone}

	// Give the watcher a moment to act on it.
	time.Sleep(200 * time.Millisecond)

	if !s.Running() {
		t.Error("a late reap from a superseded acquisition cleared the current one")
	}
	if got := s.LastError(); got != "" {
		t.Errorf("a late reap wrote last_error %q onto the current acquisition", got)
	}
	if got := s.PID(); got != 5555 {
		t.Errorf("PID = %d, want the current acquisition's 5555", got)
	}

	// Clean up so nothing is left thinking it is running.
	second <- exitInfo{pid: 5555}
	waitFor(t, 5*time.Second, "the current acquisition to settle", func() bool {
		return !s.Running()
	})
}

// ---------------------------------------------------------------------------
// The five keys that were readable but not controllable
// ---------------------------------------------------------------------------

// The sweep window, the arming delay and the antenna ports round-trip. These
// five were read by connect.cpp with j.at() and unreachable from any operator,
// which left the geometry of the capture as a hand edit on the aircraft.
func TestTheSweepWindowAndAntennasAreSettable(t *testing.T) {
	s := seeded(t, validParams)
	ctx := context.Background()

	maxUs := 400.0
	offset := 0.25
	txAnt, rxAnt := "TX/A", "RX/B"
	if err := s.SetParams(ctx, domain.SdrParamsPatch{
		SweepMaxUs:   &maxUs,
		StartOffsetS: &offset,
		TxAntenna:    &txAnt,
		RxAntenna:    &rxAnt,
	}); err != nil {
		t.Fatalf("SetParams: %v", err)
	}

	got, err := s.Params(ctx)
	if err != nil {
		t.Fatalf("Params: %v", err)
	}
	if got.SweepMaxUs != maxUs {
		t.Errorf("T_MAX_US = %g, want %g", got.SweepMaxUs, maxUs)
	}
	if got.SweepMinUs != 0 {
		t.Errorf("T_MIN_US = %g, want the file's 0 left alone", got.SweepMinUs)
	}
	if got.StartOffsetS != offset {
		t.Errorf("START_OFFSET_S = %g, want %g", got.StartOffsetS, offset)
	}
	if got.TxAntenna != txAnt {
		t.Errorf("TX_ANTENNA = %q, want %q", got.TxAntenna, txAnt)
	}
	if got.RxAntenna != rxAnt {
		t.Errorf("RX_ANTENNA = %q, want %q", got.RxAntenna, rxAnt)
	}

	// And they reached the file under the keys the C++ reads, not the names the
	// patch happens to use in Go.
	raw := readRaw(t, s)
	for key, want := range map[string]any{
		"T_MAX_US": 400.0, "START_OFFSET_S": 0.25,
		"TX_ANTENNA": txAnt, "RX_ANTENNA": rxAnt,
	} {
		if raw[key] != want {
			t.Errorf("params.json[%s] = %v, want %v", key, raw[key], want)
		}
	}
	// The inert key is still carried through untouched.
	if _, ok := raw["PULSE_DURATION"]; !ok {
		t.Error("PULSE_DURATION was dropped by the write; connect.cpp ignores it but the key must survive")
	}
}

// config.hpp throws "T_MIN_US must be less than T_MAX_US" from inside main()
// before any radio is initialised. Writing that file leaves an SDR that will not
// start and can only be recovered by hand, so it is refused host-side.
func TestSweepWindowInversionIsRefused(t *testing.T) {
	s := seeded(t, validParams)
	ctx := context.Background()

	before := readRaw(t, s)

	// Lower the floor above the shipped ceiling of 200.
	minUs := 300.0
	err := s.SetParams(ctx, domain.SdrParamsPatch{SweepMinUs: &minUs})
	if !errors.Is(err, ErrInvalidParams) {
		t.Fatalf("inverted sweep window = %v, want ErrInvalidParams", err)
	}
	if !strings.Contains(err.Error(), "T_MIN_US") || !strings.Contains(err.Error(), "T_MAX_US") {
		t.Errorf("error %q does not name both edges", err)
	}

	// The file must be untouched: a refusal that half-writes is worse.
	if got := readRaw(t, s)["T_MIN_US"]; got != before["T_MIN_US"] {
		t.Errorf("T_MIN_US = %v after a refused write, want the file's %v", got, before["T_MIN_US"])
	}
}

// Equal edges are the degenerate case of the same throw and must be refused too.
// The shipped file has T_MIN_US = 0, so lowering the CEILING to 0 is what makes
// the pair equal -- setting the floor to 0 would leave the window at 0..200 and
// change nothing.
func TestAnEmptySweepWindowIsRefused(t *testing.T) {
	s := seeded(t, validParams)

	edge := 0.0
	if err := s.SetParams(context.Background(), domain.SdrParamsPatch{SweepMaxUs: &edge}); err == nil {
		t.Error("T_MIN_US == T_MAX_US was accepted; connect.cpp throws on this at startup")
	}
}

// A single-edge move that keeps the window valid is allowed, and the other edge
// is read from the file rather than defaulting to zero.
func TestMovingOneSweepEdgeKeepsTheOther(t *testing.T) {
	s := seeded(t, validParams)

	minUs := 10.0
	if err := s.SetParams(context.Background(), domain.SdrParamsPatch{SweepMinUs: &minUs}); err != nil {
		t.Fatalf("moving only the lower edge: %v", err)
	}
	raw := readRaw(t, s)
	if raw["T_MIN_US"] != 10.0 {
		t.Errorf("T_MIN_US = %v, want 10", raw["T_MIN_US"])
	}
	if raw["T_MAX_US"] != 200.0 {
		t.Errorf("T_MAX_US = %v, want the file's 200 carried through", raw["T_MAX_US"])
	}
}

// START_OFFSET_S must be strictly positive: config.hpp throws on a zero, and a
// refusal here is the difference between a rejected keystroke and an SDR that
// will not start.
func TestStartOffsetMustBePositive(t *testing.T) {
	s := seeded(t, validParams)

	for _, v := range []float64{0, -1} {
		off := v
		err := s.SetParams(context.Background(), domain.SdrParamsPatch{StartOffsetS: &off})
		if !errors.Is(err, ErrInvalidParams) {
			t.Errorf("START_OFFSET_S = %g = %v, want ErrInvalidParams", v, err)
		}
	}
}

// The new numeric keys are bounded like every other one, because an unbounded
// typo becomes a file the program cannot start.
func TestTheNewKeysAreBounded(t *testing.T) {
	cases := []struct {
		key  string
		good float64
		bad  []float64
	}{
		{"T_MIN_US", 0, []float64{-1}},
		{"T_MAX_US", 200, []float64{2e6}},
		{"START_OFFSET_S", 0.1, []float64{0, 7200}},
	}
	for _, c := range cases {
		if err := ValidateValue(c.key, c.good); err != nil {
			t.Errorf("ValidateValue(%s, %g) = %v, want nil", c.key, c.good, err)
		}
		for _, v := range c.bad {
			if err := ValidateValue(c.key, v); err == nil {
				t.Errorf("ValidateValue(%s, %g) = nil, want a bounds error", c.key, v)
			}
		}
	}
}

// The antenna keys are strings with no numeric bounds, so validation is a no-op
// rather than an error: the legal set belongs to UHD.
func TestAntennaKeysAreNotBoundsChecked(t *testing.T) {
	for _, k := range []string{"TX_ANTENNA", "RX_ANTENNA"} {
		if err := ValidateValue(k, 0); err != nil {
			t.Errorf("ValidateValue(%s, 0) = %v, want nil (no numeric bounds)", k, err)
		}
	}
}

// waitFor polls cond until it holds or the deadline passes.
//
// The watcher runs on its own goroutine, so these are genuinely concurrent
// assertions and a fixed sleep would be either flaky or slow. The condition is
// re-read under the service's own lock.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}