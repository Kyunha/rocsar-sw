package camera

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rocsar/obc/internal/domain"
	"github.com/rocsar/obc/internal/storage"
)

// ---------------------------------------------------------------------------
// The contract
// ---------------------------------------------------------------------------

// Every key rejects a value outside its bounds and accepts one inside them, and
// the error names the range and where the range came from.
//
// Both halves, because a bound that never fires is as bad as one that always
// does: this table is the camera package's only evidence about a program it
// cannot see, and a table whose entries are never exercised is a table nobody
// checked.
func TestEveryNumericKeyIsBounded(t *testing.T) {
	for _, k := range Keys {
		t.Run(k.Name, func(t *testing.T) {
			// Over the top.
			p := set(domain.DefaultCameraParams(), k.Name, k.Max+1)
			err := Validate(p)
			if err == nil {
				t.Fatalf("%s = %d was accepted; the bound is %d..%d",
					k.Name, k.Max+1, k.Min, k.Max)
			}
			if !strings.Contains(err.Error(), k.Name) {
				t.Errorf("error does not name the field: %v", err)
			}
			if !strings.Contains(err.Error(), "fswebcam") {
				t.Errorf("error does not say where the bound came from: %v", err)
			}

			// Under the bottom, when there is a bottom to be under. The JPEG
			// factor is the exception: its floor is 1 because 0 is carried as the
			// automatic case and must survive the check, which is what
			// TestAutomaticQualityIsAccepted asserts.
			if k.Min > 0 && k.Name != "jpeg_quality" {
				p = set(p, k.Name, k.Min-1)
				if err := Validate(p); err == nil {
					t.Errorf("%s = %d was accepted; the bound is %d..%d",
						k.Name, k.Min-1, k.Min, k.Max)
				}
			}

			// Inside, at both edges.
			for _, v := range []uint32{k.Min, k.Max} {
				if err := Validate(set(domain.DefaultCameraParams(), k.Name, v)); err != nil {
					t.Errorf("%s = %d is inside the bound and was refused: %v", k.Name, v, err)
				}
			}
		})
	}
}

// Every key needs a case in TestEveryNumericKeyIsBounded, in the other
// direction: a test case for a key that does not exist is a test that passes.
func TestEveryNumericKeyHasATestCase(t *testing.T) {
	seen := map[string]bool{"jpeg_quality": true, "frames": true, "skip": true, "delay_ms": true}
	for _, k := range Keys {
		if !seen[k.Name] {
			t.Errorf("Keys has %q, which TestEveryNumericKeyIsBounded does not cover", k.Name)
		}
	}
}

// A JPEG quality of 0 is automatic and is NOT a quality of nothing, so it has to
// pass the bound check. If this ever fails, quality 0 is being treated as a
// factor of zero somewhere and every automatic photograph is being encoded at
// the smallest possible size.
func TestAutomaticQualityIsAccepted(t *testing.T) {
	p := domain.DefaultCameraParams()
	if p.JPEGQuality != 0 {
		t.Fatalf("the default quality is %d; the automatic case is carried in the zero", p.JPEGQuality)
	}
	if err := Validate(p); err != nil {
		t.Errorf("the automatic quality was refused: %v", err)
	}
}

// A resolution is validated as a STRING, because the ceiling belongs to a device
// this code cannot see. What can be checked is that it is a shape fswebcam's -r
// can accept at all.
func TestResolutionShapeIsChecked(t *testing.T) {
	good := []string{"384x288", "1280x720", "640x480", "1920x1080", "7680x4320", "1x1"}
	for _, s := range good {
		if err := ValidateResolution(s); err != nil {
			t.Errorf("resolution %q was refused: %v", s, err)
		}
	}

	bad := []struct{ in, why string }{
		{"", "empty"},
		{"1920", "no x"},
		{"x1080", "no width"},
		{"1920x", "no height"},
		{"1920X1080", "capital X, which fswebcam does not take"},
		{"1920x1080p", "trailing text"},
		{"0x480", "a zero width"},
		{"1920x0", "a zero height"},
		{"-1x480", "a negative width"},
		{"19a0x480", "a non-numeric width"},
		{"1920x480x600", "three axes"},
		{"7680x7681", "over the typo-guard ceiling on one axis"},
	}
	for _, tc := range bad {
		if err := ValidateResolution(tc.in); err == nil {
			t.Errorf("resolution %q was accepted (%s)", tc.in, tc.why)
		}
	}
}

// The typo guard is high enough that no real camera is refused by it.
func TestTheResolutionCeilingIsAboveAnyUsableCamera(t *testing.T) {
	if MaxResolutionPixels < 4096 {
		t.Errorf("MaxResolutionPixels is %d, which would refuse 4K", MaxResolutionPixels)
	}
}

