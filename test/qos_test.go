package test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/rocsar/obc/internal/domain"
	"github.com/rocsar/obc/internal/qos"
	"github.com/rocsar/obc/internal/telemetry"
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

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newTestShaper(f *fakeOps) *qos.Shaper {
	return qos.NewShaper(discardLogger(), f)
}

// Apply must install exactly the tree ARCHITECTURE.md 6.6 specifies: an HTB root,
// ONE class at the configured rate, and a leaf under it.
//
// One class, because there is no classifier to steer traffic into a second one.
// The tree used to be a priority class and a bulk class with everything sent to
// the first, which meant rate_kbps was PriorityShare of the real bandwidth: an
// operator setting 115 got 41 kbit/s of sustained throughput and nothing on
// screen said so.
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
		"class 1:a parent 1: rate 115 ceil 115",
		"leaf 6e: under 1:a",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing traffic control call %q; got:\n%s", want, got)
		}
	}
	if n := strings.Count(got, "class "); n != 1 {
		t.Errorf("%d classes installed, want 1:\n%s", n, got)
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
		{qos.ShapedClass, "1:a"},
		{qos.ShapedLeaf, "6e:"},
	} {
		if s := qos.HandleString(c.got); s != c.want {
			t.Errorf("handle = %s, want %s", s, c.want)
		}
	}
	// Unclassified traffic must land in the class that exists.
	if qos.DefaultClassMinor != 10 {
		t.Errorf("default class minor = %d, want 10 -- it must name the one class, "+
			"or every packet is handed to a handle that is not installed", qos.DefaultClassMinor)
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

	// The rate cap is the part that works, so it must still be there, and it must
	// be the number that was asked for.
	if !strings.Contains(joined, "rate 115 ceil 115") {
		t.Errorf("the rate cap lost or altered the configured rate:\n%s", joined)
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
	f := &fakeOps{failOn: "class 1:a", err: syscall.EPERM}
	s := newTestShaper(f)

	ok, reason := s.Apply(context.Background(), "eth0", 115)
	if ok {
		t.Fatal("Apply reported success after the class was refused")
	}
	if s.Active() {
		t.Error("Active() is true after a partial install")
	}
	if !strings.Contains(reason, "class") {
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

// The floor is derived from the telemetry frame budget, never configured.
//
// Configured separately it would be a number nobody could check, and the two ways
// it goes wrong are both silent: too low and telemetry stops arriving, too high
// and the operator believes the link is protected when it is not.
func TestTheFloorIsDerivedNotConfigured(t *testing.T) {
	if got := qos.MinimumRateKbps(); got != 17 {
		t.Errorf("MinimumRateKbps() = %d, want 17", got)
	}
}

// The floor has to actually fit one worst-case telemetry frame per interval, or
// it is not a floor. This is the assertion that ties the number to the traffic it
// exists to carry.
func TestFloorMatchesTheTelemetryDemand(t *testing.T) {
	floorKbit := uint64(qos.MinimumRateKbps()) * 1000   // bits per second
	demandBit := uint64(telemetry.FrameBudgetBytes) * 8 // one frame per second

	if floorKbit < demandBit {
		t.Errorf("floor %d bit/s cannot carry one %d-byte frame per second (%d bit/s)",
			floorKbit, telemetry.FrameBudgetBytes, demandBit)
	}
	// And not wastefully over: a floor far above the demand is a cap nobody can
	// lower, which defeats the point of having one.
	if floorKbit > demandBit*2 {
		t.Errorf("floor %d bit/s is more than twice the %d bit/s demand; it should be "+
			"the smallest rate that works, not a round number", floorKbit, demandBit)
	}
}

// The floor is the same number the telemetry package computes its frame budget
// from. These are two packages holding one fact, and a drift between them would
// put the floor somewhere the frame budget says the link does not need to be.
func TestFloorTracksTheFrameBudget(t *testing.T) {
	if qos.TelemetryFrameBudgetBytes() != telemetry.FrameBudgetBytes {
		t.Errorf("qos assumes a %d byte frame budget, telemetry.FrameBudgetBytes is %d",
			qos.TelemetryFrameBudgetBytes(), telemetry.FrameBudgetBytes)
	}
}

// The shaper must satisfy the port it is published behind, so a signature change
// here cannot land without the composition root noticing.
var _ domain.LinkShaper = (*qos.Shaper)(nil)
var _ domain.LinkShaper = (*qos.NullShaper)(nil)

// A rate below the telemetry floor is CLAMPED, not refused.
//
// This is the opposite of what the table above asserts for kernel failures, and
// the difference is deliberate. A kernel failure leaves the device alone, which
// is already the state we cannot fix. A too-low rate is a request we can satisfy
// safely: the operator wants the link limited, we cannot let them limit it below
// what telemetry needs, and running it UNBOUNDED would be the worst outcome
// available. Clamping honours the intent, corrects the unsafe part, and says so
// in both the log and telemetry.
//
// It is a policy choice with a real cost -- the number that took effect is not the
// number that was asked for -- and the cost is paid by being loud about it, which
// is what this asserts.
func TestARateBelowTheFloorIsClampedNotRefused(t *testing.T) {
	floor := qos.MinimumRateKbps()

	for _, requested := range []uint32{1, floor - 1} {
		f := &fakeOps{}
		s := newTestShaper(f)

		ok, reason := s.Apply(context.Background(), "eth0", requested)
		if !ok {
			t.Errorf("Apply(%d) refused with %q; the link would then be UNBOUNDED, "+
				"which is worse than capping it too high", requested, reason)
		}
		if got := s.Status().RateKbps; got != floor {
			t.Errorf("Apply(%d) reported a cap of %d kbit/s, want the floor %d",
				requested, got, floor)
		}

		// What was asked for, what was done, and that it was the floor's doing.
		for _, want := range []string{
			strconv.FormatUint(uint64(requested), 10), // the requested number
			"floor",                               // named as a clamp, not a success
			strconv.FormatUint(uint64(floor), 10), // the number that took effect
		} {
			if !strings.Contains(reason, want) {
				t.Errorf("reason %q does not mention %q; the operator cannot tell their "+
					"number was adjusted", reason, want)
			}
		}

		// And the kernel was actually told the floor, not the request.
		joined := strings.Join(f.recorded(), "\n")
		want := fmt.Sprintf("rate %d ceil %d", floor, floor)
		if !strings.Contains(joined, want) {
			t.Errorf("the installed class is not the floor:\n%s", joined)
		}
		// The requested rate must NOT appear as an installed rate anywhere.
		if requested != floor && strings.Contains(joined, fmt.Sprintf("rate %d ", requested)) {
			t.Errorf("a class was installed at the refused rate:\n%s", joined)
		}
	}
}

// The floor is enforced in Apply, so BOTH ways of setting a rate are covered.
//
// It used to live in the set_link_limit handler only. The startup configuration
// installs a cap without going near that handler, which is how a cap no telemetry
// frame would fit into could reach the device from a TOML line. The floor moving
// into Apply is what closed that; this asserts the configuration path cannot get
// under it.
func TestTheStartupConfigurationPathIsFlooredToo(t *testing.T) {
	f := &fakeOps{}
	s := newTestShaper(f)

	// Exactly what cmd/obc/main.go does at start with a too-low link.rate_kbps.
	applied, reason := s.Apply(context.Background(), "eth0", 5)
	if !applied {
		t.Fatalf("startup Apply refused outright: %s", reason)
	}
	if got := s.Status().RateKbps; got < qos.MinimumRateKbps() {
		t.Errorf("the startup path installed %d kbit/s, below the %d kbit/s floor",
			got, qos.MinimumRateKbps())
	}
	if !strings.Contains(reason, "floor") {
		t.Errorf("startup clamp is not explained to the operator: %q", reason)
	}
}

// rate_kbps is the cap. An operator setting a number gets that number, and
// telemetry reports the same one.
//
// Before the collapse it was PriorityShare of it: configure 115 and the kernel
// got a 41 kbit/s priority class carrying everything, so the link sustained 41
// while every surface in this program said 115. That is the trap this asserts
// cannot come back, and it is why the shape of the tree is checked at all.
func TestRateKbpsIsTheCapAndTelemetryAgrees(t *testing.T) {
	for _, rate := range []uint32{17, 48, 115, 230} {
		f := &fakeOps{}
		s := newTestShaper(f)
		if ok, reason := s.Apply(context.Background(), "eth0", rate); !ok {
			t.Fatalf("Apply(%d): %s", rate, reason)
		}

		st := s.Status()
		if st.RateKbps != rate {
			t.Errorf("configured %d kbit/s, telemetry reports %d", rate, st.RateKbps)
		}
		// One class, so the priority figure is the cap: telemetry and downloads
		// share it, and neither can claim more than the other has.
		if st.PriorityKbps != rate {
			t.Errorf("configured %d kbit/s, telemetry reports a priority class of %d; "+
				"with one class these are the same number", rate, st.PriorityKbps)
		}

		joined := strings.Join(f.recorded(), "\n")
		want := fmt.Sprintf("rate %d ceil %d", rate, rate)
		if !strings.Contains(joined, want) {
			t.Errorf("kernel was not given the configured rate; wanted %q in:\n%s", want, joined)
		}
	}
}

// ---------------------------------------------------------------------------
// Re-verification: is the cap actually on the device?
// ---------------------------------------------------------------------------

// stealingOps is a fakeOps whose Present answer can change underneath the shaper,
// standing in for NetworkManager, a DHCP renewal or a hand-run tc replacing the
// root qdisc.
//
// Installing a qdisc clears `stolen`, because that is what installing one does --
// so a transient loss that Verify repairs resolves, and a persistent one does
// not. Every other call behaves normally, so a re-apply genuinely reinstalls.
type stealingOps struct {
	fakeOps
	stolen bool
}

func (s *stealingOps) Present(string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.stolen, nil
}

func (s *stealingOps) steal() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stolen = true
}

func (s *stealingOps) AddRootHTB(device string) error {
	s.mu.Lock()
	s.stolen = false
	s.mu.Unlock()
	return s.fakeOps.AddRootHTB(device)
}

// A transient loss is repaired rather than merely reported.
//
// A DHCP renewal is seconds long and an operator cannot restart the OBC
// mid-flight, so Verify puts the tree back itself and the cap survives the blip.
// The point is that the re-install happens at all: the previous design reported
// whatever the last Apply said and nothing looked again.
func TestAVanishedQdiscIsReapplied(t *testing.T) {
	f := &stealingOps{}
	s := qos.NewShaper(discardLogger(), f)
	if ok, reason := s.Apply(context.Background(), "eth0", 115); !ok {
		t.Fatalf("Apply: %s", reason)
	}
	installed := len(f.recorded())

	f.steal()

	active, reason := s.Verify(context.Background())
	if !active {
		t.Errorf("Verify reported the cap lost after re-installing it (reason %q); "+
			"a repair that is not believed is worse than useless", reason)
	}
	got := f.recorded()
	if len(got) <= installed {
		t.Fatalf("Verify detected the loss but installed nothing: %v", got)
	}
	if !strings.Contains(strings.Join(got, "\n"), "root-htb eth0") {
		t.Errorf("Verify did not re-install the tree:\n%s", strings.Join(got, "\n"))
	}
}

// A loss that CANNOT be repaired must stop being reported as protection.
//
// THIS IS THE TEST THAT MATTERS MOST IN THIS FILE. The in-process token bucket is
// gone, so the kernel tree is the only limiter in the program: if it is not on the
// device then the link is UNBOUNDED. Before Verify existed, Active and Status
// answered from flags written at install time, so a qdisc that could not be
// reinstated left telemetry claiming shaping_active = true over a link nothing
// was limiting -- and no other part of this system could tell.
func TestAnUnrepairableLossStopsBeingReportedAsProtection(t *testing.T) {
	f := &stealingOps{}
	s := qos.NewShaper(discardLogger(), f)
	if ok, reason := s.Apply(context.Background(), "eth0", 115); !ok {
		t.Fatalf("Apply: %s", reason)
	}

	// The qdisc is taken, and we cannot put one back -- the same shape as the
	// capability being revoked, or the device going away.
	f.steal()
	f.failOn, f.err = "clear eth0", syscall.EPERM

	active, reason := s.Verify(context.Background())
	if active {
		t.Error("Verify reports the cap in force after an unrepairable loss; " +
			"telemetry would be claiming protection over an unbounded link")
	}
	if !strings.Contains(reason, "permission denied") && !strings.Contains(reason, "CAP_NET_ADMIN") {
		t.Errorf("reason %q does not say why the cap could not be restored", reason)
	}

	// Telemetry must carry it, not just the log.
	st := s.Status()
	if st.ShapingActive {
		t.Error("Status still reports ShapingActive over an unbounded link")
	}
	if st.State != domain.SubsystemError {
		t.Errorf("State = %s, want ERROR while the link is unbounded", st.State)
	}
	if st.InactiveReason == "" {
		t.Error("Status gives no reason, so telemetry cannot explain the degradation")
	}
}

// A read that FAILS is not evidence the qdisc is gone.
//
// The distinction matters in both directions and collapsing them is how a
// read-back becomes noise an operator learns to ignore. A transient netlink error
// must not flap telemetry into ERROR on an intact, capped link.
func TestAFailedVerificationReadIsNotTreatedAsALostQdisc(t *testing.T) {
	f := &failingPresentOps{fakeOps{}, errors.New("netlink read failed")}
	s := qos.NewShaper(discardLogger(), f)
	if ok, reason := s.Apply(context.Background(), "eth0", 115); !ok {
		t.Fatalf("Apply: %s", reason)
	}

	active, reason := s.Verify(context.Background())
	if !active {
		t.Error("a failed read reported the cap as lost; the qdisc may well still be there")
	}
	if !strings.Contains(reason, "could not read") {
		t.Errorf("reason %q does not say the read failed, so it reads as a verdict "+
			"about the link rather than about our visibility of it", reason)
	}
	if !s.Active() {
		t.Error("Active() went false on a read error")
	}
}

// failingPresentOps cannot answer the read-back.
type failingPresentOps struct {
	fakeOps
	err error
}

func (f *failingPresentOps) Present(string) (bool, error) { return false, f.err }

// A NullShaper reports the cap it would have held rather than zero.
//
// A zero in the rate field is a claim -- a limit of zero kbit/s -- and it is not
// the claim being made. "This device is not being limited" and "this device is
// limited to nothing" are different sentences and the operator needs the first.
func TestNullShaperReportsTheConfiguredRateNotZero(t *testing.T) {
	n := qos.NewNullShaper("link shaping disabled by configuration")
	n.Configure("eth0", 115)

	st := n.Status()
	if st.RateKbps != 115 {
		t.Errorf("NullShaper reports %d kbit/s, want the configured 115", st.RateKbps)
	}
	if st.Device != "eth0" {
		t.Errorf("NullShaper reports device %q, want eth0", st.Device)
	}
	if st.ShapingActive {
		t.Error("NullShaper claims to be active")
	}
	if !strings.Contains(st.InactiveReason, "configuration") {
		t.Errorf("reason %q does not say shaping was disabled deliberately", st.InactiveReason)
	}

	// And it still refuses to report a cap below the floor, so an operator looking
	// at telemetry sees the number the kernel shaper would have installed.
	n.Configure("eth0", 5)
	if got := n.Status().RateKbps; got < qos.MinimumRateKbps() {
		t.Errorf("NullShaper reports %d kbit/s, below the %d kbit/s floor", got, qos.MinimumRateKbps())
	}

	// Verify on a NullShaper is a no-op that keeps saying "not limiting".
	if active, _ := n.Verify(context.Background()); active {
		t.Error("NullShaper.Verify claims to be active")
	}
}
