package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"amux/internal/core"
	"amux/internal/selfupdate"
	"amux/internal/selfupdate/releasetest"
)

type updateFixture struct {
	repo     *releasetest.Repo
	target   string
	store    selfupdate.Store
	running  string
	report   versionReport
	restarts int
	out      strings.Builder
}

func newUpdateFixture(t *testing.T, running string) *updateFixture {
	t.Helper()
	dir := t.TempDir()
	f := &updateFixture{repo: releasetest.New(t), running: running}
	f.target = filepath.Join(dir, "amux")
	if err := os.WriteFile(f.target, []byte("bin:"+running), 0o755); err != nil {
		t.Fatal(err)
	}
	f.store = selfupdate.Store{Dir: filepath.Join(dir, "state")}
	f.report = versionReport{CLI: running, ConnectErr: syscall.ENOENT}
	return f
}

func (f *updateFixture) publish(v string, protocol int) {
	f.repo.Publish(selfupdate.Manifest{Version: v, DaemonProtocol: protocol, DatabaseMaxSchema: 3},
		map[string][]byte{selfupdate.BinaryAsset("linux", "amd64"): []byte("bin:" + v)}, nil)
}

func (f *updateFixture) daemonUp(live int, protocol int) {
	f.report = versionReport{CLI: f.running, Connected: true, Runtime: core.VersionInfo{
		DaemonVersion: f.running, DaemonProtocol: protocol, DatabaseMaxSchema: 3, LiveAgents: &live,
	}}
}

func (f *updateFixture) deps() updateDeps {
	return updateDeps{
		newUpdater: func() (*selfupdate.Updater, error) {
			return &selfupdate.Updater{
				Source:  f.repo.Source(),
				Store:   f.store,
				Running: selfupdate.Manifest{Version: f.running, DaemonProtocol: 2, DatabaseMaxSchema: 3},
				Target:  f.target,
				GOOS:    "linux", GOARCH: "amd64",
				Probe: func(_ context.Context, p string) (string, error) {
					b, err := os.ReadFile(p)
					return strings.TrimPrefix(string(b), "bin:"), err
				},
			}, nil
		},
		versions:      func() versionReport { return f.report },
		restartDaemon: func() error { f.restarts++; return nil },
		out:           func(format string, a ...any) { fmt.Fprintf(&f.out, format, a...) },
	}
}

func (f *updateFixture) onDisk(t *testing.T) string {
	t.Helper()
	b, _ := os.ReadFile(f.target)
	return strings.TrimPrefix(string(b), "bin:")
}

func TestUpdateCheckReportsWithoutInstalling(t *testing.T) {
	f := newUpdateFixture(t, "0.1.300")
	f.publish("0.1.301", 2)
	if err := runUpdate(t.Context(), updateFlags{check: true}, f.deps()); err != nil {
		t.Fatal(err)
	}
	if f.onDisk(t) != "0.1.300" || !strings.Contains(f.out.String(), "update available") {
		t.Fatalf("on disk %s, out:\n%s", f.onDisk(t), f.out.String())
	}
}

func TestUpdateRefusesDevBuildWithoutForce(t *testing.T) {
	f := newUpdateFixture(t, "0.1.300-dev+abc")
	f.publish("0.1.301", 2)
	err := runUpdate(t.Context(), updateFlags{}, f.deps())
	if err == nil || !strings.Contains(err.Error(), "--force") || f.onDisk(t) != "0.1.300-dev+abc" {
		t.Fatalf("err = %v, on disk %s", err, f.onDisk(t))
	}
	if err := runUpdate(t.Context(), updateFlags{force: true}, f.deps()); err != nil {
		t.Fatal(err)
	}
	if f.onDisk(t) != "0.1.301" {
		t.Fatalf("--force did not install: %s", f.onDisk(t))
	}
}