// The per-key bounds already keep the worst-case capture inside one command
// round trip, and this is where that is argued rather than in a runtime check.
//
// There was a frames+skip cross-field check here and it was removed: with the
// per-key maxima at 8 and 60 the sum can reach 68, and the bound it checked
// against was 120, so it could never fire. A check that cannot fire is worse
// than no check, because it reads like a decision that is still in force.
//
// The relationship is a test instead. Two budgets, two units, and the units are
// the reason Key.Unit exists: frames is a count, delay is milliseconds, and a
// bound that added them together would be a bound comparing seconds to
// milliseconds -- half of this table is not a frame count at all.
func TestThePerKeyBoundsKeepTheCaptureInsideTheCommandTimeout(t *testing.T) {
	const (
		// A 5 fps floor, not a measurement: the device's frame rate is unknown
		// to this code, so a bound has to survive the slowest one it might be.
		// A USB webcam in a high-compression mode is the slow end of what this
		// hardware does.
		slowestFramesPerSecond = 5
		commandTimeoutS        = 20
	)

	var frames uint32
	for _, k := range Keys {
		if k.Unit == "frames" && k.Max > frames {
			frames = k.Max
		}
	}
	if frames == 0 {
		t.Fatal("no key counts frames; this test has nothing to check")
	}
	seconds := float64(frames) / slowestFramesPerSecond
	if seconds > commandTimeoutS {
		t.Errorf("at most %d frames can be asked for, which is %.1f s at %d fps -- "+
			"longer than the %d s a command round trip gets, so the Ground Station "+
			"gives up on the reply before the photograph is written",
			frames, seconds, slowestFramesPerSecond, commandTimeoutS)
	}

	// The settle delay is milliseconds, and it has the same 20 s to fit inside.
	delay, ok := Lookup("delay_ms")
	if !ok {
		t.Fatal("delay_ms is not in the contract; this test has nothing to check")
	}
	if float64(delay.Max) > commandTimeoutS*1000 {
		t.Errorf("a settle delay of %d ms is longer than the %d s a command round trip gets",
			delay.Max, commandTimeoutS)
	}
}

// One refusal, every bad field. An operator who has mistyped two things should
// learn about both, not fix them one round trip at a time.
func TestEveryProblemIsReportedAtOnce(t *testing.T) {
	p := domain.DefaultCameraParams()
	p.JPEGQuality = 200
	p.Frames = 99
	p.Resolution = "enormous"

	err := Validate(p)
	if err == nil {
		t.Fatal("three bad fields were accepted")
	}
	for _, want := range []string{"jpeg_quality", "frames", "resolution"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error names %v but not %s: %v", err, want, want)
		}
	}
}

// The shipped defaults have to satisfy the contract. If they stop doing so the
// camera falls back to itself at startup, silently, and every photograph
// differs from the one the operator last saw.
func TestTheDefaultsAreInsideTheContract(t *testing.T) {
	if err := Validate(Defaults()); err != nil {
		t.Fatalf("the shipped defaults do not pass their own contract: %v", err)
	}
}

// Lookup finds every key and nothing else.
func TestLookupFindsEveryKey(t *testing.T) {
	for _, k := range Keys {
		if _, ok := Lookup(k.Name); !ok {
			t.Errorf("Lookup(%q) missed", k.Name)
		}
	}
	if _, ok := Lookup("exposure"); ok {
		t.Error("Lookup found a key that is not in Keys")
	}
}

// ---------------------------------------------------------------------------
// Merging a patch
// ---------------------------------------------------------------------------

// A partial update touches only what it names.
func TestAMergeLeavesEverythingElseAlone(t *testing.T) {
	before := domain.DefaultCameraParams()
	q := uint32(45)
	after, err := Merge(before, domain.CameraParamsPatch{JPEGQuality: &q})
	if err != nil {
		t.Fatal(err)
	}
	if after.JPEGQuality != 45 {
		t.Errorf("quality = %d, want 45", after.JPEGQuality)
	}
	if after.Resolution != before.Resolution ||
		after.Frames != before.Frames ||
		after.Skip != before.Skip ||
		after.DelayMs != before.DelayMs {
		t.Errorf("a one-field patch moved the others:\nbefore %+v\nafter  %+v", before, after)
	}
}

// A patch naming nothing is refused rather than treated as a no-op: an operator
// who pressed apply expects a change, and a silent success is indistinguishable
// from a broken button.
func TestAnEmptyPatchIsRefused(t *testing.T) {
	if !Empty(domain.CameraParamsPatch{}) {
		t.Error("Empty reported a zero patch as carrying something")
	}
	for _, p := range []domain.CameraParamsPatch{
		{JPEGQuality: ptr(uint32(1))},
		{Resolution: ptr("1x1")},
		{Frames: ptr(uint32(1))},
		{Skip: ptr(uint32(0))},
		{DelayMs: ptr(uint32(0))},
	} {
		if Empty(p) {
			t.Errorf("Empty(%+v) reported a patch as carrying nothing", p)
		}
	}
}

