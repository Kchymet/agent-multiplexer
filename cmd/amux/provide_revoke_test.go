package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"amux/internal/providercfg"
	"amux/internal/wiretls"
)

func TestProvideRevokeIsHostOnly(t *testing.T) {
	old := sessionContextRestricted
	sessionContextRestricted = func() bool { return true }
	t.Cleanup(func() { sessionContextRestricted = old })
	if err := cmdProvide([]string{"revoke", "--yes"}); err == nil || !strings.Contains(err.Error(), "host-only") {
		t.Fatalf("restricted revoke = %v", err)
	}
}

func TestProvideRevokeRequiresConfirmationBeforeHostState(t *testing.T) {
	for _, args := range [][]string{nil, {"--yes=false"}, {"--allow-compute", "--yes"}, {"--token", "secret", "--yes"}} {
		_, err := parseProvideRevoke(args, func() (providercfg.Config, error) {
			t.Fatal("unconfirmed or invalid request read host configuration")
			return providercfg.Config{}, nil
		})
		if err == nil {
			t.Fatalf("accepted %q", args)
		}
	}
}

func TestProvideRevokeTransportSettings(t *testing.T) {
	dir := t.TempDir()
	saved, override := filepath.Join(dir, "saved.token"), filepath.Join(dir, "override.token")
	for path, token := range map[string]string{saved: "saved-secret\n", override: "override-secret\n"} {
		if err := os.WriteFile(path, []byte(token), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("AMUX_PROVIDER_TOKEN", "environment-secret")
	t.Setenv(wiretls.EnvCA, "environment-ca")
	t.Setenv(wiretls.EnvServer, "environment-server")
	base := providercfg.Config{Orchestrator: "saved:7443", TokenFile: saved, CAFile: "saved-ca", ServerName: "saved-server", AllowCompute: true, PublishSessions: true}
	load := func() (providercfg.Config, error) { return base, nil }
	cfg, err := parseProvideRevoke([]string{"--yes"}, load)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Orchestrator != base.Orchestrator || cfg.Token != "saved-secret" || cfg.CAFile != "environment-ca" || cfg.ServerName != "environment-server" || cfg.AllowCompute || cfg.PublishSessions {
		t.Fatal("saved transport settings or capability isolation incorrect")
	}
	cfg, err = parseProvideRevoke([]string{"override:7443", "--yes", "--token-file", override, "--ca", "flag-ca", "--server-name", "flag-server"}, load)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Orchestrator != "override:7443" || cfg.Token != "override-secret" || cfg.CAFile != "flag-ca" || cfg.ServerName != "flag-server" {
		t.Fatal("explicit transport overrides incorrect")
	}
	base.TokenFile = ""
	cfg, err = parseProvideRevoke([]string{"--yes"}, load)
	if err != nil || cfg.Token != "environment-secret" {
		t.Fatal("environment credential fallback incorrect")
	}
	// A selected but broken token file must not revoke an environment token's
	// different registration instead.
	for _, path := range []string{filepath.Join(dir, "missing"), override} {
		if path == override {
			if err := os.WriteFile(path, []byte(" \n"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		base.TokenFile = path
		if _, err := parseProvideRevoke([]string{"--yes"}, load); err == nil {
			t.Fatal("unusable selected token file fell back to environment credential")
		}
	}
	// Never rewrite or delete the credential as a side effect of parsing.
	b, err := os.ReadFile(saved)
	if err != nil || string(b) != "saved-secret\n" {
		t.Fatal("saved credential changed")
	}
}

func TestProvideRevokeRejectsUnusableConfig(t *testing.T) {
	t.Setenv("AMUX_PROVIDER_TOKEN", "")
	for _, tc := range []struct {
		name string
		file providercfg.Config
		err  error
	}{
		{"malformed", providercfg.Config{}, errors.New("malformed configuration")},
		{"missing endpoint", providercfg.Config{}, nil},
		{"missing token", providercfg.Config{Orchestrator: "orch:7443"}, nil},
		{"unreadable token", providercfg.Config{Orchestrator: "orch:7443", TokenFile: filepath.Join(t.TempDir(), "absent")}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseProvideRevoke([]string{"--yes"}, func() (providercfg.Config, error) { return tc.file, tc.err })
			if err == nil || !strings.Contains(err.Error(), "provide revoke") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}
