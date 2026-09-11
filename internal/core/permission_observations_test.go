package core

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPermissionObservationJournalIsBounded(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := PermissionObservationPath("subject", "runtime", "generation")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, MaxPermissionObservationJournalBytes), 0o600); err != nil {
		t.Fatal(err)
	}
	err := AppendPermissionObservation("subject", "runtime", "generation", PermissionObservation{RequestID: "request"})
	if !errors.Is(err, ErrPermissionObservationJournalFull) {
		t.Fatalf("append to full observation journal = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != MaxPermissionObservationJournalBytes {
		t.Fatalf("full journal changed: size=%d", info.Size())
	}
}
