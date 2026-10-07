package qos

import (
	"fmt"

	"github.com/vishvananda/netlink"
)

// Traffic control handle encoding.
//
// tc prints handles as major:minor, e.g. "1:10". rtnetlink carries the same pair
// packed into one uint32 as major<<16|minor. The constants below are written in
// the tc notation so they can be compared against ARCHITECTURE.md and against a
// human running `tc` by hand on the target.
const (
	handleUnit = 1 << 16 // one minor number

	// The tree, in tc notation. Exported because they are the documented shape of
	// the link (ARCHITECTURE.md 6.6) and a test should be able to assert that
	// shape rather than a rendering of it.
	RootHandle  = 1*handleUnit + 0   // 1:
	ShapedClass = 1*handleUnit + 10  // 1:a
	ShapedLeaf  = 110*handleUnit + 0 // 110:

	// DefaultClassMinor points every unclassified flow at the one class there is.
	// There is no second class to be wrong about: see shaper.go for why the tree
	// was collapsed, and what that cost.
	DefaultClassMinor = 10
)

// NetlinkOps installs the hierarchy through rtnetlink.
//
// This replaces shelling out to tc. What that bought:
//
//   - No subprocess, so no sudo, no PATH, no 5-second timeout for a password
//     prompt that will never arrive. The capability is checked by the kernel and
//     the error says so.
//   - Typed errors. "There is no such qdisc" was previously a substring match on
//     tc's stderr, with a documented risk of matching a missing binary instead;
//     it is now syscall.ENOENT.
//   - Still one process, still no third-party binary on the flight computer.
//
// What it did not change: the hierarchy. Same handles, same rates, same absence of
// a classification filter. See the note in Apply.
type NetlinkOps struct{}

var _ KernelOps = NetlinkOps{}

func (NetlinkOps) linkIndex(device string) (int, error) {
	link, err := netlink.LinkByName(device)
	if err != nil {
		return 0, fmt.Errorf("cannot find network device %q: %w", device, err)
	}
	return link.Attrs().Index, nil
}

// ClearRoot removes any previous hierarchy.
//
// Parent is HANDLE_ROOT for the same reason as in AddRootHTB: a delete addressed
// to the root must say it is addressed to the root.
//
// The error is returned raw. Whether an absent qdisc is a failure or the state
// already wanted is policy, and Shaper.Apply is where policy lives -- so that it
// can be asserted without a real network namespace. Two errnos mean "there was
// nothing there", and Shaper.Apply knows about them.
func (NetlinkOps) ClearRoot(device string) error {
	index, err := NetlinkOps{}.linkIndex(device)
	if err != nil {
		return err
	}
	root := &netlink.Htb{QdiscAttrs: netlink.QdiscAttrs{
		LinkIndex: index,
		Handle:    RootHandle,
		Parent:    netlink.HANDLE_ROOT,
	}}
	if err := netlink.QdiscDel(root); err != nil {
		return fmt.Errorf("could not clear the existing qdisc on %s: %w", device, err)
	}
	return nil
}

// AddRootHTB installs the root discipline, with unclassified traffic defaulting to
// the one class this tree has.
//
// Parent is HANDLE_ROOT, not zero. A root qdisc has to say so explicitly: the
// zero value tells the kernel to look for a parent qdisc with major number 0,
// which does not exist, and the call fails with ENOENT. Nothing about that error
// suggests the parent field is the problem.
func (NetlinkOps) AddRootHTB(device string) error {
	index, err := NetlinkOps{}.linkIndex(device)
	if err != nil {
		return err
	}
	htb := netlink.NewHtb(netlink.QdiscAttrs{
		LinkIndex: index,
		Handle:    RootHandle,
		Parent:    netlink.HANDLE_ROOT,
	})
	htb.Defcls = DefaultClassMinor
	if err := netlink.QdiscAdd(htb); err != nil {
		return fmt.Errorf("could not install the root HTB qdisc on %s: %w", device, err)
	}
	return nil
}

