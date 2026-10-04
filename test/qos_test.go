package test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"testing"

	"github.com/rocsar/obc/internal/config"
	"github.com/rocsar/obc/internal/qos"
)

// fakeExec records the tc invocations and returns canned output.
type fakeExec struct {
	calls [][]string
	// respond maps a substring of the joined command to a canned result.
	respond func(joined string) (string, error)
}

func (f *fakeExec) run(ctx context.Context, name string, args ...string) (string, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	joined := strings.Join(args, " ")
	if f.respond != nil {
		if out, err := f.respond(joined); err != nil || out != "" {
			return out, err
		}
	}
	return "", nil
}

func (f *fakeExec) joined() []string {
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, strings.Join(c, " "))
	}
	return out
}

func newTestShaper(f *fakeExec) *qos.Shaper {
	return qos.NewShaper(slog.New(slog.NewTextHandler(io.Discard, nil)), f.run)
}

// Apply must install the hierarchy the ARCHITECTURE.md 6.6 specifies: HTB root,
// a priority class with a floor and a ceiling at the full link, and a bulk class
// for everything else.
func TestShaperInstallsTheSpecifiedHierarchy(t *testing.T) {
	f := &fakeExec{}
	s := newTestShaper(f)

	ok, reason := s.Apply(context.Background(), "eth0", 115)
	if !ok {
		t.Fatalf("Apply failed: %s", reason)
	}

	joined := strings.Join(f.joined(), "\n")

	for _, want := range []string{
		"qdisc add dev eth0 root handle 1: htb default 10",
		"class add dev eth0 parent 1: classid 1:10 htb rate 41kbit ceil 115kbit",
		"class add dev eth0 parent 1: classid 1:20 htb rate 74kbit ceil 74kbit",
		"qdisc add dev eth0 parent 1:10 handle 110: pfifo",
		"qdisc add dev eth0 parent 1:20 handle 120: pfifo",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing tc invocation:\n  %s\ngot:\n  %s", want, joined)
		}
	}

	// The prior hierarchy is cleared first, or `class add` fails on the second
	// start and shaping silently degrades.
	if len(f.calls) == 0 || strings.Join(f.calls[0], " ") != "tc qdisc del dev eth0 root" {
		t.Errorf("the existing qdisc was not cleared first; first call was %v", f.calls[0])
	}
}

// THE silent-failure trap.
//
// There is no classification filter, and the reason has to survive that.
//
// A filter was shipped here for a long time and never worked: it had no classid,
// so it matched packets and diverted nothing, while the root default sent
// unclassified traffic to the BULK class and made the priority class unreachable.
// Fixing all three problems still did not make it match on the target.
//
// So this asserts two things instead: that no filter is installed (adding one
// back without reading the note in shaper.go would be a regression, not a fix),
// and that telemetry never claims more than a rate cap.
func TestNoClassificationFilterIsInstalled(t *testing.T) {
	f := &fakeExec{}
	s := newTestShaper(f)
	ok, reason := s.Apply(context.Background(), "eth0", 115)
	if !ok {
		t.Fatalf("Apply failed: %s", reason)
	}

	joined := strings.Join(f.joined(), "\n")
	if strings.Contains(joined, "flower") || strings.Contains(joined, "filter") {
		t.Errorf("a classification filter was installed; it does not work on this "+
			"interface and re-adding it will look like a fix while bulk traffic "+
			"still shares the priority class:\n%s", joined)
	}

	// The default must be the priority class. With `default 20` every
	// unclassified packet went to bulk and nothing could reach 1:10 at all.
	if !strings.Contains(joined, "htb default 10") {
		t.Errorf("unclassified traffic does not default to the priority class:\n%s", joined)
	}

	// The rate cap is the part that works, so it must still be there.
	for _, want := range []string{"rate 41kbit ceil 115kbit", "rate 74kbit"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the rate cap lost %q:\n%s", want, joined)
		}
	}

	// And the reason must say what is and is not happening. An empty reason here
	// is how "shaping: active" came to mean "a rate cap exists", which is what an
	// operator has to be told the difference between.
	if reason == "" {
		t.Fatal("Apply reported no reason; telemetry would read as fully shaped")
	}
	for _, want := range []string{"rate limited", "NOT classified"} {
		if !strings.Contains(reason, want) {
			t.Errorf("the reason does not mention %q, so it cannot be acted on: %q", want, reason)
		}
	}

	// Shaping is genuinely in force, so it must not report itself inactive --
	// that would make the link look unprotected when it is capped.
	st := s.Status()
	if !st.ShapingActive {
		t.Error("Status reports shaping inactive, but the rate cap is in force")
	}
	if !strings.Contains(st.InactiveReason, "NOT classified") {
		t.Errorf("Status does not carry the caveat to telemetry: %q", st.InactiveReason)
	}
}

