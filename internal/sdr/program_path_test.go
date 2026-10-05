package sdr

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The acquisition program is installed on PATH rather than shipped beside the
// OBC binary, so a binary scp'd onto the Pi can drive a program that was built
// and installed separately. These tests pin the two things that change with it:
// where the binary is found, and -- the part that can bite -- that the program
// and the OBC still read the SAME params.json.

// A program on PATH is found, and the program directory is not required to
// contain an executable at all. This is the deployed shape: the binary lives in
// /usr/local/bin, the parameters live under /root.
func TestProgramIsFoundOnPath(t *testing.T) {
	dir := t.TempDir()
	prog := filepath.Join(dir, "sdr-ettus-b200mini")
	if err := os.MkdirAll(filepath.Join(prog, "parameters"), 0o755); err != nil {
		t.Fatal(err)
	}
	// No `connect` file anywhere under prog. Only PATH can satisfy this.
	binDir := t.TempDir()
	bin := filepath.Join(binDir, "connect")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)

	got, err := LookProgram(prog)
	if err != nil {
		t.Fatalf("LookProgram: %v", err)
	}
	if got != bin {
		t.Errorf("LookProgram = %q, want %q -- the PATH copy should win", got, bin)
	}
}

// PATH first, but a checkout with the vendored tree must keep working with
// nothing installed. Without the fallback, `go run ./cmd/obc --mock-...` on a
// fresh clone could not start an acquisition at all.
func TestProgramFallsBackToTheProgramDirectory(t *testing.T) {
	dir := t.TempDir()
	prog := filepath.Join(dir, "sdr-ettus-b200mini")
	if err := os.MkdirAll(prog, 0o755); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(prog, "connect")
	if err := os.WriteFile(local, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// An empty PATH: nothing to find there.
	t.Setenv("PATH", t.TempDir())

	got, err := LookProgram(prog)
	if err != nil {
		t.Fatalf("LookProgram: %v", err)
	}
	if got != local {
		t.Errorf("LookProgram = %q, want the fallback %q", got, local)
	}
}

// Neither on PATH nor beside the parameters is a named failure, not a start
// that dies with exit status 127 in a log the operator has to go and read.
func TestNoProgramAnywhereIsRefused(t *testing.T) {
	dir := t.TempDir()
	prog := filepath.Join(dir, "sdr-ettus-b200mini")
	if err := os.MkdirAll(prog, 0o755); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(prog, "connect")
	t.Setenv("PATH", t.TempDir())

	_, err := LookProgram(prog)
	if err == nil {
		t.Fatal("LookProgram succeeded with no binary on PATH and none in the program directory")
	}
	// Both places must be named: "not found" alone sends the operator looking in
	// one of the two.
	if !strings.Contains(err.Error(), "PATH") || !strings.Contains(err.Error(), local) {
		t.Errorf("the error does not name both places it looked: %v", err)
	}
}

// THE load-bearing one.
//
// connect.cpp resolves "./../sdr-ettus-b200mini/parameters/params.json" against
// its working directory, which is the program directory. That only reaches
// <programDir>/parameters/params.json when the directory is named
// sdr-ettus-b200mini. Point sdr.program anywhere else and the OBC edits one file
// while the program reads another: SetParams reports success, the GUI shows the
// new PRF, and the capture uses the old one.
func TestProgramAndServiceAgreeOnTheParamsFile(t *testing.T) {
	dir := t.TempDir()
	prog := filepath.Join(dir, "sdr-ettus-b200mini")
	if err := os.MkdirAll(filepath.Join(prog, "parameters"), 0o755); err != nil {
		t.Fatal(err)
	}

	s := NewService(prog, dir, discardLogger())
	got, err := s.programParamsPath()
	if err != nil {
		t.Fatalf("a correctly named program directory was rejected: %v", err)
	}
	if want := s.ParamsPath(); got != filepath.Clean(want) {
		t.Errorf("programParamsPath = %q, want the file the service edits %q", got, want)
	}
	if err := s.CheckProgramConfig(); err != nil {
		t.Errorf("CheckProgramConfig: %v", err)
	}
}

// A directory not named sdr-ettus-b200mini must be refused up front, with both
// paths in the message. This is the deployment mistake the PATH change makes
// reachable: decoupling the binary from the directory is the whole point, and
// it removes the accident that used to keep the name right.
func TestAMisnamedProgramDirectoryIsRefused(t *testing.T) {
	dir := t.TempDir()
	// The obvious wrong choice: a tidy name that says nothing about the C++.
	prog := filepath.Join(dir, "rocsar-sdr")
	if err := os.MkdirAll(filepath.Join(prog, "parameters"), 0o755); err != nil {
		t.Fatal(err)
	}

	s := NewService(prog, dir, discardLogger())
	err := s.CheckProgramConfig()
	if err == nil {
		t.Fatal("a program directory the C++ cannot resolve was accepted; " +
			"parameter edits would report success and change nothing")
	}
	// The message has to say which file each side would use, or it is not
	// actionable.
	for _, want := range []string{
		filepath.Join(dir, "sdr-ettus-b200mini", "parameters", "params.json"),
		filepath.Join(prog, "parameters", "params.json"),
		programConfigRelPath,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q:\n%v", want, err)
		}
	}
}

// A symlinked program directory is a legitimate deployment -- /opt/rocsar ->
// /root/rocsar-rpi/sdr-ettus-b200mini -- and must not be refused for having a
// different string path. Compared as strings it would differ; compared as files
// it is the same one, which is what matters.
func TestASymlinkedProgramDirectoryIsAccepted(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "sdr-ettus-b200mini")
	if err := os.MkdirAll(filepath.Join(real, "parameters"), 0o755); err != nil {
		t.Fatal(err)
	}
	params := filepath.Join(real, "parameters", "params.json")
	if err := os.WriteFile(params, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "current")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	// Through the link the C++ reaches the real directory's file; through the
	// link the service reaches it too, and SameFile settles it.
	s := NewService(link, dir, discardLogger())
	if _, err := s.programParamsPath(); err != nil {
		t.Errorf("a symlinked program directory was refused: %v", err)
	}
}

// Connect must refuse the misnamed directory BEFORE creating a log file or
// spawning anything. A refused start that still leaves a connect-*.log behind
// looks like it half-worked.
func TestConnectRefusesAMisnamedDirectoryWithoutStarting(t *testing.T) {
	dir := t.TempDir()
	logRoot := t.TempDir()
	prog := filepath.Join(dir, "rocsar-sdr")
	if err := os.MkdirAll(filepath.Join(prog, "parameters"), 0o755); err != nil {
		t.Fatal(err)
	}

	// connect IS available, so the binary lookup succeeds and the only thing
	// left to refuse this start is the params.json disagreement. Without it this
	// test would pass for the wrong reason.
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "connect"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)

	started := false
	s := NewService(prog, logRoot, discardLogger())
	s.start = func(context.Context, string, string, []string, *os.File) (int, error) {
		started = true
		return 0, nil
	}

	if err := s.Connect(context.Background()); err == nil {
		t.Fatal("Connect started a program whose params.json it cannot reach")
	}
	if started {
		t.Error("Connect spawned the program before checking the params path")
	}
	entries, err := os.ReadDir(filepath.Join(logRoot, "sdr"))
	if err == nil && len(entries) > 0 {
		t.Errorf("a refused Connect left %d log file(s) behind", len(entries))
	}
}
