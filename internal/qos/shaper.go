// Package qos shapes the air link.
//
// This package is the kernel layer, and only the kernel layer: an HTB hierarchy
// on the link device, so the cap is enforced on traffic this process does not
// own. Anything on the Pi can saturate the radio link; a queue in our own process
// cannot help with that. The limiter that used to live in internal/transport is
// gone, and with it the last thing that bounded artefact bytes without a
// capability.
//
// There is ONE class. The tree used to be a priority class and a bulk class,
// with a DefaultClassMinor sending everything to the priority one because no
// classifier could tell the two apart -- so the bulk class was unreachable, the
// priority class was the whole link, and `rate_kbps` silently meant
// PriorityShare * rate_kbps of sustained bandwidth. That is a number an operator
// cannot reason about, and it was the reason for collapsing this: with one class,
// rate_kbps is the cap.
//
// Losing classification costs bandwidth, not correctness: a flat cap needs no
// classifier, and the classes demonstrably do not classify anything on this
// interface anyway. See ARCHITECTURE.md 6.6 for the three faults that retired
// the flower filter, and MinimumRateKbps for the floor that keeps the cap from
// being set below what telemetry needs.
//
// The kernel is reached over rtnetlink, not by running tc. See KernelOps below.
//
// ARCHITECTURE.md 6.6.
package qos

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/rocsar/obc/internal/domain"
	"github.com/rocsar/obc/internal/telemetry"
)

// KernelOps is the traffic control surface the shaper needs from the kernel.
//
// It is an interface for one reason: the hierarchy logic below is the part worth
// testing and worth reading, and it should not be welded to a particular way of
// reaching the kernel. NetlinkOps is the implementation that ships. Swapping in a
// tc-based one is a single type, and the tests here already drive it through a
// fake.
type KernelOps interface {
	// ClearRoot removes the root qdisc. An absent one is success.
	ClearRoot(device string) error
	// AddRootHTB installs the root discipline.
	AddRootHTB(device string) error
	// AddClass adds an HTB class with a rate and a ceiling, both in kbit/s.
	AddClass(device string, classid, parent uint32, rateKbps, ceilKbps uint32) error
	// AddLeaf attaches a queueing discipline under a class.
	AddLeaf(device string, parent, leaf uint32) error
	// Present reports whether a shaping qdisc is actually on the device.
	Present(device string) (bool, error)
}

// MinimumRateKbps is the lowest cap this system will install.
//
// # The derivation, which is the whole point
//
// The link must be able to carry one worst-case telemetry frame per telemetry
// interval. That is the only traffic that cannot wait: everything else on the
// device -- artefact downloads, SSH, anything the SDR or a system service
// pushes -- is opportunistic and is exactly what the cap is there to bound.
//
//	one frame  = telemetry.FrameBudgetBytes = 2048 B
//	per        = telemetry interval          = 1 s
//	demand     = 2048 * 8 / 1 s              = 16.4 kbit/s
//	floor      = ceil(demand)                = 17 kbit/s
//
// # Why the frame BUDGET and not a measured frame
//
// Measured frames are around 400 B, and a floor derived from those would be about
// 5 kbit/s -- a rate at which one resync frame takes a third of a second and the
// 1 Hz design point in ARCHITECTURE.md 6.5 simply stops being true. The budget is
// the worst case the system claims to survive, so it is the honest basis.
//
// # Why it matters at all
//
// Below the floor the console goes stale, and stale is a safety state and not a
// cosmetic one: GUI_ARCHITECTURE.md 6.7 has the console grey its panels and
// stand down motion after three intervals without a frame. A cap that low does
// not make the link "safer", it makes the vehicle invisible. The floor is what
// makes "lower the limit" a request that can be honoured without becoming a
// blind-flying decision.
//
// Derived, never configured, so that the number can be recomputed when the frame
// budget changes rather than argued about. TestFloorMatchesTheTelemetryDemand
// asserts it against the constant it comes from, so the two cannot drift.
func MinimumRateKbps() uint32 {
	// Bytes to bits, per second, rounded up: 2048 * 8 / 1s = 16384 bit/s.
	bitsPerSecond := (uint64(telemetry.FrameBudgetBytes) * 8) / uint64(telemetryIntervalSeconds)
	if bitsPerSecond%1000 != 0 {
		bitsPerSecond += 1000 - bitsPerSecond%1000
	}
	return uint32(bitsPerSecond / 1000)
}