func TestUpdateUpToDate(t *testing.T) {
	f := newUpdateFixture(t, "0.1.301")
	f.publish("0.1.301", 2)
	if err := runUpdate(t.Context(), updateFlags{}, f.deps()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.out.String(), "up to date") {
		t.Fatalf("out:\n%s", f.out.String())
	}
}

func TestUpdateRestartsIdleDaemon(t *testing.T) {
	f := newUpdateFixture(t, "0.1.300")
	f.publish("0.1.301", 2)
	f.daemonUp(0, 2)
	if err := runUpdate(t.Context(), updateFlags{}, f.deps()); err != nil {
		t.Fatal(err)
	}
	if f.onDisk(t) != "0.1.301" || f.restarts != 1 {
		t.Fatalf("on disk %s, restarts %d", f.onDisk(t), f.restarts)
	}
}

func TestUpdateNeverRestartsDaemonWithAgentsUnlessAsked(t *testing.T) {
	f := newUpdateFixture(t, "0.1.300")
	f.publish("0.1.301", 2)
	f.daemonUp(2, 2)
	if err := runUpdate(t.Context(), updateFlags{}, f.deps()); err != nil {
		t.Fatal(err)
	}
	if f.onDisk(t) != "0.1.301" || f.restarts != 0 {
		t.Fatalf("compatible update: on disk %s, restarts %d", f.onDisk(t), f.restarts)
	}
	if !strings.Contains(f.out.String(), "2 agents are running") {
		t.Fatalf("out:\n%s", f.out.String())
	}

	f = newUpdateFixture(t, "0.1.300")
	f.publish("0.1.301", 2)
	f.daemonUp(2, 2)
	if err := runUpdate(t.Context(), updateFlags{restartDaemon: true}, f.deps()); err != nil {
		t.Fatal(err)
	}
	if f.restarts != 1 {
		t.Fatalf("--restart-daemon did not restart (restarts %d)", f.restarts)
	}
}

func TestUpdateStagesProtocolChangeWhileAgentsRun(t *testing.T) {
	f := newUpdateFixture(t, "0.1.300")
	f.publish("0.1.302", 3)
	f.daemonUp(1, 2)
	if err := runUpdate(t.Context(), updateFlags{}, f.deps()); err != nil {
		t.Fatal(err)
	}
	if f.onDisk(t) != "0.1.300" || f.restarts != 0 {
		t.Fatalf("protocol change swapped under a busy daemon: on disk %s, restarts %d", f.onDisk(t), f.restarts)
	}
	st, _ := f.store.Load()
	if st.Staged == nil || st.Staged.Version != "0.1.302" {
		t.Fatalf("not staged: %+v", st)
	}
}

func TestUpdateRefusesIncompatibleDatabase(t *testing.T) {
	f := newUpdateFixture(t, "0.1.300")
	f.repo.Publish(selfupdate.Manifest{Version: "0.1.301", DaemonProtocol: 2, DatabaseMaxSchema: 2},
		map[string][]byte{selfupdate.BinaryAsset("linux", "amd64"): []byte("bin:0.1.301")}, nil)
	err := runUpdate(t.Context(), updateFlags{}, f.deps())
	if err == nil || !strings.Contains(err.Error(), "schema") || f.onDisk(t) != "0.1.300" {
		t.Fatalf("err = %v, on disk %s", err, f.onDisk(t))
	}
}

func TestVersionShortAndManifest(t *testing.T) {
	out, err := captureOutput(t, func() error { return cmdVersionArgs([]string{"--short"}) })
	if err != nil || strings.Count(strings.TrimSpace(out), "\n") != 0 || strings.TrimSpace(out) == "" {
		t.Fatalf("--short = %q, %v", out, err)
	}
	out, err = captureOutput(t, func() error { return cmdVersionArgs([]string{"--manifest"}) })
	if err != nil || !strings.Contains(out, `"daemonProtocol"`) || !strings.Contains(out, `"databaseMaxSchema"`) {
		t.Fatalf("--manifest = %q, %v", out, err)
	}
}
