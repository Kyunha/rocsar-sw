// Package qos shapes the air link.
//
// Two layers, because they solve different problems and confusing them is how
// you end up with neither working:
//
//   - Userspace (Limiter): a priority queue and a token bucket in front of the
//     telemetry socket. It stops a bulk transfer from monopolising the socket's
//     send path before the kernel ever sees the traffic.
//
//   - Kernel (Shaper): an HTB hierarchy on the link device, so the limit is
//     enforced on traffic this process does not own. Anything on the Pi can
//     saturate the radio link; a queue in our own process cannot help with that.
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

	"github.com/rocsar/obc/internal/domain"
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

// PriorityShare is the fraction of the link reserved for telemetry and commands.
//
// 0.36 of 115 kbit/s is about 41 kbit/s, which is roughly 20x a 1 Hz telemetry
// frame. The headroom is deliberate: the frame budget in ARCHITECTURE.md is a
// cap, and a link that is exactly big enough for the average frame has no room
// for the one that carries a resync.
const PriorityShare = 0.36

// Shaper installs the kernel traffic control hierarchy.
type Shaper struct {
	log *slog.Logger
	ops KernelOps

	mu       sync.RWMutex
	device   string
	rateKbps uint32
	prioKbps uint32
	active   bool
	reason   string
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

// PriorityKbps returns the priority class ceiling for a link rate.
//
// Derived, not configured: the priority class and the bulk class are two halves
// of one number, and letting them be set independently is how they come to sum
// to more than the link can carry.
func PriorityKbps(rateKbps uint32) uint32 {
	return uint32(float64(rateKbps) * PriorityShare)
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
	prio := PriorityKbps(rateKbps)
	if prio == 0 {
		// A rate low enough that the priority class rounds to zero would leave
		// telemetry with no guaranteed bandwidth at all, which defeats the
		// point of having a priority class.
		return s.fail(fmt.Sprintf("link rate %d kbit/s is too low: the priority class would be %d kbit/s",
			rateKbps, prio))
	}

	bulk := rateKbps - prio

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
		// default 10, not 20: unclassified traffic belongs in the class with a
		// guaranteed floor, so a device with no classifier still protects
		// telemetry rather than the other way round.
		{"root HTB", func() error { return s.ops.AddRootHTB(device) }},

		// Priority class: a guaranteed floor at prio, free to borrow up to the
		// full link when nothing else wants it.
		{"priority class", func() error {
			return s.ops.AddClass(device, PriorityClass, RootHandle, prio, rateKbps)
		}},

		// Bulk class: everything else, flat.
		{"bulk class", func() error {
			return s.ops.AddClass(device, BulkClass, RootHandle, bulk, bulk)
		}},

		// Leaf qdiscs. These are queueing, not shaping; the shaping is the HTB
		// above them.
		{"priority leaf", func() error { return s.ops.AddLeaf(device, PriorityClass, PriorityLeaf) }},
		{"bulk leaf", func() error { return s.ops.AddLeaf(device, BulkClass, BulkLeaf) }},
	}

	for _, step := range steps {
		if err := step.run(); err != nil {
			return s.fail(s.describe("could not install the "+step.what, err))
		}
	}

	// NO CLASSIFICATION FILTER. This is a rate cap and nothing more, and the
	// reason is written down here so nobody re-adds a filter without reading it.
	//
	// The design was: HTB with a priority class for telemetry and a bulk class for
	// artefact downloads, and a tc flower filter on dst_port 5557 to put HTTP
	// traffic in the bulk class. Three separate things were wrong, and fixing all
	// three was still not enough.
	//
	// 1. The filter had no classid. A flower filter with a match and no classid
	//    classifies nothing -- it inspects packets and then does nothing with
	//    them. Bulk traffic went wherever the root default sent it, filter or no
	//    filter.
	//
	// 2. The root HTB was `default 20`, which is the BULK class. So every
	//    unclassified packet went to bulk and the priority class was
	//    unreachable -- nothing could ever land in 1:10, telemetry included.
	//
	// 3. With the classid added, a proper classful tree (explicit root class 1:1,
	//    children parented to it, default 10) and GRO/GSO turned off so packets
	//    reached the filter layer un-coalesced, a 30 KB ranged fetch of a real
	//    artefact still landed in the PRIORITY class at 41 kbit/s and the bulk
	//    class stayed at zero packets.
	//
	// So: the rate cap works and is kept, because a shared 115 kbit/s radio link
	// genuinely should be capped and the HTB classes demonstrably enforce it
	// (measured on the target: 41 kbit/s floor, 115 kbit/s ceiling, bulk 74).
	// Classification does not work on this interface and is not shipped as though
	// it did.
	//
	// What replaces it in the minimal system is a limiter in THIS process on the
	// artefact HTTP path, driven by qos.bulk_rate_bps. That bounds the traffic we
	// produce, which is the traffic that was actually starving telemetry. It is
	// not link shaping and does not pretend to be: it cannot constrain the SDR, a
	// system service, or anything else on the box.
	//
	// Proper per-class link constraint is deferred to its own module. It needs a
	// working classifier on this interface, and finding one is a piece of work in
	// its own right -- u32 on the TCP port, net_cls on the listener's cgroup, or
	// nftables. Guessing between those from here is how the current filter got
	// written. Note that net_cls would need the artefact listener in its own
	// process, since a cgroup is a property of a process and not of a goroutine or
	// a socket, so it is not a small change either.
	//
	// The default is the priority class, so an unconfigured device caps the whole
	// link rather than dumping everything into a class named "bulk" that nothing is
	// being deliberately steered into.
	s.set(device, rateKbps, prio, true, unclassifiedReason(rateKbps, prio))
	s.log.Info("link rate applied; traffic is NOT classified",
		"device", device, "rate_kbps", rateKbps, "priority_kbps", prio,
		"unclassified_default", "1:10")
	return true, s.reasonLocked()
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

func (s *Shaper) set(device string, rateKbps, prioKbps uint32, active bool, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.device, s.rateKbps, s.prioKbps, s.active, s.reason = device, rateKbps, prioKbps, active, reason
}

func (s *Shaper) fail(reason string) (bool, string) {
	s.log.Warn("link shaping unavailable", "reason", reason)
	s.set("", 0, 0, false, reason)
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
		PriorityKbps:  s.prioKbps,
		ShapingActive: s.active,
	}
	if !s.active {
		// Three different operator actions live behind this one field: tc is
		// missing, sudo is not permitted, or shaping is off in configuration.
		// A single bool sends someone to debug the wrong one.
		st.State = domain.SubsystemError
		st.InactiveReason = s.reason
	} else if s.reason != "" {
		// Shaping is in force but something is degraded -- the bulk filter, so
		// far. That is not the same as inactive and must not read as such.
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

// NullShaper records requests and changes nothing.
//
// The default for a laptop, a test and CI. A bare `go run` on a developer
// machine must not try to reshape a real NIC -- that needs CAP_NET_ADMIN and it
// is not something to discover by running the thing.
type NullShaper struct {
	mu     sync.RWMutex
	reason string
}

// NewNullShaper returns a shaper that does nothing, with the given explanation.
func NewNullShaper(reason string) *NullShaper { return &NullShaper{reason: reason} }

func (n *NullShaper) Apply(context.Context, string, uint32) (bool, string) { return false, n.reason }
func (n *NullShaper) Active() bool                                         { return false }
func (n *NullShaper) QdiscPresent(context.Context, string) (bool, error)   { return false, nil }

func (n *NullShaper) Status() domain.LinkStatus {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return domain.LinkStatus{
		State:          domain.SubsystemReady,
		ShapingActive:  false,
		InactiveReason: n.reason,
	}
}

// unclassifiedReason is what telemetry reports when the rate cap is in force but
// nothing is steering traffic between classes.
//
// It exists so that "shaping" never reads as more than it is. The previous
// version of this code reported shaping as active with no reason at all, while
// the filter that was supposed to classify bulk traffic silently did nothing --
// so an operator reading telemetry had no way to tell a working priority class
// from a decorative one.
func unclassifiedReason(rateKbps, prioKbps uint32) string {
	return fmt.Sprintf("rate limited to %d kbit/s (%d kbit/s priority floor); "+
		"traffic is NOT classified -- a download shares the priority class with telemetry",
		rateKbps, prioKbps)
}