// telemetryIntervalSeconds is the design point MinimumRateKbps assumes.
//
// [telemetry] interval is configurable, and a faster interval would double the
// demand and so raise the floor. That is not wired up: the pipeline is a 1 Hz
// ticker throughout (ARCHITECTURE.md 6.5) and a config key that silently
// invalidated the floor would be worse than one that is not offered. Asserted
// against config's default in TestFloorAssumesTheOneHertzDesignPoint.
const telemetryIntervalSeconds = 1

// TelemetryFrameBudgetBytes exposes the frame size the floor is derived from, so a
// test in another package can assert the two agree rather than the number being
// restated in two places and drifting.
func TelemetryFrameBudgetBytes() int { return telemetry.FrameBudgetBytes }

// Shaper installs the kernel traffic control hierarchy.
type Shaper struct {
	log *slog.Logger
	ops KernelOps

	mu       sync.RWMutex
	device   string
	rateKbps uint32
	active   bool
	reason   string
	// lostAt records when the qdisc was last found missing, so a re-verification
	// loop can report how long the link has been unbounded.
	lostAt time.Time
}

// NewShaper returns a shaper. Pass nil for ops to use rtnetlink.
func NewShaper(log *slog.Logger, ops KernelOps) *Shaper {
	if log == nil {
		log = slog.Default()
	}
	if ops == nil {
		ops = NetlinkOps{}
	}
	return &Shaper{log: log, ops: ops}
}

// Apply installs the hierarchy. It never returns an error.
//
// Traffic control fails for dozens of reasons unrelated to our logic -- no
// CAP_NET_ADMIN, wrong interface, kernel without HTB -- and not one of them
// justifies taking down a telemetry server. The operator gets the real reason in
// telemetry; everything else keeps running unshaped.
func (s *Shaper) Apply(ctx context.Context, device string, rateKbps uint32) (bool, string) {
	if device == "" {
		return s.fail("no link device configured")
	}
	if rateKbps == 0 {
		return s.fail("link rate must be greater than zero")
	}

	// The floor is enforced HERE, not in the command handler, so that neither the
	// startup configuration nor set_link_limit can route around it. Two paths
	// could otherwise set a cap, and the floor is only a floor if both respect it.
	//
	// A too-low request is CLAMPED rather than refused, and the reason says so.
	// The alternative -- refuse and leave the device unconstrained -- turns one
	// typo in a TOML line into an unbounded link, which is the exact failure the
	// limiter this replaced used to prevent. Clamping honours the operator's
	// intent ("limit this link"), corrects the unsafe part, and says what it did.
	floor := MinimumRateKbps()
	clampedMsg := ""
	if rateKbps < floor {
		s.log.Warn("link rate below the telemetry floor; clamping",
			"requested_kbps", rateKbps, "floor_kbps", floor)
		clampedMsg = fmt.Sprintf(
			"requested %d kbit/s is below the %d kbit/s telemetry floor, so the link is "+
				"capped at %d kbit/s instead", rateKbps, floor, floor)
		rateKbps = floor
	}

	// Remove any previous hierarchy first, or the class adds fail on the second
	// start and shaping silently degrades. An absent qdisc is success -- see
	// absentQdisc for what the kernel calls that.
	if err := s.ops.ClearRoot(device); err != nil && !absentQdisc(err) {
		return s.fail(s.describe("could not clear the existing qdisc", err))
	}

	steps := []struct {
		what string
		run  func() error
	}{
		{"root HTB", func() error { return s.ops.AddRootHTB(device) }},
		// The one class, at exactly the requested rate: rate and ceil are the same
		// number because there is no other class to borrow from and no floor above
		// this one to stay under.
		{"shaped class", func() error {
			return s.ops.AddClass(device, ShapedClass, RootHandle, rateKbps, rateKbps)
		}},
		// Leaf qdisc. This is queueing, not shaping; the shaping is the HTB above it.
		{"shaped leaf", func() error { return s.ops.AddLeaf(device, ShapedClass, ShapedLeaf) }},
	}

	for _, step := range steps {
		if err := step.run(); err != nil {
			return s.fail(s.describe("could not install the "+step.what, err))
		}
	}

	// NO CLASSIFICATION FILTER, and that is not an omission. The design was: HTB
	// with a priority class for telemetry and a bulk class for artefact downloads,
	// and a tc flower filter on dst_port 5557 to put HTTP traffic in the bulk class.
	// Three separate things were wrong, and fixing all three was still not enough.
	//
	// 1. The filter had no classid. A flower filter with a match and no classid
	//    classifies nothing -- it inspects packets and then does nothing with
	//    them. Bulk traffic went wherever the root default sent it, filter or no
	//    filter.
	//
	// 2. The root HTB was `default 20`, which is the BULK class. So every
	//    unclassified packet went to bulk and the priority class was unreachable --
	//    nothing could ever land in 1:10, telemetry included.
	//
	// 3. With the classid added, a proper classful tree (explicit root class 1:1,
	//    children parented to it, default 10) and GRO/GSO turned off so packets
	//    reached the filter layer un-coalesced, a 30 KB ranged fetch of a real
	//    artefact still landed in the PRIORITY class and the bulk class stayed at
	//    zero packets.
	//
	// So there is one class and one cap, which is what "limit this link" means to
	// an operator setting a number. Classification -- a guaranteed floor for
	// telemetry while artefacts use the remainder -- is deferred to its own
	// module: it needs a classifier that demonstrably matches on this interface,
	// and guessing between u32, net_cls and nftables is how the last attempt came
	// to be written. Note net_cls would need the artefact listener in its own
	// process, since a cgroup is a property of a process and not of a goroutine or
	// a socket.
	reason := cappedReason(rateKbps, floor)
	if clampedMsg != "" {
		reason = clampedMsg + "; " + reason
	}
	s.set(device, rateKbps, true, reason)
	s.log.Info("link rate applied", "device", device, "rate_kbps", rateKbps,
		"floor_kbps", floor, "clamped", clampedMsg != "")
	return true, s.reasonLocked()
}

