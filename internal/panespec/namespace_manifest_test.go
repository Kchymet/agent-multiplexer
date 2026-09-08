package panespec

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"amux/internal/core"
	"amux/internal/store"
)

func argvSequence(argv []string, sequence ...string) int {
	for i := 0; i+len(sequence) <= len(argv); i++ {
		if slices.Equal(argv[i:i+len(sequence)], sequence) {
			return i
		}
	}
	return -1
}

func TestOwnOnlyManifestMountsTypedAccessAndFreshPIDProc(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	useFakeSecureBwrap(t)
	s := store.Session{ID: "agent-a", RootID: "root", Agent: "codex", Dir: filepath.Join(home, "sessions", "old-root", "agent-a")}
	spec := testLaunchSpec(t, s)
	argv, err := scope(s.Dir, TabAgent, s, spec.Access, []string{"/usr/bin/true"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"--die-with-parent", "--unshare-user", "--unshare-pid"} {
		if !slices.Contains(argv, flag) {
			t.Errorf("manifest missing %s: %v", flag, argv)
		}
	}
	if argvSequence(argv, "--proc", "/proc") < 0 {
		t.Fatalf("manifest lacks private procfs: %v", argv)
	}
	want := [][]string{
		{"--bind", s.Dir, s.Dir},
		{"--ro-bind", spec.Access.CredentialHostDir, core.SessionAccessDir()},
		{"--ro-bind", spec.Access.MailboxHostDir, spec.Access.MailboxMountDir},
		{"--bind", spec.Access.RequestsHostDir, spec.Access.RequestsMountDir},
	}
	last := -1
	for _, sequence := range want {
		at := argvSequence(argv, sequence...)
		if at < 0 {
			t.Errorf("manifest missing %v: %v", sequence, argv)
		}
		if at <= last {
			t.Errorf("mount order is unsafe at %v: %v", sequence, argv)
		}
		last = at
	}
	if argvSequence(argv, "--ro-bind", spec.Access.CredentialHostDir, spec.Access.CredentialMountDir) >= 0 {
		t.Fatalf("manifest mounts credentials below the read-only mailbox: %v", argv)
	}
	for _, forbidden := range []string{
		core.DataDir(), core.StateDir(), core.HookStateDir(), core.TranscriptDir(),
		filepath.Dir(spec.Access.MailboxHostDir), "/run",
	} {
		if slices.Contains(argv, forbidden) {
			t.Errorf("manifest exposes forbidden broad path %q: %v", forbidden, argv)
		}
	}
	if argvSequence(argv, "--chdir", s.Dir) < last {
		t.Errorf("chdir must follow all authority mounts: %v", argv)
	}
}

func TestTypedLaunchPathsStripInheritedHostAuthority(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("AMUX_CODEX_BIN", "/bin/true")
	useFakeSecureBwrap(t)
	secrets := []string{
		"AMUX_MUX_TOKEN", "AMUX_PROVIDER_TOKEN", "AMUX_PROVIDER_PASSWORD",
		"AMUX_TLS_KEY", "AMUX_TLS_KEY_PASSWORD", "AMUX_HOST_PRIVATE_KEY",
		"AMUX_RPC_DIR", "OPENAI_API_KEY",
	}
	for _, name := range secrets {
		t.Setenv(name, "planted-host-only")
	}
	s := store.Session{ID: "subject", Agent: "codex", Dir: filepath.Join(home, "subject")}
	spec := testLaunchSpec(t, s)
	_, _, agentArgv, err := Resolve(spec, TabAgent)
	if err != nil {
		t.Fatal(err)
	}
	_, _, serverArgv, _, err := AppServerCommand(spec)
	if err != nil {
		t.Fatal(err)
	}
	_, _, attachArgv, err := AttachCommand(spec, "unix:///own/cx.sock", "thread")
	if err != nil {
		t.Fatal(err)
	}
	for label, argv := range map[string][]string{"agent": agentArgv, "app-server": serverArgv, "attach": attachArgv} {
		separator := slices.Index(argv, "--")
		if separator < 0 || separator+2 >= len(argv) || argv[separator+2] != payloadExecArg {
			t.Errorf("%s does not pass through the descriptor-clean payload trampoline: %v", label, argv)
		}
		if argvSequence(argv, "--setenv", payloadExecEnv, "1") < 0 {
			t.Errorf("%s does not activate the descriptor-clean payload trampoline: %v", label, argv)
		}
		for _, name := range secrets {
			if argvSequence(argv, "--unsetenv", name) < 0 {
				t.Errorf("%s did not strip %s: %v", label, name, argv)
			}
		}
		if strings.Contains(strings.Join(argv, "\x00"), "planted-host-only") {
			t.Errorf("%s serialized a host credential value: %v", label, argv)
		}
	}
}

func TestLaunchSpecRejectsPlantedDestinationAliasesAndDeniedParents(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	useFakeSecureBwrap(t)
	outside := filepath.Join(home, "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	canary := filepath.Join(outside, "canary")
	if err := os.WriteFile(canary, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("symlink", func(t *testing.T) {
		s := store.Session{ID: "alias", Agent: "codex", Dir: filepath.Join(home, "alias")}
		spec := testLaunchSpec(t, s)
		if err := os.Symlink(outside, filepath.Join(s.Dir, ".amux")); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := Resolve(spec, TabTerminal); err == nil || !strings.Contains(err.Error(), "not a real directory") {
			t.Fatalf("planted .amux alias error = %v", err)
		}
	})

	t.Run("permission-denied", func(t *testing.T) {
		s := store.Session{ID: "denied", Agent: "codex", Dir: filepath.Join(home, "denied")}
		spec := testLaunchSpec(t, s)
		amuxDir := filepath.Join(s.Dir, ".amux")
		if err := os.MkdirAll(filepath.Join(amuxDir, "control"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(amuxDir, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(amuxDir, 0o700) })
		if _, _, _, err := Resolve(spec, TabTerminal); err == nil {
			t.Fatal("permission denial was treated as an authoritative host path")
		}
	})

	if got, err := os.ReadFile(canary); err != nil || string(got) != "unchanged" {
		t.Fatalf("refused launch changed outside canary: %q, %v", got, err)
	}
}

func TestLaunchSpecRejectsForgedSubjectsSourcesAndTargets(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	useFakeSecureBwrap(t)
	s := store.Session{ID: "subject", Agent: "codex", Dir: filepath.Join(home, "subject")}
	base := testLaunchSpec(t, s)
	cases := map[string]func(*LaunchSpec){
		"subject": func(spec *LaunchSpec) { spec.Access.SubjectID = "peer" },
		"mailbox-target": func(spec *LaunchSpec) {
			spec.Access.MailboxMountDir = filepath.Join(home, "peer", ".amux", "control")
		},
		"requests-target": func(spec *LaunchSpec) { spec.Access.RequestsMountDir = filepath.Join(s.Dir, "requests") },
		"credential-target": func(spec *LaunchSpec) {
			spec.Access.CredentialMountDir = filepath.Join(s.Dir, ".amux", "peer-credential")
		},
		"requests-source": func(spec *LaunchSpec) { spec.Access.RequestsHostDir = t.TempDir() },
		"credential-overlap": func(spec *LaunchSpec) {
			spec.Access.CredentialHostDir = filepath.Join(spec.Access.MailboxHostDir, "responses")
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			spec := base
			mutate(&spec)
			if _, _, _, err := Resolve(spec, TabTerminal); !errors.Is(err, ErrAccessRequired) {
				t.Fatalf("forged LaunchSpec error = %v, want ErrAccessRequired", err)
			}
		})
	}
}

func TestIsolationSupportRejectsUnsafeBubblewrapAndDisabledJail(t *testing.T) {
	for _, tc := range []struct {
		version string
		wantErr bool
	}{
		{"bubblewrap 0.11.2\n", true},
		{"bubblewrap 0.12.0\n", false},
		{"bubblewrap 1.0.0\n", false},
		{"unexpected\n", true},
	} {
		t.Run(strings.TrimSpace(tc.version), func(t *testing.T) {
			dir := t.TempDir()
			binary := filepath.Join(dir, "bwrap")
			script := "#!/bin/sh\nprintf '%s' '" + strings.TrimSpace(tc.version) + "'\n"
			if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			err := requireBubblewrapVersion(binary)
			if (err != nil) != tc.wantErr {
				t.Fatalf("requireBubblewrapVersion error = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil && !errors.Is(err, ErrIsolationUnsupported) {
				t.Fatalf("error does not identify unsupported isolation: %v", err)
			}
		})
	}
}
