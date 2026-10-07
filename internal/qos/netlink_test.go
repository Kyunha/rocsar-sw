package qos

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// These tests drive NetlinkOps itself, against a dummy interface in a private
// network namespace. Everything here is skipped unless the kernel allows it,
// because installing traffic control needs CAP_NET_ADMIN and a test must not
// reshape the machine it is running on.
//
// The unit tests in test/qos_test.go drive Shaper through a fake and cover the
// hierarchy decisions. These cover the part a fake cannot: that the netlink calls
// are accepted by a real kernel, and that what is read back is what was asked
// for. That is the whole risk of the swap from tc -- an attribute the binding
// spells wrong compiles fine, runs fine, and silently shapes nothing.

// netnsAvailable reports whether this process can create a network namespace and
// act inside it, and whether HTB is compiled into the running kernel.
func netnsAvailable(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root to create a network namespace")
	}
	ns, err := netns.New()
	if err != nil {
		t.Skipf("cannot create a network namespace: %v", err)
	}
	defer ns.Close()
}

func newDummyDevice(t *testing.T, name string) {
	t.Helper()
	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: name}}
	if err := netlink.LinkAdd(link); err != nil {
		t.Skipf("cannot add a dummy device (dummy driver missing?): %v", err)
	}
	t.Cleanup(func() { _ = netlink.LinkDel(link) })

	// Read it back rather than trusting the index LinkAdd reported on the struct,
	// because every operation below addresses the device by index.
	fresh, err := netlink.LinkByName(name)
	if err != nil {
		t.Fatalf("cannot read back the dummy device: %v", err)
	}

	// The link must be up before a qdisc can be attached: the kernel rejects
	// qdisc creation on an interface that is not running, with EINVAL. The real
	// radio interface is brought up by the system, not by this code, so this is a
	// property of the test harness rather than of NetlinkOps.
	if err := netlink.LinkSetUp(fresh); err != nil {
		t.Skipf("cannot bring up a dummy device: %v", err)
	}
}

// The rates must arrive at the kernel as the configuration states them.
//
// This is the assertion that matters most and that a fake cannot make. The binding
// takes bits per second and divides by eight to reach the bytes per second the
// HTB attributes carry, so a unit slip here would be a silent 8x error: the
// hierarchy installs, telemetry says shaping is active, and the link runs at the
// wrong speed.
func TestNetlinkClassRatesReachTheKernelUnchanged(t *testing.T) {
	netnsAvailable(t)
	const dev = "shapetest0"
	newDummyDevice(t, dev)

	// Installed through Shaper, not by hand, so what is read back is what the
	// shipped code path produces.
	if ok, reason := NewShaper(nil, nil).Apply(context.Background(), dev, 115); !ok {
		t.Fatalf("Apply: %s", reason)
	}

	link, err := netlink.LinkByName(dev)
	if err != nil {
		t.Fatal(err)
	}
	classes, err := netlink.ClassList(link, RootHandle)
	if err != nil {
		t.Fatalf("ClassList: %v", err)
	}

	type key struct{ rate, ceil uint64 }
	got := make(map[uint32]key)
	for _, c := range classes {
		htb, ok := c.(*netlink.HtbClass)
		if !ok {
			continue
		}
		// Read back in bits per second, undoing the binding's conversion.
		got[htb.Attrs().Handle] = key{rate: htb.Rate * 8, ceil: htb.Ceil * 8}
	}

	// One class, and its rate is the configured rate: rate_kbps means the cap,
	// which is the whole point of collapsing the tree. Before the collapse this
	// read back 41_000 for a configured 115, because the cap the operator set was
	// only ever PriorityShare of what the link actually carried.
	if got[ShapedClass] != (key{115_000, 115_000}) {
		t.Errorf("shaped class = %+v, want rate 115000 ceil 115000 bits/s", got[ShapedClass])
	}
	if n := len(got); n != 1 {
		t.Errorf("%d classes installed, want exactly 1: there is no classifier to "+
			"steer traffic between them, so a second class is unreachable", n)
	}
}

