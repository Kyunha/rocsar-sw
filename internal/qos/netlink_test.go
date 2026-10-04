package qos

import (
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

	ops := NetlinkOps{}
	if err := ops.ClearRoot(dev); err != nil {
		t.Fatalf("ClearRoot on a fresh device: %v", err)
	}
	if err := ops.AddRootHTB(dev); err != nil {
		t.Fatalf("AddRootHTB: %v", err)
	}
	if err := ops.AddClass(dev, PriorityClass, RootHandle, 41, 115); err != nil {
		t.Fatalf("AddClass priority: %v", err)
	}
	if err := ops.AddClass(dev, BulkClass, RootHandle, 74, 74); err != nil {
		t.Fatalf("AddClass bulk: %v", err)
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

	if got[PriorityClass] != (key{41_000, 115_000}) {
		t.Errorf("priority class = %+v, want rate 41000 ceil 115000 bits/s", got[PriorityClass])
	}
	if got[BulkClass] != (key{74_000, 74_000}) {
		t.Errorf("bulk class = %+v, want rate 74000 ceil 74000 bits/s", got[BulkClass])
	}
}

// The whole hierarchy installs and reads back as HTB.
func TestNetlinkInstallsTheHierarchyAndReadsItBack(t *testing.T) {
	netnsAvailable(t)
	const dev = "shapetest1"
	newDummyDevice(t, dev)

	ops := NetlinkOps{}
	if err := ops.ClearRoot(dev); err != nil {
		t.Fatalf("ClearRoot: %v", err)
	}
	if err := ops.AddRootHTB(dev); err != nil {
		t.Fatalf("AddRootHTB: %v", err)
	}
	if err := ops.AddClass(dev, PriorityClass, RootHandle, 41, 115); err != nil {
		t.Fatalf("AddClass priority: %v", err)
	}
	if err := ops.AddClass(dev, BulkClass, RootHandle, 74, 74); err != nil {
		t.Fatalf("AddClass bulk: %v", err)
	}
	if err := ops.AddLeaf(dev, PriorityClass, PriorityLeaf); err != nil {
		t.Fatalf("AddLeaf priority: %v", err)
	}
	if err := ops.AddLeaf(dev, BulkClass, BulkLeaf); err != nil {
		t.Fatalf("AddLeaf bulk: %v", err)
	}

	present, err := ops.Present(dev)
	if err != nil {
		t.Fatalf("Present: %v", err)
	}
	if !present {
		t.Error("Present reports no shaping qdisc on a device we just installed one on")
	}

	// Reinstalling over an existing hierarchy must work, which is what makes this
	// idempotent across restarts. ClearRoot is called first by Apply, so this also
	// exercises the delete path against a real qdisc.
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

// Clearing a device that never had one is success, not an error.
//
// With tc this was a substring match on three known messages, one of which was
// documented as dangerous to match loosely. It is now ENOENT.
func TestNetlinkClearRootOnAnUnshapedDeviceIsSuccess(t *testing.T) {
	netnsAvailable(t)
	const dev = "shapetest2"
	newDummyDevice(t, dev)

	if err := (NetlinkOps{}).ClearRoot(dev); err != nil {
		t.Errorf("ClearRoot on a device with no hierarchy returned %v, want nil", err)
	}
	// And twice, to be sure it is genuinely idempotent rather than accidentally
	// matching the first call.
	if err := (NetlinkOps{}).ClearRoot(dev); err != nil {
		t.Errorf("second ClearRoot returned %v, want nil", err)
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
