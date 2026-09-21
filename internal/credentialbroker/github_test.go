package credentialbroker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestGitHubScopeAndFreshHostSelection(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GH_CONFIG_DIR", dir)
	for _, key := range []string{"GH_HOST", "GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"} {
		t.Setenv(key, "")
	}
	path := filepath.Join(dir, "hosts.yml")
	write := func(value string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("github.com:\n  user: selected\nenterprise.example:\n  user: selected\n")
	o := Operation{Verb: Read, Service: "gh:github.com", Account: GitHubAccount}
	if !GitHubAllowed(o) {
		t.Fatal("active configured host denied")
	}
	for _, mutate := range []func(*Operation){
		func(o *Operation) { o.Account = "other-user" },
		func(o *Operation) { o.Verb = Write; o.Value = "token" },
		func(o *Operation) { o.Verb = Delete },
		func(o *Operation) { o.Service = "gh:unconfigured.example" },
		func(o *Operation) { o.Service = "gh:--hostname" },
		func(o *Operation) { o.Service = "gh:github.com\n" },
		func(o *Operation) { o.Service = "gh:https://github.com" },
		func(o *Operation) { o.Service = "gh:github.com:443" },
		func(o *Operation) { o.Service = "gh:../keychain" },
	} {
		bad := o
		mutate(&bad)
		if GitHubAllowed(bad) {
			t.Fatal("unauthorized operation allowed")
		}
	}
	write("enterprise.example:\n  user: selected\n")
	if GitHubAllowed(o) {
		t.Fatal("removed host remained authorized")
	}
	t.Setenv("GH_ENTERPRISE_TOKEN", "synthetic")
	o.Service = "gh:unknown.example"
	if GitHubAllowed(o) {
		t.Fatal("ambient enterprise token admitted an unselected host")
	}
	t.Setenv("GH_HOST", "unknown.example")
	if !GitHubAllowed(o) {
		t.Fatal("daemon-selected env-only host denied")
	}
}

func TestGitHubBackendUsesActiveNativeToken(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GH_CONFIG_DIR", dir)
	t.Setenv("PATH", dir)
	if err := os.WriteFile(filepath.Join(dir, "hosts.yml"), []byte("github.com:\n  user: selected\n"), 0600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n[ \"$*\" = 'auth token --hostname github.com' ] || exit 9\nprintf '%s\\n' \"$SYNTHETIC_TOKEN\"\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	// macOS may validate a newly created executable on its first launch. Do
	// that outside the broker's short production deadline, especially when
	// the full suite is concurrently compiling native sandbox fixtures.
	_ = exec.Command(filepath.Join(dir, "gh")).Run()
	o := Operation{Verb: Read, Service: "gh:github.com", Account: GitHubAccount}
	for _, token := range []string{"initial", "rotated"} {
		result, err := (GitHub{Env: []string{"SYNTHETIC_TOKEN=" + token}}).Execute(context.Background(), o)
		if err != nil || result.ExitCode != 0 || result.Value != token {
			t.Fatalf("did not read current active native credential: %v", err)
		}
	}
	o.Account = "other"
	if _, err := (GitHub{}).Execute(context.Background(), o); err == nil {
		t.Fatal("native backend accepted account override")
	}
}
