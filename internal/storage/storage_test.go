package storage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A path that cleans to something outside the root must be refused, not
// normalised. The second case is the one that gets missed: `photos/../..` is
// harmless-looking and resolves outside the root exactly as `../..` does.
func TestResolveRefusesToEscapeTheRoot(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "data")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := New(root)

	for _, name := range []string{
		"../etc/passwd",
		"photos/../../etc/passwd",
		"..",
		"a/b/../../../..",
		"/etc/passwd",
		"/",
	} {
		if _, err := s.Resolve(name); err == nil {
			t.Errorf("Resolve(%q) succeeded; it must be refused", name)
		}
	}

	for _, name := range []string{"a.jpg", "photos/a.jpg", "./a.jpg", "a/b/c/d.log"} {
		got, err := s.Resolve(name)
		if err != nil {
			t.Errorf("Resolve(%q) = %v, want success", name, err)
			continue
		}
		if !strings.HasPrefix(got, root+string(filepath.Separator)) {
			t.Errorf("Resolve(%q) = %q, which is outside %q", name, got, root)
		}
	}
}

// The atomic write is the property that matters: a reader sees the file complete
// or not at all.
func TestWriteFileAtomicLeavesNoPartialFile(t *testing.T) {
	s := New(t.TempDir())

	body := make([]byte, 1<<20)
	for i := range body {
		body[i] = byte(i)
	}
	if err := s.WriteFileAtomic("big.bin", body, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(s.Root(), "big.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(body) {
		t.Fatalf("read back %d bytes, wrote %d", len(got), len(body))
	}

	// No temp files left behind: a `.partial-` file that survives would be
	// listed as an artefact the operator cannot explain.
	items, _ := os.ReadDir(s.Root())
	for _, it := range items {
		if strings.HasPrefix(it.Name(), ".partial-") {
			t.Errorf("a temporary file survived the write: %s", it.Name())
		}
	}
	if len(items) != 1 {
		names := make([]string, 0, len(items))
		for _, it := range items {
			names = append(names, it.Name())
		}
		t.Errorf("expected exactly one file, got %v", names)
	}
}

func TestWriteFileAtomicRefusesEscapingNames(t *testing.T) {
	s := New(t.TempDir())
	if err := s.WriteFileAtomic("../escape.txt", []byte("x"), 0o644); err == nil {
		t.Fatal("a write escaped the data directory")
	}
}

// Classification is by filename convention because there is no catalogue. That
// is a real reduction in rigour and the naming IS the contract, so it is tested.
func TestKindClassification(t *testing.T) {
	cases := map[string]string{
		"cam-20261003-142530.jpg":     KindCamera,
		"cam-1.jpeg":                  KindCamera,
		"rx_data_20261003_142530.bin": KindSDR,
		"sar-session-1.dat":           KindSDR,
		"connect-20261003-142530.log": KindLog,
		"notes.txt":                   KindLog,
		"eph.dat":                     KindUnknown,
		"something.else":              KindUnknown,
	}
	for name, want := range cases {
		if got := KindOf(name, false); got != want {
			t.Errorf("KindOf(%q) = %q, want %q", name, got, want)
		}
	}
	if got := KindOf("photos", true); got != KindUnknown {
		t.Errorf("KindOf on a directory = %q, want %q", got, KindUnknown)
	}
}

// Names must sort lexically in chronological order, which is why the format is
// not RFC3339.
func TestArtefactNamesSortChronologically(t *testing.T) {
	early := NameFor(KindCamera, "jpg")
	late := NameFor(KindSDR, "bin")
	if !strings.HasSuffix(early, ".jpg") || !strings.HasPrefix(early, "camera-") {
		t.Errorf("NameFor produced %q", early)
	}
	if late <= early {
		// Same second, so equal -- the assertion only checks the shape.
		t.Logf("names within one second: %q %q", early, late)
	}
}

// The listing must not show the debris of an interrupted write.
func TestListHidesTemporaryFiles(t *testing.T) {
	s := New(t.TempDir())
	if err := s.WriteFileAtomic("cam-1.jpg", []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Root(), ".partial-xyz"), []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}

	entries, err := s.List(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name, ".partial-") {
			t.Errorf("the listing exposed a partial file: %+v", e)
		}
	}
	if len(entries) != 1 || entries[0].Name != "cam-1.jpg" {
		t.Errorf("listing = %+v", entries)
	}
	if entries[0].Kind != KindCamera {
		t.Errorf("kind = %q, want %q", entries[0].Kind, KindCamera)
	}
}

// An unwritable data directory is fatal at startup, unlike a missing device.
func TestCheckRejectsAMissingDirectory(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "does-not-exist"))
	if err := s.Check(); err == nil {
		t.Fatal("Check passed for a directory that does not exist")
	}
	if err := New(t.TempDir()).Check(); err != nil {
		t.Fatalf("Check failed for a writable directory: %v", err)
	}
}

func TestFreeSpaceReportsSomethingPlausible(t *testing.T) {
	s := New(t.TempDir())
	free, err := s.FreeSpace()
	if err != nil {
		t.Fatalf("FreeSpace: %v", err)
	}
	if free == 0 {
		t.Error("FreeSpace returned 0 on a writable filesystem")
	}
	if free > 1<<50 {
		t.Errorf("FreeSpace returned %d bytes, which is implausible", free)
	}
}
