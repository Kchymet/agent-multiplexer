package selfupdate_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"amux/internal/selfupdate"
	"amux/internal/selfupdate/releasetest"
)

// fakeBinary is the content of a "release binary"; the probe reads the version
// back out of it instead of executing it.
func fakeBinary(version string) []byte { return []byte("amux-binary:" + version) }

func fakeProbe(_ context.Context, path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(string(b), "amux-binary:"), nil
}

func newUpdater(t *testing.T, repo *releasetest.Repo, running string) *selfupdate.Updater {
	t.Helper()
	dir := t.TempDir()
	target := filepath.Join(dir, "bin", "amux")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, fakeBinary(running), 0o755); err != nil {
		t.Fatal(err)
	}
	return &selfupdate.Updater{
		Source:   repo.Source(),
		Store:    selfupdate.Store{Dir: filepath.Join(dir, "state")},
		Running:  selfupdate.Manifest{Version: running, DaemonProtocol: 2, DatabaseMaxSchema: 3},
		Target:   target,
		ShimPath: filepath.Join(dir, "config", "amux.sh"),
		GOOS:     "linux",
		GOARCH:   "arm64",
		Probe:    fakeProbe,
	}
}

func release(v string) selfupdate.Manifest {
	return selfupdate.Manifest{Version: v, DaemonProtocol: 2, DatabaseMaxSchema: 3}
}

func TestFetchVerifyApplyEndToEnd(t *testing.T) {
	repo := releasetest.New(t)
	repo.Publish(release("0.1.301"), map[string][]byte{
		"amux-linux-arm64": fakeBinary("0.1.301"),
		"amux-linux-amd64": fakeBinary("wrong-platform"),
		"amux.sh":          []byte("# shim v301\n"),
	}, nil)
	u := newUpdater(t, repo, "0.1.300")
	m, err := u.Source.Latest(t.Context())
	if err != nil || m.Version != "0.1.301" {
		t.Fatalf("Latest = %+v, %v", m, err)
	}
	staged, err := u.Fetch(t.Context(), m)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(u.Target); string(b) != string(fakeBinary("0.1.300")) {
		t.Fatal("Fetch must not touch the target")
	}
	var st selfupdate.State
	if err := u.Apply(&st, staged); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(u.Target); string(b) != string(fakeBinary("0.1.301")) {
		t.Fatalf("target = %q", b)
	}
	if b, _ := os.ReadFile(u.ShimPath); string(b) != "# shim v301\n" {
		t.Fatalf("shim = %q", b)
	}
	if st.Installed != "0.1.301" || st.Path != u.Target {
		t.Fatalf("state = %+v", st)
	}
	if entries, _ := os.ReadDir(u.Store.StagingDir()); len(entries) != 0 {
		t.Fatalf("staging not cleaned: %v", entries)
	}
	for _, p := range repo.Requests {
		if strings.Contains(p, "/latest/") && !strings.HasSuffix(p, selfupdate.ManifestAsset) {
			t.Fatalf("asset %s read through latest instead of the manifest's tag", p)
		}
	}
}

func TestFetchRejectsChecksumMismatch(t *testing.T) {
	repo := releasetest.New(t)
	repo.Publish(release("0.1.301"), map[string][]byte{
		"amux-linux-arm64": fakeBinary("0.1.301"),
	}, map[string]string{"amux-linux-arm64": strings.Repeat("ab", 32)})
	u := newUpdater(t, repo, "0.1.300")
	_, err := u.Fetch(t.Context(), release("0.1.301"))
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("Fetch = %v, want checksum mismatch", err)
	}
	entries, _ := os.ReadDir(u.Store.StagingDir())
	if len(entries) != 0 {
		t.Fatalf("unverified download left in staging: %v", entries)
	}
}

