package harnessbin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// fakeSource "publishes" versions as shell scripts that print their version.
type fakeSource struct {
	kind    string
	latest  string
	fetched []string
	fail    error
}

func (f *fakeSource) DefaultChannel() string { return "stable" }
func (f *fakeSource) Latest(context.Context, string) (string, error) {
	if f.fail != nil {
		return "", f.fail
	}
	return f.latest, nil
}
func (f *fakeSource) Fetch(_ context.Context, version, dir string) error {
	if f.fail != nil {
		return f.fail
	}
	f.fetched = append(f.fetched, version)
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "bin", f.kind), []byte("#!/bin/sh\necho "+version+"\n"), 0o755)
}

func newTestManager(t *testing.T) (*Manager, *fakeSource) {
	t.Helper()
	src := &fakeSource{kind: "claude", latest: "1.0.0"}
	m := &Manager{
		Root:    t.TempDir(),
		Sources: map[string]Source{"claude": src, "codex": &fakeSource{kind: "codex", latest: "0.1.0"}},
		InUse:   func() ([]string, error) { return nil, nil },
		Verify:  func(context.Context, string) (string, error) { return "ok", nil },
	}
	return m, src
}

func mustInstall(t *testing.T, m *Manager, version string) {
	t.Helper()
	if _, err := m.Install(context.Background(), "claude", version, true); err != nil {
		t.Fatalf("Install(%s): %v", version, err)
	}
}

func TestInstallActivatesThroughAtomicCurrentLink(t *testing.T) {
	m, src := newTestManager(t)
	if got := m.Current("claude"); got != "" {
		t.Fatalf("fresh Current = %q", got)
	}
	v, err := m.Install(context.Background(), "claude", "", true) // default channel
	if err != nil || v != "1.0.0" {
		t.Fatalf("Install = %q, %v", v, err)
	}
	link, err := os.Readlink(filepath.Join(m.Root, "claude", "current"))
	if err != nil || link != "1.0.0" {
		t.Fatalf("current link = %q, %v; want relative 1.0.0", link, err)
	}
	p, ver, ok := m.ActiveBin("claude")
	if !ok || ver != "1.0.0" || p != filepath.Join(m.Root, "claude", "1.0.0", "bin", "claude") {
		t.Fatalf("ActiveBin = %q %q %v", p, ver, ok)
	}
	// Re-installing an installed version does not download again.
	mustInstall(t, m, "1.0.0")
	if !reflect.DeepEqual(src.fetched, []string{"1.0.0"}) {
		t.Fatalf("fetched = %v", src.fetched)
	}
	// No staging leftovers.
	entries, _ := os.ReadDir(filepath.Join(m.Root, "claude"))
	for _, e := range entries {
		if len(e.Name()) > 8 && e.Name()[:8] == ".staging" {
			t.Fatalf("staging dir left behind: %s", e.Name())
		}
	}
}

func TestVersionSwitchKeepsResolvedPathOfRunningLaunch(t *testing.T) {
	m, _ := newTestManager(t)
	mustInstall(t, m, "1.0.0")
	running, _, _ := m.ActiveBin("claude") // what a session launched with
	mustInstall(t, m, "1.1.0")
	if got := m.Current("claude"); got != "1.1.0" {
		t.Fatalf("Current = %q", got)
	}
	if st := m.LoadState("claude"); st.Previous != "1.0.0" {
		t.Fatalf("Previous = %q", st.Previous)
	}
	// The running session's resolved binary is still on disk and unchanged.
	if b, err := os.ReadFile(running); err != nil || string(b) != "#!/bin/sh\necho 1.0.0\n" {
		t.Fatalf("running binary = %q, %v", b, err)
	}
	from, to, err := m.Rollback("claude")
	if err != nil || from != "1.1.0" || to != "1.0.0" || m.Current("claude") != "1.0.0" {
		t.Fatalf("Rollback = %q→%q, %v (current %q)", from, to, err, m.Current("claude"))
	}
	if st := m.LoadState("claude"); st.Previous != "1.1.0" {
		t.Fatalf("after rollback Previous = %q, want 1.1.0 (rollback is reversible)", st.Previous)
	}
}