// cappedReason is what telemetry reports: the cap is real, and nothing is
// steering traffic within it.
func cappedReason(rateKbps, floorKbps uint32) string {
	return fmt.Sprintf("rate limited to %d kbit/s (floor %d kbit/s); traffic is NOT "+
		"classified -- telemetry and artefact downloads share one class", rateKbps, floorKbps)
}

// absentQdisc reports whether failing to clear a root qdisc means "there was
// nothing to delete", which is success for us, because it is already the state we
// want.
//
// The kernel says it two ways, and both have to be handled:
//
//   - ENOENT: the device has no qdisc at the handle we asked for.
//   - EINVAL: the device's root qdisc exists but has handle zero. Every freshly
//     created interface carries the built-in `noqueue` qdisc, whose handle is 0;
//     the kernel finds it and then refuses, because deleting a handle-zero qdisc
//     would be deleting the device's only queue. This is the case iproute2 prints
//     as "Cannot delete qdisc with handle of zero", which is the string the tc
//     implementation had to match -- carefully, because the message for a missing
//     tc binary also contains "not found" and a loose match there reported
//     success while nothing was shaped.
//
// EINVAL is safe to read as "nothing to delete" only because the request behind
// this is fully determined: one root delete, one fixed handle, one fixed parent.
// No caller input reaches it, so there is nothing here that could turn a malformed
// request into this path.
func absentQdisc(err error) bool {
	return errors.Is(err, syscall.ENOENT) ||
		errors.Is(err, os.ErrNotExist) ||
		errors.Is(err, syscall.EINVAL)
}