func TestFetchRejectsMissingPlatformAndWrongVersion(t *testing.T) {
	repo := releasetest.New(t)
	repo.Publish(release("0.1.301"), map[string][]byte{"amux-darwin-arm64": fakeBinary("0.1.301")}, nil)
	u := newUpdater(t, repo, "0.1.300")
	if _, err := u.Fetch(t.Context(), release("0.1.301")); err == nil || !strings.Contains(err.Error(), "amux-linux-arm64") {
		t.Fatalf("missing platform: %v", err)
	}
	repo.Publish(release("0.1.302"), map[string][]byte{"amux-linux-arm64": fakeBinary("0.1.299")}, nil)
	if _, err := u.Fetch(t.Context(), release("0.1.302")); err == nil || !strings.Contains(err.Error(), "reports version") {
		t.Fatalf("version mismatch: %v", err)
	}
}

func TestApplyPendingStaged(t *testing.T) {
	repo := releasetest.New(t)
	next := selfupdate.Manifest{Version: "0.1.310", DaemonProtocol: 3, DatabaseMaxSchema: 3}
	repo.Publish(next, map[string][]byte{"amux-linux-arm64": fakeBinary("0.1.310")}, nil)
	u := newUpdater(t, repo, "0.1.300")
	staged, err := u.Fetch(t.Context(), next)
	if err != nil {
		t.Fatal(err)
	}
	if err := u.Store.Save(selfupdate.State{Staged: staged}); err != nil {
		t.Fatal(err)
	}
	v, err := u.ApplyPendingStaged()
	if err != nil || v != "0.1.310" {
		t.Fatalf("ApplyPendingStaged = %q, %v", v, err)
	}
	if b, _ := os.ReadFile(u.Target); string(b) != string(fakeBinary("0.1.310")) {
		t.Fatalf("target = %q", b)
	}
	st, _ := u.Store.Load()
	if st.Staged != nil || st.Installed != "0.1.310" {
		t.Fatalf("state = %+v", st)
	}
	if v, err := u.ApplyPendingStaged(); err != nil || v != "" {
		t.Fatalf("second apply = %q, %v", v, err)
	}
}

func TestApplyPendingStagedNeverOverwritesALaterDevBuild(t *testing.T) {
	repo := releasetest.New(t)
	next := selfupdate.Manifest{Version: "0.1.310", DaemonProtocol: 3, DatabaseMaxSchema: 3}
	repo.Publish(next, map[string][]byte{"amux-linux-arm64": fakeBinary("0.1.310")}, nil)
	u := newUpdater(t, repo, "0.1.300")
	staged, err := u.Fetch(t.Context(), next)
	if err != nil {
		t.Fatal(err)
	}
	if err := u.Store.Save(selfupdate.State{Staged: staged}); err != nil {
		t.Fatal(err)
	}
	// The user installs a checkout build after the release was staged.
	dev := fakeBinary("0.1.305-dev+abc")
	if err := os.WriteFile(u.Target, dev, 0o755); err != nil {
		t.Fatal(err)
	}
	if v, err := u.ApplyPendingStaged(); err != nil || v != "" {
		t.Fatalf("ApplyPendingStaged = %q, %v; want the stale stage dropped", v, err)
	}
	if b, _ := os.ReadFile(u.Target); string(b) != string(dev) {
		t.Fatalf("dev build overwritten: %q", b)
	}
	if st, _ := u.Store.Load(); st.Staged != nil {
		t.Fatalf("stale stage kept: %+v", st.Staged)
	}

	// A stage the user forced over a dev build is applied.
	staged, err = u.Fetch(t.Context(), next)
	if err != nil {
		t.Fatal(err)
	}
	staged.Forced = true
	if err := u.Store.Save(selfupdate.State{Staged: staged}); err != nil {
		t.Fatal(err)
	}
	if v, err := u.ApplyPendingStaged(); err != nil || v != "0.1.310" {
		t.Fatalf("forced stage = %q, %v", v, err)
	}
}

func TestLatestWithoutReleases(t *testing.T) {
	repo := releasetest.New(t)
	_, err := repo.Source().Latest(t.Context())
	if err == nil || !strings.Contains(err.Error(), "no published release") {
		t.Fatalf("Latest on empty repo = %v", err)
	}
}
