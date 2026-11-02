package provider

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestLockStatusIsSingleWriter: a second provider on the same status file is
// refused with the holder's pid (flock is per open file description, so a
// second open in one process contends exactly like a second process), and the
// file is free again once the holder releases it.
func TestLockStatusIsSingleWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "provider-status.json")
	release, err := LockStatus(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LockStatus(path); !errors.Is(err, ErrStatusHeld) || !strings.Contains(err.Error(), "pid "+strconv.Itoa(os.Getpid())) {
		t.Fatalf("second LockStatus = %v, want ErrStatusHeld naming pid %d", err, os.Getpid())
	}
	release()
	release2, err := LockStatus(path)
	if err != nil {
		t.Fatalf("LockStatus after release = %v", err)
	}
	release2()
}
