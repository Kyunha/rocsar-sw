package test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"testing"

	"github.com/rocsar/obc/internal/config"
	"github.com/rocsar/obc/internal/domain"
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
		"qdisc add dev eth0 root handle 1: htb default 20",
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
// The flower filter is what makes bulk traffic land in the bulk class. Without
// it every command still succeeds, telemetry still says shaping is active, and
// the only symptom is that telemetry slows down when a file moves. This asserts
// the filter is installed AND that its absence is reported rather than swallowed.
func TestBulkFlowerFilterIsInstalledAndItsAbsenceIsReported(t *testing.T) {
	// With the filter working.
	f := &fakeExec{}
	s := newTestShaper(f)
	ok, reason := s.Apply(context.Background(), "eth0", 115)
	if !ok {
		t.Fatalf("Apply failed: %s", reason)
	}
	joined := strings.Join(f.joined(), "\n")
	if !strings.Contains(joined, "flower dst_port 5557") {
		t.Errorf("the bulk flower filter was not installed; bulk traffic would fall through to the default class.\ngot:\n%s", joined)
	}
	if !strings.Contains(joined, "parent 1:") || !strings.Contains(joined, "prio 20") {
		t.Errorf("the filter is not attached to the HTB root with a distinct priority:\n%s", joined)
	}
	if reason != "" {
		t.Errorf("a fully successful Apply reported a reason: %s", reason)
	}

	// With the filter failing. Shaping still holds; the classification does not.
	f2 := &fakeExec{respond: func(joined string) (string, error) {
		if strings.Contains(joined, "filter") {
			return "Error: Cannot add filter", errors.New("exit status 1")
		}
		return "", nil
	}}
	s2 := newTestShaper(f2)
	ok2, reason2 := s2.Apply(context.Background(), "eth0", 115)
	if !ok2 {
		t.Errorf("a missing filter made Apply report shaping as entirely off (%s); shaping IS in force, the classification is not", reason2)
	}
	if !strings.Contains(reason2, "filter") {
		t.Errorf("a missing bulk filter was not explained: %q", reason2)
	}
	// And it must not read as a healthy system.
	st := s2.Status()
	if st.State == domain.SubsystemReady {
		t.Error("Status reports READY with a missing bulk filter")
	}
	if !st.ShapingActive {
		t.Error("Status reports shaping inactive, but it is in force")
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

// The flower's dst_port and the HTTP server's port are one fact in two places.
// The shaper is built from the constant, and the config validation refuses a
// mismatched pair, so the two cannot silently disagree.
func TestShaperFilterPortMatchesTheHTTPBulkPort(t *testing.T) {
	f := &fakeExec{}
	s := newTestShaper(f)
	if _, reason := s.Apply(context.Background(), "eth0", 115); !strings.Contains(reason, "") || reason != "" {
		t.Fatalf("unexpected reason: %s", reason)
	}
	joined := strings.Join(f.joined(), "\n")
	want := fmt.Sprintf("dst_port %d", config.BulkPort)
	if !strings.Contains(joined, want) {
		t.Errorf("the filter does not use config.BulkPort (%d):\n%s", config.BulkPort, joined)
	}
}
