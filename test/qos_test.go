package test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/rocsar/obc/internal/domain"
	"github.com/rocsar/obc/internal/qos"
)

// fakeOps records the traffic control calls Apply makes and can fail any of them.
//
// It replaces a fake that recorded tc command lines. The calls are recorded as
// structured values rather than as rendered text, so a test asserts the hierarchy
// itself -- handle, parent, rate, ceiling -- instead of matching a command string.
type fakeOps struct {
	mu    sync.Mutex
	calls []string

	// failOn, if non-empty, is a substring of the call that should fail.
	failOn string
	err    error
}

func (f *fakeOps) record(call string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	if f.failOn != "" && strings.Contains(call, f.failOn) {
		return f.err
	}
	return nil
}

func (f *fakeOps) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeOps) ClearRoot(device string) error {
	return f.record("clear " + device)
}

func (f *fakeOps) AddRootHTB(device string) error {
	return f.record("root-htb " + device)
}

func (f *fakeOps) AddClass(device string, classid, parent uint32, rateKbps, ceilKbps uint32) error {
	return f.record(fmt.Sprintf("class %s parent %s rate %d ceil %d",
		qos.HandleString(classid), qos.HandleString(parent), rateKbps, ceilKbps))
}

func (f *fakeOps) AddLeaf(device string, parent, leaf uint32) error {
	return f.record(fmt.Sprintf("leaf %s under %s", qos.HandleString(leaf), qos.HandleString(parent)))
}

func (f *fakeOps) Present(device string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == "root-htb "+device {
			return true, nil
		}
	}
	return false, nil
}

func newTestShaper(f *fakeOps) *qos.Shaper {
	return qos.NewShaper(slog.New(slog.NewTextHandler(io.Discard, nil)), f)
}

// Apply must install the hierarchy the ARCHITECTURE.md 6.6 specifies: HTB root,
// a priority class with a floor and a ceiling at the full link, and a bulk class
// for everything else.
func TestShaperInstallsTheSpecifiedHierarchy(t *testing.T) {
	f := &fakeOps{}
	s := newTestShaper(f)

	ok, reason := s.Apply(context.Background(), "eth0", 115)
	if !ok {
		t.Fatalf("Apply failed: %s", reason)
	}

	got := strings.Join(f.recorded(), "\n")
	for _, want := range []string{
		"clear eth0",
		"root-htb eth0",
		"class 1:a parent 1: rate 41 ceil 115",
		"class 1:14 parent 1: rate 74 ceil 74",
		"leaf 6e: under 1:a",
		"leaf 78: under 1:14",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing traffic control call %q; got:\n%s", want, got)
		}
	}

	// The prior hierarchy is cleared first, or the class adds fail on the second
	// start and shaping silently degrades.
	if first := f.recorded()[0]; first != "clear eth0" {
		t.Errorf("the existing qdisc was not cleared first; first call was %q", first)
	}
}

