package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"amux/internal/access"
)

func TestCheckProtectedHostCredentialDistinguishesExpiry(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	a, err := access.OpenWithClock(t.TempDir(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	hostDir, err := a.EnsureHost(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sessionDir, err := a.Ensure(context.Background(), access.SubjectSession, "s1")
	if err != nil {
		t.Fatal(err)
	}

	if err := checkProtectedHostCredential(t.TempDir(), now); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing credential error = %v, want %v", err, os.ErrNotExist)
	}
	if err := checkProtectedHostCredential(sessionDir, now); !errors.Is(err, access.ErrInvalidCredential) {
		t.Fatalf("non-host credential error = %v, want %v", err, access.ErrInvalidCredential)
	}
	if err := checkProtectedHostCredential(hostDir, now); err != nil {
		t.Fatalf("valid host credential error = %v", err)
	}
	expired := now.Add(31 * 24 * time.Hour)
	err = checkProtectedHostCredential(hostDir, expired)
	if !errors.Is(err, access.ErrExpired) {
		t.Fatalf("expired host credential error = %v, want %v", err, access.ErrExpired)
	}
	if errors.Is(err, access.ErrInvalidCredential) || !strings.Contains(err.Error(), "expired at") {
		t.Fatalf("expired host credential error %q does not name expiry", err)
	}
}