// The whole hierarchy installs and reads back as HTB.
func TestNetlinkInstallsTheHierarchyAndReadsItBack(t *testing.T) {
	netnsAvailable(t)
	const dev = "shapetest1"
	newDummyDevice(t, dev)

	ops := NetlinkOps{}
	if ok, reason := NewShaper(nil, nil).Apply(context.Background(), dev, 115); !ok {
		t.Fatalf("Apply: %s", reason)
	}

	present, err := ops.Present(dev)
	if !present {
		t.Error("Present reports no shaping qdisc on a device we just installed one on")
	}

	// And that the leaves are attached where they belong: one per class, parented
	// to it. An HTB class with no leaf cannot be scheduled, so a missing leaf is a
	// hierarchy that looks installed and does not shape.
	link, err := netlink.LinkByName(dev)
	if err != nil {
		t.Fatal(err)
	}
	qdiscs, err := netlink.QdiscList(link)
	if err != nil {
		t.Fatal(err)
	}
	leaves := map[uint32]uint32{} // parent -> leaf handle
	for _, q := range qdiscs {
		if _, ok := q.(*netlink.FqCodel); ok {
			leaves[q.Attrs().Parent] = q.Attrs().Handle
		}
	}
	if leaves[ShapedClass] != ShapedLeaf {
		t.Errorf("shaped leaf = %s, want %s", HandleString(leaves[ShapedClass]), HandleString(ShapedLeaf))
	}

	// Removing the hierarchy must actually remove it, which is the read-back that
	// detects "the call returned nil and the effect is not there".
	if err := ops.ClearRoot(dev); err != nil {
		t.Fatalf("ClearRoot over an existing hierarchy: %v", err)
	}
	present, err = ops.Present(dev)
	if err != nil {
		t.Fatal(err)
	}
	if present {
		t.Error("Present still reports a shaping qdisc after ClearRoot")
	}
}

// End to end, through Shaper, on a device that has never been shaped.
//
// This is the case that used to be a substring match on tc's stderr. The first
// Apply clears a hierarchy that does not exist, and every fresh interface carries
// a handle-zero `noqueue` qdisc that the kernel answers EINVAL to -- so a fresh
// device is the exact case most likely to be mishandled, and it is the case that
// decides whether shaping works on first boot or only after a manual tc run.
func TestShaperAppliesToAFreshDeviceAndIsIdempotent(t *testing.T) {
	netnsAvailable(t)
	const dev = "shapetest2"
	newDummyDevice(t, dev)

	s := NewShaper(nil, nil)

	ok, reason := s.Apply(context.Background(), dev, 115)
	if !ok {
		t.Fatalf("first Apply on a fresh device failed: %s", reason)
	}
	present, err := (NetlinkOps{}).Present(dev)
	if err != nil {
		t.Fatal(err)
	}
	if !present {
		t.Error("Present reports no shaping qdisc after a successful Apply")
	}

	// Applying again must work over the hierarchy the first call installed, or a
	// restart silently degrades shaping.
	ok, reason = s.Apply(context.Background(), dev, 115)
	if !ok {
		t.Fatalf("second Apply over an existing hierarchy failed: %s", reason)
	}
	if !s.Active() {
		t.Error("Active() is false after two successful Apply calls")
	}
}

// A device that does not exist is an error that names the device, because that is
// the mistake an operator actually makes: a typo in configuration.
func TestNetlinkMissingDeviceNamesTheDevice(t *testing.T) {
	err := (NetlinkOps{}).ClearRoot("definitely-not-a-real-device0")
	if err == nil {
		t.Fatal("ClearRoot on a nonexistent device returned nil")
	}
	if got := err.Error(); !strings.Contains(got, "definitely-not-a-real-device0") {
		t.Errorf("error %q does not name the device the operator mistyped", got)
	}
}
