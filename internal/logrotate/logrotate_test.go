package logrotate

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// openLogFD opens path for append on a fresh descriptor number, standing in
// for the daemon's stderr, and returns a writer bound to that number.
func openLogFD(t *testing.T, path string) int {
	t.Helper()
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_WRONLY|syscall.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Close(fd) })
	return fd
}

func writeFD(t *testing.T, fd int, s string) {
	t.Helper()
	if _, err := syscall.Write(fd, []byte(s)); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRotateRedirectsDescriptorAndKeepsGenerations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	fd := openLogFD(t, path)
	p := Policy{Path: path, MaxBytes: 10, Keep: 2, FDs: []int{fd}}

	writeFD(t, fd, "short\n")
	if rotated, err := RotateIfLarge(p); err != nil || rotated {
		t.Fatalf("under the limit: rotated=%v err=%v", rotated, err)
	}

	for gen := 1; gen <= 3; gen++ {
		writeFD(t, fd, strings.Repeat("x", 10)+"\n")
		rotated, err := RotateIfLarge(p)
		if err != nil || !rotated {
			t.Fatalf("gen %d: rotated=%v err=%v", gen, rotated, err)
		}
		// The same descriptor number now writes to the fresh live file.
		writeFD(t, fd, "after\n")
		if got := read(t, path); got != "after\n" {
			t.Fatalf("gen %d: live log = %q, want only post-rotation writes", gen, got)
		}
		if got := read(t, path+".1"); !strings.HasSuffix(got, strings.Repeat("x", 10)+"\n") {
			t.Fatalf("gen %d: .1 = %q", gen, got)
		}
	}
	if _, err := os.Stat(path + ".2"); err != nil {
		t.Fatalf(".2 missing: %v", err)
	}
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Fatalf("Keep=2 left a third generation: %v", err)
	}
}

func TestRotateIgnoresLogNotBehindOurDescriptors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 100)), 0o644); err != nil {
		t.Fatal(err)
	}
	// The daemon's stderr is a terminal (here: some other file), not the log.
	other := openLogFD(t, filepath.Join(dir, "tty"))
	rotated, err := RotateIfLarge(Policy{Path: path, MaxBytes: 10, Keep: 3, FDs: []int{other}})
	if err != nil || rotated {
		t.Fatalf("rotated=%v err=%v; a log we do not write must be left alone", rotated, err)
	}
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Fatalf("unexpected rotation: %v", err)
	}
}