// AddClass adds one HTB class.
//
// Rate and ceiling are given in kbit/s, the unit the configuration and
// ARCHITECTURE.md use, and are converted to the bits/s that NewHtbClass expects.
// That helper divides by 8 itself to reach the bytes/s the kernel wants, and it
// derives the buffer and cell buffer from the rate and the MTU exactly as tc does
// -- so the shaping characteristics measured on the target (a 41 kbit/s floor, a
// 115 kbit/s ceiling) are preserved by the swap.
func (NetlinkOps) AddClass(device string, classid, parent uint32, rateKbps, ceilKbps uint32) error {
	index, err := NetlinkOps{}.linkIndex(device)
	if err != nil {
		return err
	}
	class := netlink.NewHtbClass(
		netlink.ClassAttrs{LinkIndex: index, Handle: classid, Parent: parent},
		netlink.HtbClassAttrs{Rate: uint64(rateKbps) * 1000, Ceil: uint64(ceilKbps) * 1000},
	)
	if err := netlink.ClassAdd(class); err != nil {
		return fmt.Errorf("could not add HTB class %s to %s at %d kbit/s: %w",
			HandleString(classid), device, rateKbps, err)
	}
	return nil
}

// AddLeaf attaches a queueing discipline under a class. It is the leaf, not the
// shaping: the rate and ceiling above are what bound the link.
//
// This is fq_codel where the previous implementation used `pfifo limit N`, and
// the change is forced rather than chosen: rtnetlink's plain-FIFO leaf carries no
// settable limit in the Go binding (PfifoFast derives its queue length from the
// device's txqueuelen instead), so the old `pfifo limit 200` cannot be expressed.
// fq_codel is self-tuning and actively holds the queue short, which on a
// 115 kbit/s link is what keeps a bulk transfer from inflating latency for the
// telemetry sharing it.
func (NetlinkOps) AddLeaf(device string, parent, handle uint32) error {
	index, err := NetlinkOps{}.linkIndex(device)
	if err != nil {
		return err
	}
	leaf := netlink.NewFqCodel(netlink.QdiscAttrs{LinkIndex: index, Handle: handle, Parent: parent})
	if err := netlink.QdiscAdd(leaf); err != nil {
		return fmt.Errorf("could not attach a leaf qdisc to class %s on %s: %w",
			HandleString(parent), device, err)
	}
	return nil
}

// Present reads the device back from the kernel.
//
// This read-back is why the silent classification failure was detectable at all.
// Without it the only evidence that shaping worked is that the calls returned nil,
// which is exactly what happens when the calls are correct and the effect is not.
//
// Only a real HTB qdisc counts. A device whose root qdisc is the built-in `noqueue`
// is not shaped, and reporting it as shaped is what would make this useless: after
// a successful ClearRoot the device is exactly in that state, and a read-back that
// called that "present" could never report a missing hierarchy.
func (NetlinkOps) Present(device string) (bool, error) {
	link, err := netlink.LinkByName(device)
	if err != nil {
		return false, fmt.Errorf("cannot find network device %q: %w", device, err)
	}
	qdiscs, err := netlink.QdiscList(link)
	if err != nil {
		return false, fmt.Errorf("cannot read the qdiscs on %s: %w", device, err)
	}
	for _, q := range qdiscs {
		if _, ok := q.(*netlink.Htb); ok {
			return true, nil
		}
	}
	return false, nil
}

// HandleString renders a packed handle back into tc's major:minor notation.
//
// A minor of zero prints as a bare colon, because that is how tc prints it: "1:",
// not "1:0". A message an operator compares against `tc qdisc show` output has to
// look like that output.
func HandleString(h uint32) string {
	if h&0xffff == 0 {
		return fmt.Sprintf("%x:", h>>16)
	}
	return fmt.Sprintf("%x:%x", h>>16, h&0xffff)
}