// describe turns a kernel error into something an operator can act on.
//
// The errno is the message. "permission denied" on a traffic control call means
// exactly one thing on a flight computer -- the unit is missing CAP_NET_ADMIN --
// and saying so is more useful than passing the raw string through, because the
// raw string does not tell a reader what to change.
func (s *Shaper) describe(what string, err error) string {
	switch {
	case errors.Is(err, syscall.EPERM), errors.Is(err, syscall.EACCES):
		return fmt.Sprintf("%s: permission denied. Link shaping needs CAP_NET_ADMIN; "+
			"grant it to the obc unit or run obc with the capability", what)
	case errors.Is(err, syscall.ENOENT):
		return fmt.Sprintf("%s: the device or the qdisc does not exist (ENOENT)", what)
	case errors.Is(err, syscall.EINVAL), errors.Is(err, syscall.EOPNOTSUPP):
		return fmt.Sprintf("%s: the kernel rejected the request. Either the device "+
			"cannot do HTB or the kernel has no traffic control support (%v)", what, err)
	case errors.Is(err, syscall.ENODEV):
		return fmt.Sprintf("%s: no such network device", what)
	default:
		return fmt.Sprintf("%s: %v", what, err)
	}
}

func (s *Shaper) set(device string, rateKbps uint32, active bool, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.device, s.rateKbps, s.active, s.reason = device, rateKbps, active, reason
	if active {
		s.lostAt = time.Time{}
	}
}

func (s *Shaper) fail(reason string) (bool, string) {
	s.log.Warn("link shaping unavailable", "reason", reason)
	s.set("", 0, false, reason)
	return false, reason
}

func (s *Shaper) reasonLocked() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.reason
}

func (s *Shaper) Active() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.active
}

// Status reports what the shaper believes, for telemetry.
func (s *Shaper) Status() domain.LinkStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()

	st := domain.LinkStatus{
		State:         domain.SubsystemReady,
		Device:        s.device,
		RateKbps:      s.rateKbps,
		PriorityKbps:  s.rateKbps, // one class: the whole cap is the telemetry floor
		ShapingActive: s.active,
	}
	if !s.active {
		// Three different operator actions live behind this one field: the device
		// does not exist, the unit is missing CAP_NET_ADMIN, or shaping is off in
		// configuration. A single bool sends someone to debug the wrong one.
		st.State = domain.SubsystemError
		st.InactiveReason = s.reason
	} else if s.reason != "" {
		// Shaping is in force but something is degraded -- most often that the
		// traffic is not classified, so a download shares the cap with telemetry.
		// That is not the same as inactive and must not read as such.
		st.State = domain.SubsystemBusy
		st.InactiveReason = s.reason
	}
	return st
}

// QdiscPresent reads the device back from the kernel.
//
// This read-back is why the silent classification failure is detectable at all.
// Without it the only evidence that shaping worked is that the calls returned nil,
// which is exactly what happens when the calls are correct and the effect is not.
func (s *Shaper) QdiscPresent(ctx context.Context, device string) (bool, error) {
	return s.ops.Present(device)
}