// The handles are exported and asserted directly, because they are the documented
// shape of the link and a refactor that renumbered them would silently move
// traffic between classes.
func TestHierarchyHandlesMatchTheDocumentedLink(t *testing.T) {
	for _, c := range []struct {
		got  uint32
		want string
	}{
		{qos.RootHandle, "1:"},
		{qos.PriorityClass, "1:a"},
		{qos.BulkClass, "1:14"},
		{qos.PriorityLeaf, "6e:"},
		{qos.BulkLeaf, "78:"},
	} {
		if s := qos.HandleString(c.got); s != c.want {
			t.Errorf("handle = %s, want %s", s, c.want)
		}
	}
	// Unclassified traffic must default to the priority class, not the bulk one.
	if qos.DefaultClassMinor != 10 {
		t.Errorf("default class minor = %d, want 10 -- with default 20 every "+
			"unclassified packet goes to bulk and telemetry is unreachable", qos.DefaultClassMinor)
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
// This asserts the parts that remain: that unclassified traffic still defaults to
// the priority class, that the rate cap is still there, and that telemetry never
// claims more than a rate cap. Adding a filter back is now structurally harder than
// it was -- qos.KernelOps has no method that installs one -- but the reason string
// is what an operator reads, so it is asserted here.
func TestNoClassificationFilterIsInstalled(t *testing.T) {
	f := &fakeOps{}
	s := newTestShaper(f)
	ok, reason := s.Apply(context.Background(), "eth0", 115)
	if !ok {
		t.Fatalf("Apply failed: %s", reason)
	}

	joined := strings.Join(f.recorded(), "\n")
	for _, forbidden := range []string{"filter", "flower", "u32", "net_cls", "nftables"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("something matching %q was installed; classification does not work "+
				"on this interface and re-adding it will look like a fix while bulk traffic "+
				"still shares the priority class:\n%s", forbidden, joined)
		}
	}

	// The default must be the priority class. With `default 20` every
	// unclassified packet went to bulk and nothing could reach 1:10 at all.
	if qos.DefaultClassMinor != 10 {
		t.Errorf("unclassified traffic does not default to the priority class")
	}

	// The rate cap is the part that works, so it must still be there.
	for _, want := range []string{"rate 41 ceil 115", "rate 74 ceil 74"} {
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

// Clearing a device with no hierarchy of ours is success. There are two ways the
// kernel says so -- ENOENT when there is no qdisc at that handle, and EINVAL when
// the device's root qdisc has handle zero, which is what every fresh interface
// carries. The tc implementation had to match both as text.
func TestClearingAnAbsentQdiscIsSuccessButAPermissionFailureIsNot(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		wantOK  bool
		comment string
	}{
		{
			name:   "no qdisc at that handle",
			err:    syscall.ENOENT,
			wantOK: true,
		},
		{
			name:   "root qdisc has handle zero",
			err:    syscall.EINVAL,
			wantOK: true,
		},
		{
			name:   "no capability",
			err:    syscall.EPERM,
			wantOK: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeOps{failOn: "clear eth0", err: c.err}
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

// Traffic control failing must never take down a telemetry server, and the reason
// must reach telemetry so the operator can act on it.
func TestShaperNeverPanicsAndAlwaysExplainsItself(t *testing.T) {
	cases := []struct {
		name       string
		device     string
		rate       uint32
		failOn     string
		err        error
		wantOK     bool
		wantSubstr string
	}{
		// The two that used to be tc-specific, now expressed as what the kernel
		// actually returns. "sudo needs a password" is gone as a concept: there is
		// no sudo, and the failure is a capability the unit either has or does not.
		{"no capability", "eth0", 115, "clear eth0", syscall.EPERM, false, "CAP_NET_ADMIN"},
		{"wrong interface", "eth0", 115, "root-htb eth0", syscall.ENODEV, false, "no such network device"},
		{"kernel without htb", "eth0", 115, "root-htb eth0", syscall.EOPNOTSUPP, false, "traffic control"},

		{"empty device", "", 115, "", nil, false, "device"},
		{"zero rate", "eth0", 0, "", nil, false, "greater than zero"},
		{"rate too low to shape", "eth0", 1, "", nil, false, "too low"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeOps{failOn: c.failOn, err: c.err}
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

// A failure partway through must not be reported as shaping being in force.
//
// This is the case where the kernel accepted the root qdisc and then refused a
// class. The device is left half-shaped, and telemetry claiming "active" would be
// the wrong answer in a way that is hard to notice.
func TestPartialInstallIsNotReportedAsActive(t *testing.T) {
	f := &fakeOps{failOn: "class 1:14", err: syscall.EPERM}
	s := newTestShaper(f)

	ok, reason := s.Apply(context.Background(), "eth0", 115)
	if ok {
		t.Fatal("Apply reported success after the bulk class was refused")
	}
	if s.Active() {
		t.Error("Active() is true after a partial install")
	}
	if !strings.Contains(reason, "bulk") && !strings.Contains(reason, "class") {
		t.Errorf("reason %q does not say which step failed", reason)
	}
}

// "disabled by configuration" and "the kernel refused us" are two different
// operator actions and must not be collapsed into one boolean.
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

	// Shaping on but the kernel refused.
	f := &fakeOps{failOn: "clear eth0", err: syscall.EPERM}
	s := newTestShaper(f)
	ok, reason := s.Apply(context.Background(), "eth0", 115)
	if ok || reason == "" {
		t.Fatalf("expected failure, got ok=%v reason=%q", ok, reason)
	}
	if strings.Contains(reason, "configuration") {
		t.Error("a permission failure was reported as a configuration decision")
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

// The shaper must satisfy the port it is published behind, so a signature change
// here cannot land without the composition root noticing.
var _ domain.LinkShaper = (*qos.Shaper)(nil)
var _ domain.LinkShaper = (*qos.NullShaper)(nil)
