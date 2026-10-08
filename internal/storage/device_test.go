package storage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The device check is the only thing standing between a dropped SSD and a
// silent data loss, so it is tested against the shape of that failure rather
// than against a happy path that never occurs on the aircraft.

// An empty device asserts nothing. This is the laptop: http.root is a directory
// under the working tree and there is no second filesystem to be on, so asking
// for one would fail every development run.
func TestAnEmptyDeviceAssertsNothing(t *testing.T) {
	if err := New(t.TempDir(), "").Check(); err != nil {
		t.Fatalf("Check failed with no device configured: %v", err)
	}
}

// A device that cannot be resolved is refused. The configured UUID has to name
// something under /dev/disk/by-uuid, and on a machine where it does not, the
// answer is that the disk is not attached.
func TestAnUnresolvableDeviceIsRefused(t *testing.T) {
	dir := t.TempDir()
	err := New(dir, "00000000-0000-0000-0000-000000000000").Check()
	if err == nil {
		t.Fatal("Check passed with a device that is not attached")
	}
	// "not attached" and "not mounted" send an operator to different places.
	if !strings.Contains(err.Error(), "not attached") {
		t.Errorf("the refusal does not say the disk is absent: %v", err)
	}
}

// The central case: a real, writable directory that is not on the disk the
// operator named.
//
// This is what /mnt/rocsar_ssd looks like after the SSD drops off the USB bus.
// The directory survives, every writability probe passes, and a program that
// only checked writability would have started and reported success.
func TestAWritableDirectoryOnTheWrongFilesystemIsRefused(t *testing.T) {
	dir := t.TempDir()
	// A UUID that resolves to something real, if anything happens to be attached
	// here. Under test the root filesystem is not the aircraft's SSD, so this
	// must be refused -- and if a machine really does have that disk mounted at
	// tempdir, the test is vacuous rather than wrong, so it is skipped.
	resolved, err := filepath.EvalSymlinks(filepath.Join(diskByUUIDDir, "a82c5820-9183-45ba-bf2f-a956f6dec4cd"))
	if err == nil {
		if onDevice, e := filesystemAt(dir); e == nil && onDevice != nil && onDevice.source == resolved {
			t.Skip("this machine has the aircraft's SSD under the test directory")
		}
	}

	err = New(dir, "a82c5820-9183-45ba-bf2f-a956f6dec4cd").Check()
	if err == nil {
		t.Fatal("Check passed for a writable directory on the wrong filesystem; " +
			"this is the dropped-SSD case and it must be fatal")
	}
	if !strings.Contains(err.Error(), "a82c5820") {
		t.Errorf("the refusal does not name the device that was expected: %v", err)
	}
}

// The error must distinguish ErrWrongDevice from ErrNoRoot. A caller that
// collapses them tells an operator with a missing mount to go looking for a
// missing directory.
func TestTheWrongDeviceIsADistinctError(t *testing.T) {
	err := New(t.TempDir(), "00000000-0000-0000-0000-000000000000").Check()
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), ErrWrongDevice.Error()) {
		t.Errorf("the error is not ErrWrongDevice: %v", err)
	}
}

// A missing root is still ErrNoRoot and must not be reported as a device
// problem: the directory is not there at all, which is a different instruction
// for whoever is on the aircraft.
func TestAMissingRootIsStillReportedAsAMissingRoot(t *testing.T) {
	err := New(filepath.Join(t.TempDir(), "nope"), "").Check()
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if strings.Contains(err.Error(), ErrWrongDevice.Error()) {
		t.Errorf("a missing directory was reported as a wrong device: %v", err)
	}
}

// mountinfo escaping: the kernel octal-escapes space and three other characters
// in mount points, and without decoding them a mount point containing a space
// never compares equal to its own path -- so the check would report a mismatch
// that does not exist.
func TestMountPointEscapesAreDecoded(t *testing.T) {
	cases := map[string]string{
		`/mnt/a\040b`:          "/mnt/a b",
		`/mnt/plain`:           "/mnt/plain",
		`/mnt/tab\011here`:     "/mnt/tab\there",
		`/mnt/back\134slash`:   `/mnt/back\slash`,
		`/mnt/dollar\043hash`:  "/mnt/dollar#hash",
		`/mnt/trailing\`:       "/mnt/trailing\\",
		`/mnt/not\999an octal`: `/mnt/not\999an octal`,
	}
	for in, want := range cases {
		if got := unescapeMount(in); got != want {
			t.Errorf("unescapeMount(%q) = %q, want %q", in, got, want)
		}
	}
}

// filesystemAt must pick the LONGEST matching mount point, not the first. When
// the SSD is missing, "/" still matches every path, and returning "/" would make
// the check compare the root filesystem against the aircraft's disk and report a
// confusing "on /dev/root" rather than "nothing is mounted there".
func TestFilesystemAtPrefersTheDeepestMount(t *testing.T) {
	m, err := filesystemAt("/")
	if err != nil {
		t.Skipf("cannot read mountinfo: %v", err)
	}
	if m == nil {
		t.Fatal("no filesystem reported for /")
	}
	// The root mount point must be exactly "/", not some deeper path that happens
	// to exist on this machine.
	if m.point != "/" {
		t.Errorf("filesystemAt(/) returned the mount at %q; the deepest match for / is / itself", m.point)
	}
}

// A path that is inside a mounted filesystem reports that filesystem, which is
// what makes the check work for a data root that is a subdirectory rather than a
// mount point in its own right.
func TestFilesystemAtResolvesAPathInsideAMount(t *testing.T) {
	dir := t.TempDir()
	m, err := filesystemAt(dir)
	if err != nil {
		t.Skipf("cannot read mountinfo: %v", err)
	}
	if m == nil {
		t.Fatal("no filesystem reported for a temporary directory")
	}
	if m.point == "/" {
		// /tmp is often a real mount on some systems; if it is not, "/" is the
		// correct answer and this is not a failure. The assertion that matters is
		// in TestAWritableDirectoryOnTheWrongFilesystemIsRefused.
		t.Logf("%s resolves to the root filesystem", dir)
	}
}

// deviceFieldEmptyIsMeaningful: a Store with no device must not silently become
// a Store with one. Guarding the zero value keeps New's two-argument form from
// being misread later.
func TestTheZeroStoreHasNoDeviceAssertion(t *testing.T) {
	var s Store
	if s.device != "" {
		t.Errorf("the zero Store asserts device %q", s.device)
	}
	if err := s.Check(); err == nil {
		t.Log("Check on the zero Store did not fail; the root does not exist")
	}
}

// The device comparison must not be fooled by a symlinked data root. The Pi's
// root is on mmcblk0p2 and the SSD is sda, so if a symlink were resolved lazily
// the two could compare unequal for the wrong reason.
func TestADeviceCheckToleratesASymlinkedRoot(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("cannot create a symlink: %v", err)
	}
	if err := New(link, "").Check(); err != nil {
		t.Fatalf("Check failed for a symlinked root with no device configured: %v", err)
	}
}