// `tc qdisc del` on a device with no qdisc returns an error, and for us that is
// SUCCESS. Anchored on the exact text, because the message for a missing tc
// binary also contains "not found" and would otherwise read as "nothing to
// delete" -- reporting success when tc is not installed at all, and then nothing
// is shaped.
func TestDeletingAnAbsentQdiscIsSuccessButAMissingTcIsNot(t *testing.T) {
	cases := []struct {
		name    string
		out     string
		err     error
		wantOK  bool // did Apply report success?
		comment string
	}{
		{
			name:   "no qdisc present",
			out:    "RTNETLINK answers: No such file or directory",
			err:    errors.New("exit status 2"),
			wantOK: true,
		},
		{
			name:   "tc not installed",
			out:    `exec: "tc": executable file not found in $PATH`,
			err:    errors.New("executable file not found"),
			wantOK: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeExec{respond: func(joined string) (string, error) {
				if strings.Contains(joined, "qdisc del") {
					return c.out, c.err
				}
				return "", nil
			}}
			s := newTestShaper(f)
			ok, reason := s.Apply(context.Background(), "eth0", 115)
			if ok != c.wantOK {
				t.Errorf("Apply ok = %v (reason %q), want %v", ok, reason, c.wantOK)
			}
			if !ok && reason == "" {
				t.Error("Apply failed without giving a reason the operator can act on")
			}
		})
	}
}

// tc failing must never take down a telemetry server, and the reason must reach
// telemetry so the operator can act on it.
func TestShaperNeverPanicsAndAlwaysExplainsItself(t *testing.T) {
	cases := []struct {
		name       string
		device     string
		rate       uint32
		execErr    error
		execOut    string
		wantOK     bool
		wantSubstr string
	}{
		{"tc missing entirely", "eth0", 115, exec.ErrNotFound, "", false, "tc"},
		{"sudo needs a password", "eth0", 115, errors.New("exit status 1"), "sudo: a password is required", false, "sudo"},
		{"wrong interface", "eth0", 115, errors.New("exit status 1"), "Cannot find device \"eth0\"", false, "Cannot find device"},
		{"kernel without htb", "eth0", 115, errors.New("exit status 2"), "Specified qdisc kind is unknown", false, "unknown"},
		{"empty device", "", 115, nil, "", false, "device"},
		{"zero rate", "eth0", 0, nil, "", false, "greater than zero"},
		{"rate too low to shape", "eth0", 1, nil, "", false, "too low"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeExec{respond: func(string) (string, error) { return c.execOut, c.execErr }}
			s := newTestShaper(f)

			ok, reason := s.Apply(context.Background(), c.device, c.rate)
			if ok != c.wantOK {
				t.Errorf("ok = %v, want %v (reason %q)", ok, c.wantOK, reason)
			}
			if !strings.Contains(strings.ToLower(reason), strings.ToLower(c.wantSubstr)) {
				t.Errorf("reason %q does not mention %q; the operator cannot act on it", reason, c.wantSubstr)
			}
			// And the failure must be visible in telemetry, not just the log.
			st := s.Status()
			if st.ShapingActive {
				t.Error("Status reports shaping active after a failure")
			}
			if st.InactiveReason == "" {
				t.Error("Status gives no reason, so telemetry cannot explain the degradation")
			}
			if s.Active() {
				t.Error("Active() is true after a failure")
			}
		})
	}
}

// "tc unavailable", "no permission" and "disabled by configuration" are three
// different operator actions and must not be collapsed into one boolean.
func TestInactiveReasonDistinguishesCauses(t *testing.T) {
	// Shaping off by configuration.
	null := qos.NewNullShaper("link shaping disabled by configuration")
	st := null.Status()
	if st.ShapingActive {
		t.Error("NullShaper reports active")
	}
	if !strings.Contains(st.InactiveReason, "configuration") {
		t.Errorf("reason %q does not say shaping was disabled deliberately", st.InactiveReason)
	}

	// Shaping on but tc missing.
	f := &fakeExec{respond: func(string) (string, error) { return "", exec.ErrNotFound }}
	s := newTestShaper(f)
	ok, reason := s.Apply(context.Background(), "eth0", 115)
	if ok || reason == "" {
		t.Fatalf("expected failure, got ok=%v reason=%q", ok, reason)
	}
	if strings.Contains(reason, "configuration") {
		t.Error("a missing tc was reported as a configuration decision")
	}
}

// The priority class is derived from the link rate, never configured
// separately. Two independently settable halves are how they come to sum to more
// than the link can carry.
func TestPriorityClassIsDerivedNotConfigured(t *testing.T) {
	cases := []struct {
		rate uint32
		want uint32
	}{
		{115, 41},
		{1000, 360},
		{100, 36},
	}
	for _, c := range cases {
		if got := qos.PriorityKbps(c.rate); got != c.want {
			t.Errorf("PriorityKbps(%d) = %d, want %d", c.rate, got, c.want)
		}
	}
}

// config.BulkPort is still load-bearing, and no longer for the reason it was.
//
// It used to be the dst_port in a tc filter, and ARCHITECTURE.md called the
// coupling silent: change the HTTP port and bulk traffic stops being classified,
// downloads join the priority class, and telemetry starves with nothing reporting
// an error. There is no filter now, so that particular silence is gone.
//
// What remains is that BulkPort names the HTTP artefact port for the in-process
// limiter and for anything that reasons about which traffic is bulk. So it still
// has to equal the port the HTTP server actually listens on -- startup refuses a
// mismatched pair rather than discovering it in flight, and this is the test
// behind that refusal.
func TestBulkPortStillMatchesTheHTTPArtefactPort(t *testing.T) {
	const httpAddr = ":5557"
	if got := config.BulkPort; got != 5557 {
		t.Errorf("config.BulkPort = %d, but the artefact server listens on %s. "+
			"Anything reasoning about which traffic is bulk would be reasoning "+
			"about the wrong port", got, httpAddr)
	}
}
