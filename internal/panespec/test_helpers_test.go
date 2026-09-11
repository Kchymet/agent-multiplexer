package panespec

import (
	"os"
	"path/filepath"
	"testing"

	"amux/internal/access"
	"amux/internal/store"
)

func requireRuntimeIsolation(t *testing.T) {
	t.Helper()
	if err := IsolationSupport(); err != nil {
		if os.Getenv("AMUX_REQUIRE_NAMESPACE_TEST") == "1" {
			t.Fatalf("required protected namespace unavailable: %v", err)
		}
		t.Skipf("protected namespace unavailable: %v", err)
	}
}

func testLaunchSpec(t *testing.T, s store.Session) LaunchSpec {
	t.Helper()
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	mailbox := filepath.Join(root, "mailbox")
	requests := filepath.Join(mailbox, "requests")
	responses := filepath.Join(mailbox, "responses")
	credential := filepath.Join(root, "credential")
	for _, dir := range []string{mailbox, requests, responses, credential} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	mount := filepath.Join(s.Dir, ".amux", access.MailboxDirName)
	return LaunchSpec{Session: s, Access: access.SessionAccess{
		SubjectID:      s.ID,
		MailboxHostDir: mailbox, MailboxMountDir: mount,
		RequestsHostDir: requests, RequestsMountDir: filepath.Join(mount, "requests"),
		CredentialHostDir: credential, CredentialMountDir: filepath.Join(mount, access.CredentialDirName),
	}}
}

func useFakeSecureBwrap(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "bwrap")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nif [ \"$1\" = --version ]; then echo 'bubblewrap 0.12.0'; exit 0; fi\nexit 99\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AMUX_JAIL", "on")
	return path
}
