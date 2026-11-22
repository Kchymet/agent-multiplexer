package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"amux/internal/buildinfo"
)

// Updater downloads, verifies and installs releases for one target binary.
type Updater struct {
	Source  Source
	Store   Store
	Running Manifest // the running build's own contracts
	Target  string   // binary to replace (resolved executable)
	// ShimPath is where the shell shim lives (config dir/amux.sh); empty skips it.
	ShimPath string
	GOOS     string
	GOARCH   string
	// Probe runs a binary's `version --short`; ProbeVersion by default.
	Probe func(ctx context.Context, binary string) (string, error)
}

func (u *Updater) platform() (string, string) {
	goos, goarch := u.GOOS, u.GOARCH
	if goos == "" {
		goos = runtime.GOOS
	}
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	return goos, goarch
}

func (u *Updater) probe(ctx context.Context, binary string) (string, error) {
	if u.Probe != nil {
		return u.Probe(ctx, binary)
	}
	return ProbeVersion(ctx, binary)
}

// InstalledVersion reports the version of the binary currently at Target.
func (u *Updater) InstalledVersion(ctx context.Context) (string, error) {
	return u.probe(ctx, u.Target)
}

// Fetch downloads the release binary for this platform (and the shell shim)
// into the staging directory, verifies both against the release's SHA256SUMS,
// and runs the binary to confirm it executes here and reports the manifest's
// version. Nothing outside the staging directory changes.
func (u *Updater) Fetch(ctx context.Context, m Manifest) (*Staged, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	sums, err := u.Source.Sums(ctx, m.Version)
	if err != nil {
		return nil, err
	}
	goos, goarch := u.platform()
	asset := BinaryAsset(goos, goarch)
	want, ok := sums[asset]
	if !ok {
		return nil, fmt.Errorf("release %s has no %s asset for this platform", m.Version, asset)
	}
	dir := u.Store.StagingDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	final := filepath.Join(dir, fmt.Sprintf("amux-%s-%s-%s", m.Version, goos, goarch))
	if err := u.download(ctx, m.Version, asset, final, want); err != nil {
		return nil, err
	}
	got, err := u.probe(ctx, final)
	if err != nil {
		_ = os.Remove(final)
		return nil, fmt.Errorf("downloaded %s does not run on this host: %w", asset, err)
	}
	if got != m.Version {
		_ = os.Remove(final)
		return nil, fmt.Errorf("downloaded %s reports version %s, release manifest says %s", asset, got, m.Version)
	}
	st := &Staged{Manifest: m, File: final, SHA256: want, Target: u.Target}
	if wantShim, ok := sums[ShimAsset]; ok && u.ShimPath != "" {
		b, err := u.Source.Asset(ctx, m.Version, ShimAsset)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(b)
		if err := VerifySHA256(ShimAsset, hex.EncodeToString(sum[:]), wantShim); err != nil {
			return nil, err
		}
		shim := filepath.Join(dir, fmt.Sprintf("amux.sh-%s", m.Version))
		if err := WriteFileAtomic(shim, b, 0o644); err != nil {
			return nil, err
		}
		st.Shim = shim
	}
	return st, nil
}

func (u *Updater) download(ctx context.Context, version, asset, final, want string) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(final), ".download-*")
	if err != nil {
		return err
	}
	defer func() {
		_ = tmp.Close()
		if err != nil {
			_ = os.Remove(tmp.Name())
		}
	}()
	h := sha256.New()
	if err = u.Source.Download(ctx, version, asset, io.MultiWriter(tmp, h)); err != nil {
		return err
	}
	if err = VerifySHA256(asset, hex.EncodeToString(h.Sum(nil)), want); err != nil {
		return err
	}
	if err = tmp.Chmod(0o755); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), final)
}

// Apply installs a staged release over its target (re-verifying the checksum
// over the bytes written), refreshes the shell shim, and records the install in
// st. The caller holds the store lock and saves st.
func (u *Updater) Apply(st *State, staged *Staged) error {
	if staged.Target != u.Target {
		return fmt.Errorf("staged update targets %s, not %s", staged.Target, u.Target)
	}
	if err := ReplaceFile(u.Target, staged.File, 0o755, staged.SHA256); err != nil {
		return err
	}
	if staged.Shim != "" && u.ShimPath != "" {
		if b, err := os.ReadFile(staged.Shim); err == nil {
			_ = WriteFileAtomic(u.ShimPath, b, 0o644)
		}
	}
	st.Installed, st.Path = staged.Version, u.Target
	if st.Staged != nil && st.Staged.Version == staged.Version {
		st.Staged = nil
	}
	u.cleanStaging(st)
	return nil
}

// cleanStaging removes staged files no longer referenced by st.
func (u *Updater) cleanStaging(st *State) {
	entries, err := os.ReadDir(u.Store.StagingDir())
	if err != nil {
		return
	}
	keep := map[string]bool{}
	if st.Staged != nil {
		keep[st.Staged.File], keep[st.Staged.Shim] = true, true
	}
	for _, e := range entries {
		p := filepath.Join(u.Store.StagingDir(), e.Name())
		if !keep[p] && !strings.HasPrefix(e.Name(), ".download-") {
			_ = os.Remove(p)
		}
	}
}

// ApplyPendingStaged installs a staged release for this target, if any, under
// the store lock. It is the "next restart" path: `amux daemon restart`/`start`
// call it between stopping and starting the daemon, when no old daemon is left
// running that the new binary's protocol could break. It returns the version
// installed, or "".
func (u *Updater) ApplyPendingStaged() (string, error) {
	unlock, err := u.Store.Lock(30 * time.Second)
	if err != nil {
		return "", err
	}
	defer unlock()
	st, err := u.Store.Load()
	if err != nil || st.Staged == nil || st.Staged.Target != u.Target {
		return "", err
	}
	staged := *st.Staged
	drop := ""
	if installed, err := u.InstalledVersion(context.Background()); err != nil {
		drop = fmt.Sprintf("cannot identify the installed binary (%v)", err)
	} else if !Newer(staged.Version, installed) {
		drop = fmt.Sprintf("%s is already installed", installed)
	} else if !buildinfo.IsRelease(installed) && !staged.Forced {
		// A checkout build was installed after the release was staged; the
		// user chose it, so the stale stage must not overwrite it.
		drop = fmt.Sprintf("development build %s was installed since", installed)
	}
	if drop != "" {
		st.Staged, st.Pending = nil, ""
		u.cleanStaging(&st)
		return "", u.Store.Save(st)
	}
	if err := u.Apply(&st, &staged); err != nil {
		st.LastError = err.Error()
		_ = u.Store.Save(st)
		return "", err
	}
	st.Pending, st.LastError = "", ""
	return staged.Version, u.Store.Save(st)
}