func TestInstallNoActivateLeavesCurrent(t *testing.T) {
	m, _ := newTestManager(t)
	mustInstall(t, m, "1.0.0")
	if _, err := m.Install(context.Background(), "claude", "2.0.0", false); err != nil {
		t.Fatal(err)
	}
	if m.Current("claude") != "1.0.0" || !reflect.DeepEqual(m.Installed("claude"), []string{"1.0.0", "2.0.0"}) {
		t.Fatalf("current %q installed %v", m.Current("claude"), m.Installed("claude"))
	}
}

func TestPinBlocksUpdateAndRollback(t *testing.T) {
	m, src := newTestManager(t)
	mustInstall(t, m, "1.0.0")
	if v, err := m.Pin(context.Background(), "claude", "1.0.0"); err != nil || v != "1.0.0" {
		t.Fatalf("Pin = %q, %v", v, err)
	}
	src.latest = "2.0.0"
	if _, _, err := m.Update(context.Background(), "claude", ""); !errors.Is(err, ErrPinned) {
		t.Fatalf("Update while pinned = %v, want ErrPinned", err)
	}
	if _, err := m.Install(context.Background(), "claude", "2.0.0", true); !errors.Is(err, ErrPinned) {
		t.Fatalf("activating Install while pinned = %v", err)
	}
	if _, _, err := m.Rollback("claude"); err == nil {
		t.Fatal("Rollback while pinned succeeded")
	}
	if err := m.Unpin("claude"); err != nil {
		t.Fatal(err)
	}
	from, to, err := m.Update(context.Background(), "claude", "")
	if err != nil || from != "1.0.0" || to != "2.0.0" {
		t.Fatalf("Update = %q→%q, %v", from, to, err)
	}
}

func TestGCKeepsCurrentPreviousPinnedAndRunning(t *testing.T) {
	m, _ := newTestManager(t)
	for _, v := range []string{"1.0.0", "1.1.0", "1.2.0", "1.3.0", "1.4.0"} {
		mustInstall(t, m, v)
	}
	// 1.4.0 current, 1.3.0 previous. Pin requires activation, so pin 1.1.0
	// then re-activate 1.4.0 by unpinning and re-pinning state manually.
	st := m.LoadState("claude")
	st.Pinned = "1.1.0"
	if err := m.saveState("claude", st); err != nil {
		t.Fatal(err)
	}
	// A live process runs 1.0.0's binary.
	m.InUse = func() ([]string, error) {
		return []string{"/usr/bin/zsh", m.BinPath("claude", "1.0.0")}, nil
	}
	removed, kept, err := m.GC("claude")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(removed, []string{"1.2.0"}) {
		t.Fatalf("removed = %v, want [1.2.0]", removed)
	}
	if !reflect.DeepEqual(m.Installed("claude"), []string{"1.0.0", "1.1.0", "1.3.0", "1.4.0"}) {
		t.Fatalf("installed after GC = %v (kept %v)", m.Installed("claude"), kept)
	}
	// Once the process exits, the next GC reclaims it.
	m.InUse = func() ([]string, error) { return nil, nil }
	if removed, _, _ := m.GC("claude"); !reflect.DeepEqual(removed, []string{"1.0.0"}) {
		t.Fatalf("second GC removed %v", removed)
	}
}

func TestGCRemovesNothingWhenProcessScanFails(t *testing.T) {
	m, _ := newTestManager(t)
	for _, v := range []string{"1.0.0", "1.1.0", "1.2.0"} {
		mustInstall(t, m, v)
	}
	m.InUse = func() ([]string, error) { return nil, errors.New("hidepid") }
	removed, _, err := m.GC("claude")
	if err == nil || len(removed) != 0 || len(m.Installed("claude")) != 3 {
		t.Fatalf("GC with failed scan removed %v, err %v", removed, err)
	}
}