// Verify re-reads the device and reports whether the cap is genuinely in force.
//
// THIS IS NOT OPTIONAL BOOKKEEPING. Everything that was in-process is gone: the
// token bucket on the artefact path was deleted, so if the kernel tree is not
// actually on the device then the link is UNBOUNDED and nothing else in this
// program knows it. Before that deletion the qdisc going missing degraded one
// producer's rate; now it removes the only limit there is.
//
// And it can go missing. NetworkManager, systemd-networkd, a DHCP renewal or a
// hand-run tc call will each replace the root qdisc, and none of them tells us.
// Active() and Status() read flags cached at Apply time, so before this existed
// telemetry would keep reporting shaping_active=true over an unlimited link.
//
// The two answers differ in kind and are not collapsed: missing means nothing is
// limiting the device and the reason says so; present-but-inactive means our own
// install failed and the reason says that instead.
func (s *Shaper) Verify(ctx context.Context) (bool, string) {
	s.mu.RLock()
	device := s.device
	active := s.active
	s.mu.RUnlock()

	if device == "" {
		// Nothing was ever installed on this shaper; there is nothing to verify.
		return active, s.reasonLocked()
	}

	present, err := s.ops.Present(device)
	if err != nil {
		// A read that failed is not evidence the qdisc is gone. Reporting it as
		// missing would flap the operator's view every time the netlink read
		// hiccuped, and the cost of a false alarm here is a lot of chasing.
		reason := s.describe("could not read the traffic control state back from the kernel", err)
		s.log.Warn("link shaping could not be verified", "device", device, "reason", reason)
		return active, reason
	}

	if present == active {
		return present, s.reasonLocked()
	}

	if present && !active {
		// Something installed a qdisc we did not. Not necessarily ours. Saying so
		// is honest and leaves the operator to decide; claiming success would be
		// the worse error, because our rate may not be the rate in force.
		reason := fmt.Sprintf("a traffic control qdisc is present on %s but this OBC did not "+
			"install one; the effective cap may not be the %d kbit/s reported",
			device, s.Status().RateKbps)
		s.log.Warn("unexpected qdisc on the link device", "reason", reason)
		s.set(device, s.Status().RateKbps, true, reason)
		return true, reason
	}

	// The serious direction: we believe we installed a cap and the kernel says
	// otherwise. Mark it inactive so telemetry stops claiming protection, and
	// re-apply -- our install may simply have been replaced, and putting it back
	// is the difference between a visible blip and an unbounded flight.
	since := ""
	s.mu.Lock()
	if !s.lostAt.IsZero() {
		since = fmt.Sprintf(" (first noticed %s ago)", time.Since(s.lostAt).Round(time.Second))
	} else {
		s.lostAt = time.Now()
	}
	s.active = false
	s.reason = fmt.Sprintf("the traffic control qdisc was removed from %s%s, so the link is "+
		"NOT being limited; re-applying", device, since)
	s.mu.Unlock()

	s.log.Error("link shaping disappeared; the device is unbounded",
		"device", device, "rate_kbps", s.Status().RateKbps)

	if _, reason := s.Apply(ctx, device, s.Status().RateKbps); reason != "" {
		s.log.Error("re-applying link shaping failed", "reason", reason)
	}
	return s.Active(), s.reasonLocked()
}

// NullShaper records requests and changes nothing.
//
// The default for a laptop, a test and CI. A bare `go run` on a developer
// machine must not try to reshape a real NIC -- that needs CAP_NET_ADMIN and it
// is not something to discover by running the thing.
//
// It still REPORTS the rate it was asked to hold, and still refuses to go below
// the floor, so that "shaping is off" reads as "this device is not being limited"
// rather than as "the limit happens to be zero". A zero in the rate field is a
// claim -- a limit of zero kbit/s -- and it is not the claim being made.
type NullShaper struct {
	mu     sync.RWMutex
	reason string
	device string
	// rateKbps is the cap that WOULD be in force, so telemetry and the Ground
	// Station can show the operator what the link is meant to be limited to even
	// on a machine where nothing is limiting it.
	rateKbps uint32
}

// NewNullShaper returns a shaper that does nothing, with the given explanation.
func NewNullShaper(reason string) *NullShaper { return &NullShaper{reason: reason} }

// Configure records the device and rate the OBC was configured with, so Status can
// report the intended cap. It never shapes anything.
//
// The floor is still applied to what it reports: an operator looking at telemetry
// should see the same number the kernel shaper would have installed, not a rate
// the floor would have refused.
func (n *NullShaper) Configure(device string, rateKbps uint32) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.device = device
	n.rateKbps = max(rateKbps, MinimumRateKbps())
}

func (n *NullShaper) Apply(_ context.Context, device string, rateKbps uint32) (bool, string) {
	n.Configure(device, rateKbps)
	return false, n.reason
}

func (n *NullShaper) Active() bool { return false }

func (n *NullShaper) QdiscPresent(context.Context, string) (bool, error) { return false, nil }

// Verify is a no-op that keeps its configured rate. A NullShaper was never
// installing anything, so there is nothing to lose and nothing to re-apply; the
// distinction it exists to preserve is "not limiting this device", which it keeps
// saying rather than flipping between states.
func (n *NullShaper) Verify(context.Context) (bool, string) { return false, n.reasonLocked() }

func (n *NullShaper) reasonLocked() string {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.reason
}

func (n *NullShaper) Status() domain.LinkStatus {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return domain.LinkStatus{
		State:          domain.SubsystemReady,
		Device:         n.device,
		RateKbps:       n.rateKbps,
		PriorityKbps:   n.rateKbps,
		ShapingActive:  false,
		InactiveReason: n.reason,
	}
}