// A refused merge changes nothing, including the caller's copy.
func TestARefusedMergeReturnsNoSettings(t *testing.T) {
	q := uint32(200)
	got, err := Merge(domain.DefaultCameraParams(), domain.CameraParamsPatch{JPEGQuality: &q})
	if err == nil {
		t.Fatal("quality 200 was accepted")
	}
	if got != (domain.CameraParams{}) {
		t.Errorf("a refused merge returned settings: %+v", got)
	}
}

// ---------------------------------------------------------------------------
// Where the settings live
// ---------------------------------------------------------------------------

func TestSettingsRoundTripThroughTheFile(t *testing.T) {
	dir := t.TempDir()
	store := storage.New(dir, "")

	want := domain.DefaultCameraParams()
	want.JPEGQuality = 55
	want.Resolution = "1280x720"
	want.Frames = 3
	want.Skip = 5
	want.DelayMs = 250

	if err := SaveParams(store, want); err != nil {
		t.Fatalf("SaveParams: %v", err)
	}
	got, err := LoadParams(store)
	if err != nil {
		t.Fatalf("LoadParams: %v", err)
	}
	if got != want {
		t.Errorf("round trip:\n got %+v\nwant %+v", got, want)
	}

	// The file uses the same key names the console is shown, because the console
	// compares the two.
	body, err := os.ReadFile(filepath.Join(dir, ParamsFileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"jpeg_quality", "resolution", "frames", "skip", "delay_ms"} {
		if !strings.Contains(string(body), key) {
			t.Errorf("%s does not name %q; the file and the console must share a vocabulary:\n%s",
				ParamsFileName, key, body)
		}
	}
}

// A file that is absent, unreadable, unparseable or out of bounds all resolve to
// the same thing: no usable stored settings, with the reason attached. Every one
// of them is a hand-edited state, and the answer to each is the same -- fall
// back to settings known to work and say why.
func TestAnUnusableSettingsFileIsRefusedWithAReason(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"not JSON", "{not json", "not the JSON this program writes"},
		{"a bare number", "42", "not the JSON this program writes"},
		{"an out-of-bounds setting", `{"jpeg_quality":200}`, "refuses to use"},
		{"a nonsense resolution", `{"resolution":"enormous"}`, "refuses to use"},
		{"empty", "", "not the JSON this program writes"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			store := storage.New(dir, "")
			if err := os.WriteFile(filepath.Join(dir, ParamsFileName), []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}

			_, err := LoadParams(store)
			if !errors.Is(err, ErrNoParamsFile) {
				t.Fatalf("err = %v, want ErrNoParamsFile", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// No file is the normal state on a first flight, not a failure to report loudly.
func TestAnAbsentSettingsFileIsReportedAsAbsent(t *testing.T) {
	store := storage.New(t.TempDir(), "")
	_, err := LoadParams(store)
	if !errors.Is(err, ErrNoParamsFile) {
		t.Errorf("err = %v, want ErrNoParamsFile", err)
	}
}

// The first camera over a data directory writes the settings in force, so the
// file and the behaviour agree from the first photograph rather than diverging
// the first time somebody changes one.
func TestTheInitialSettingsAreWrittenOnceSoTheySurviveARestart(t *testing.T) {
	dir := t.TempDir()
	device := filepath.Join(dir, "video0")
	if err := os.WriteFile(device, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	store := storage.New(dir, "")
	initial := domain.DefaultCameraParams()
	initial.Resolution = "640x480"

	NewCapture(device, "photos", store, initial, slog.New(discardLogger{}))

	body, err := os.ReadFile(filepath.Join(dir, ParamsFileName))
	if err != nil {
		t.Fatalf("the initial settings were not written: %v", err)
	}
	if !strings.Contains(string(body), "640x480") {
		t.Errorf("%s does not carry the configured resolution:\n%s", ParamsFileName, body)
	}
}

// A stored file beats the configured value, and the OBC says so rather than
// leaving two sources of truth to be discovered in the air.
func TestAStoredFileWinsOverTheConfiguredValue(t *testing.T) {
	dir := t.TempDir()
	device := filepath.Join(dir, "video0")
	if err := os.WriteFile(device, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	store := storage.New(dir, "")

	stored := domain.DefaultCameraParams()
	stored.Resolution = "320x240"
	if err := SaveParams(store, stored); err != nil {
		t.Fatal(err)
	}

	configured := domain.DefaultCameraParams()
	configured.Resolution = "1920x1080"

	cam := NewCapture(device, "photos", store, configured, slog.New(discardLogger{}))
	got, err := cam.Params(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Resolution != "320x240" {
		t.Errorf("resolution in force is %q; the stored file is the designed precedence "+
			"over the configured %q", got.Resolution, configured.Resolution)
	}
}

// Configured settings this program refuses to use must not become the settings
// in force. A camera that fell back silently would photograph at something other
// than what the configuration asked for and say nothing.
func TestUnusableConfiguredSettingsFallBackToTheDefaults(t *testing.T) {
	dir := t.TempDir()
	device := filepath.Join(dir, "video0")
	if err := os.WriteFile(device, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	store := storage.New(dir, "")

	bad := domain.DefaultCameraParams()
	bad.JPEGQuality = 500
	bad.Resolution = "enormous"

	cam := NewCapture(device, "photos", store, bad, slog.New(discardLogger{}))
	got, err := cam.Params(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != Defaults() {
		t.Errorf("settings in force are %+v, want the defaults %+v", got, Defaults())
	}
}

// A refused settings write must not change what the camera is using, or
// telemetry would report settings that the next restart throws away.
func TestARefusedWriteLeavesTheSettingsInForceAlone(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory permissions do not prevent writing")
	}
	h := newHarness(t, 320, 240)
	before, err := h.camera.Params(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// The data directory is made unwritable, which is the closest a test gets to
	// a read-only SSD. NOT the settings FILE: WriteFileAtomic writes a temporary
	// file and renames it over the target, and a rename only needs write
	// permission on the DIRECTORY -- so a read-only file would not stop it, and
	// a test that believed otherwise would pass for the wrong reason.
	if err := os.Chmod(h.dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(h.dir, 0o700) })

	q := uint32(60)
	if err := h.camera.SetParams(context.Background(),
		domain.CameraParamsPatch{JPEGQuality: &q}); err == nil {
		t.Fatal("a settings change into an unwritable data directory succeeded")
	}

	after, err := h.camera.Params(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Errorf("settings changed to %+v after a refused write; they must stay %+v",
			after, before)
	}
}

// A read-only settings file must not stop the camera photographing. Every other
// layer in this system degrades and reports rather than refusing to run.
func TestAnUnwritableSettingsFileDoesNotStopTheCamera(t *testing.T) {
	h := newHarness(t, 320, 240)
	q := uint32(60)
	_ = h.camera.SetParams(context.Background(), domain.CameraParamsPatch{JPEGQuality: &q})

	// Whatever happened to the write, a capture still works.
	if _, err := h.camera.Capture(context.Background()); err != nil {
		t.Fatalf("Capture after a settings problem: %v", err)
	}
}

// A camera can be photographing while its settings are being changed, and the
// photograph must be taken with the settings that were in force when it started.
func TestACaptureUsesTheSettingsThatWereInForceWhenItStarted(t *testing.T) {
	h := newHarness(t, 320, 240)

	done := make(chan struct{})
	go func() {
		defer close(done)
		q := uint32(75)
		for i := 0; i < 50; i++ {
			_ = h.camera.SetParams(context.Background(),
				domain.CameraParamsPatch{JPEGQuality: &q})
		}
	}()

	// Racing the settings file, not asserting anything about who won.
	for i := 0; i < 5; i++ {
		if _, err := h.camera.Capture(context.Background()); err != nil {
			t.Fatalf("Capture: %v", err)
		}
	}
	<-done
}

// withKey sets one field by its contract name, so the tests above can be table
// driven over Keys without a switch per field.
// keysNamed lists the contract names, for the test that keeps the two in step.
func keysNamed() []string {
	out := make([]string, 0, len(Keys))
	for _, k := range Keys {
		out = append(out, k.Name)
	}
	return out
}

// The contract keys are the settings file's keys and the console's JSON fields,
// in that order. A new setting that is not in this list is one whose name can
// drift between the three places it appears.
func TestTheContractNamesTheFileAndTheConsole(t *testing.T) {
	want := []string{"jpeg_quality", "frames", "skip", "delay_ms"}
	got := keysNamed()
	if !slices.Equal(got, want) {
		t.Errorf("Keys = %v, want %v", got, want)
	}
}

// set writes one field of a CameraParams by its contract name and returns it, so
// the tests above can be table driven over Keys instead of carrying a switch per
// field. The two field lists are asserted against each other by
// TestEveryNumericKeyIsBounded.
func set(p domain.CameraParams, name string, v uint32) domain.CameraParams {
	switch name {
	case "jpeg_quality":
		p.JPEGQuality = v
	case "frames":
		p.Frames = v
	case "skip":
		p.Skip = v
	case "delay_ms":
		p.DelayMs = v
	default:
		panic("set: no case for " + name)
	}
	return p
}