func TestGCPrefixDoesNotConfuseSimilarVersions(t *testing.T) {
	m, _ := newTestManager(t)
	for _, v := range []string{"1.0.1", "1.0.10", "1.1.0", "1.2.0"} {
		mustInstall(t, m, v)
	}
	// Running 1.0.10 must not protect 1.0.1 (string-prefix bug guard).
	m.InUse = func() ([]string, error) { return []string{m.BinPath("claude", "1.0.10")}, nil }
	removed, _, err := m.GC("claude")
	if err != nil || !reflect.DeepEqual(removed, []string{"1.0.1"}) {
		t.Fatalf("removed %v, %v", removed, err)
	}
}

func TestUninstallFallsBackAndKeepsRunningVersion(t *testing.T) {
	m, _ := newTestManager(t)
	mustInstall(t, m, "1.0.0")
	mustInstall(t, m, "1.1.0")
	m.InUse = func() ([]string, error) { return []string{m.BinPath("claude", "1.1.0")}, nil }
	removed, kept, err := m.Uninstall("claude")
	if err != nil || !reflect.DeepEqual(removed, []string{"1.0.0"}) || !reflect.DeepEqual(kept, []string{"1.1.0"}) {
		t.Fatalf("Uninstall = %v %v %v", removed, kept, err)
	}
	if _, _, ok := m.ActiveBin("claude"); ok {
		t.Fatal("ActiveBin still reports a managed install after uninstall")
	}
}

func TestFailedFetchLeavesNoVersion(t *testing.T) {
	m, src := newTestManager(t)
	src.fail = errors.New("network down")
	if _, err := m.Install(context.Background(), "claude", "1.0.0", true); err == nil {
		t.Fatal("Install succeeded")
	}
	if len(m.Installed("claude")) != 0 || m.Current("claude") != "" {
		t.Fatalf("installed %v current %q", m.Installed("claude"), m.Current("claude"))
	}
}

func TestBrokenBinaryIsNotActivated(t *testing.T) {
	m, _ := newTestManager(t)
	m.Verify = func(context.Context, string) (string, error) { return "", errors.New("exec format error") }
	if _, err := m.Install(context.Background(), "claude", "1.0.0", true); err == nil {
		t.Fatal("Install of a binary that does not run succeeded")
	}
	if len(m.Installed("claude")) != 0 {
		t.Fatalf("installed %v", m.Installed("claude"))
	}
}

func TestRejectsUnsafeVersions(t *testing.T) {
	for _, v := range []string{"../x", "1.2", "current", "1.2.3/../../x", "", "1.2.3-..", "stable"} {
		if ValidVersion(v) {
			t.Errorf("ValidVersion(%q) = true", v)
		}
	}
	for _, v := range []string{"2.1.311", "0.169.0", "1.0.0-beta.1", "1.0.0+build"} {
		if !ValidVersion(v) {
			t.Errorf("ValidVersion(%q) = false", v)
		}
	}
	m, src := newTestManager(t)
	src.latest = "../../evil"
	if _, err := m.Install(context.Background(), "claude", "", true); err == nil {
		t.Fatal("a channel resolving to a traversal version was installed")
	}
	if _, err := m.Install(context.Background(), "hermes", "1.0.0", true); err == nil {
		t.Fatal("unsupported kind installed")
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"2.1.9", "2.1.10", -1}, {"2.1.10", "2.1.9", 1}, {"1.0.0", "1.0.0", 0},
		{"1.0.0-rc.1", "1.0.0", -1}, {"0.169.0", "0.99.0", 1},
	}
	for _, c := range cases {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("CompareVersions(%s,%s) = %d want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestManagedVersionRoot(t *testing.T) {
	root := "/data/amux/harnesses"
	cases := map[string]string{
		root + "/claude/2.1.0/bin/claude":       root + "/claude/2.1.0",
		root + "/codex/0.169.0/codex-path/rg":   root + "/codex/0.169.0",
		root + "/claude/current/bin/claude":     "",
		root + "/hermes/1.0.0/bin/hermes":       "",
		"/usr/bin/claude":                       "",
		root + "/claude/2.1.0":                  "",
		root + "/../harnesses/claude/2.1.0/bin": root + "/claude/2.1.0",
	}
	for p, want := range cases {
		got, ok := managedVersionRoot(root, p)
		if got != want || ok != (want != "") {
			t.Errorf("managedVersionRoot(%q) = %q, %v; want %q", p, got, ok, want)
		}
	}
}

func TestCheckNotifyAndAuto(t *testing.T) {
	m, src := newTestManager(t)
	ctx := context.Background()
	if res := m.Check(ctx, Settings{Update: UpdateNotify}); len(res) != 0 {
		t.Fatalf("Check touched an unmanaged harness: %+v", res)
	}
	mustInstall(t, m, "1.0.0")
	src.latest = "1.1.0"
	now := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	m.Now = func() time.Time { return now }

	if res := m.Check(ctx, Settings{Update: UpdateOff}); res != nil {
		t.Fatalf("off mode checked: %+v", res)
	}
	res := m.Check(ctx, Settings{Update: UpdateNotify})
	if len(res) != 1 || res[0].Updated || res[0].Available != "1.1.0" {
		t.Fatalf("notify = %+v", res)
	}
	if st := m.LoadState("claude"); st.Available != "1.1.0" || !st.CheckedAt.Equal(now) || m.Current("claude") != "1.0.0" {
		t.Fatalf("notify state %+v current %q", st, m.Current("claude"))
	}
	res = m.Check(ctx, Settings{Update: UpdateAuto})
	if len(res) != 1 || !res[0].Updated || m.Current("claude") != "1.1.0" {
		t.Fatalf("auto = %+v current %q", res, m.Current("claude"))
	}
	if st := m.LoadState("claude"); st.Available != "" || st.Previous != "1.0.0" {
		t.Fatalf("after auto state %+v", st)
	}
	// Pinned: auto degrades to notify.
	if _, err := m.Pin(ctx, "claude", "1.1.0"); err != nil {
		t.Fatal(err)
	}
	src.latest = "1.2.0"
	res = m.Check(ctx, Settings{Update: UpdateAuto})
	if len(res) != 1 || res[0].Updated || res[0].Err != nil || m.Current("claude") != "1.1.0" {
		t.Fatalf("pinned auto = %+v current %q", res, m.Current("claude"))
	}
}

func TestSettingsDefaultsAndValidation(t *testing.T) {
	s, err := parseSettings([]byte(`{"update":"auto","check_interval":"10m","claude":{"bin":"/x/claude","channel":"latest"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if s.UpdateMode() != UpdateAuto || s.Interval() != time.Hour || s.Kind("claude").Bin != "/x/claude" || s.Kind("codex").Bin != "" {
		t.Fatalf("settings %+v interval %v", s, s.Interval())
	}
	s, _ = parseSettings(nil)
	if s.UpdateMode() != UpdateOff || s.Interval() != DefaultCheckInterval {
		t.Fatalf("defaults %+v", s)
	}
	if s, _ := parseSettings([]byte(`{"update":"bogus"}`)); s.UpdateMode() != UpdateOff {
		t.Fatal("invalid mode not treated as off")
	}
}

func TestSetGetRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	if err := Set(KeyUpdate, "sometimes"); err == nil {
		t.Fatal("invalid update mode accepted")
	}
	if err := Set(KeyInterval, "5m"); err == nil {
		t.Fatal("sub-hour interval accepted")
	}
	for key, val := range map[string]string{KeyUpdate: "notify", KeyInterval: "12h", "harnesses.codex.bin": "/opt/codex", "harnesses.claude.channel": "latest"} {
		if err := Set(key, val); err != nil {
			t.Fatalf("Set(%s): %v", key, err)
		}
		if got, err := Get(key); err != nil || got != val {
			t.Fatalf("Get(%s) = %q, %v", key, got, err)
		}
	}
	s, err := LoadSettings()
	if err != nil || s.UpdateMode() != UpdateNotify || s.Interval() != 12*time.Hour || s.Kind("codex").Bin != "/opt/codex" {
		t.Fatalf("LoadSettings = %+v, %v", s, err)
	}
	if err := Set("harnesses.codex.bin", ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := Get("harnesses.codex.bin"); got != "" {
		t.Fatalf("unset left %q", got)
	}
	if err := Set("harnesses.hermes.bin", "/x"); err == nil {
		t.Fatal("unknown key accepted")
	}
}
