package harnessbin

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Source is one harness's official distribution channel.
type Source interface {
	// DefaultChannel is the channel install/update use when none is given.
	DefaultChannel() string
	// Latest resolves a channel name to a concrete version.
	Latest(ctx context.Context, channel string) (string, error)
	// Fetch downloads, verifies and unpacks version into dir so that
	// dir/bin/<kind> is the launch executable.
	Fetch(ctx context.Context, version, dir string) error
}

// DefaultSources are the official native distributions: Claude Code's release
// bucket (the one https://claude.ai/install.sh uses) and Codex's GitHub release
// packages (the ones https://github.com/openai/codex install.sh uses).
func DefaultSources() map[string]Source {
	return map[string]Source{
		"claude": &ClaudeSource{BaseURL: ClaudeReleasesURL},
		"codex":  &CodexSource{BaseURL: CodexReleasesURL},
	}
}

// ClaudeReleasesURL is Claude Code's native release root: <root>/{stable,latest}
// name a version; <root>/<version>/manifest.json lists per-platform SHA-256s;
// <root>/<version>/<platform>/claude is the self-contained binary.
const ClaudeReleasesURL = "https://downloads.claude.ai/claude-code-releases"

// CodexReleasesURL is Codex's GitHub release root.
const CodexReleasesURL = "https://github.com/openai/codex/releases"

var httpClient = &http.Client{Timeout: 30 * time.Minute}

const userAgent = "amux-harness-installer"

func get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return resp, nil
}

func getSmall(ctx context.Context, url string) ([]byte, error) {
	resp, err := get(ctx, url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}

// download streams url into path, returning its SHA-256 and size.
func download(ctx context.Context, url, path string) (string, int64, error) {
	resp, err := get(ctx, url)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", 0, fmt.Errorf("download %s: %w", url, err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// ClaudeSource installs Claude Code's native build.
type ClaudeSource struct {
	BaseURL  string
	Platform string // override for tests; "" ⇒ detected
}

func (*ClaudeSource) DefaultChannel() string { return "stable" }

func (s *ClaudeSource) Latest(ctx context.Context, channel string) (string, error) {
	if channel != "stable" && channel != "latest" {
		return "", fmt.Errorf("unknown Claude Code channel %q (want stable, latest, or a version)", channel)
	}
	b, err := getSmall(ctx, s.BaseURL+"/"+channel)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func (s *ClaudeSource) platform() (string, error) {
	if s.Platform != "" {
		return s.Platform, nil
	}
	arch := map[string]string{"amd64": "x64", "arm64": "arm64"}[runtime.GOARCH]
	if arch == "" {
		return "", fmt.Errorf("unsupported architecture %s", runtime.GOARCH)
	}
	switch runtime.GOOS {
	case "darwin":
		return "darwin-" + arch, nil
	case "linux":
		if muslHost() {
			return "linux-" + arch + "-musl", nil
		}
		return "linux-" + arch, nil
	}
	return "", fmt.Errorf("unsupported OS %s", runtime.GOOS)
}

func muslHost() bool {
	for _, p := range []string{"/lib/libc.musl-x86_64.so.1", "/lib/libc.musl-aarch64.so.1"} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

type claudeManifest struct {
	Version   string `json:"version"`
	Platforms map[string]struct {
		Checksum string `json:"checksum"`
		Size     int64  `json:"size"`
	} `json:"platforms"`
}

func (s *ClaudeSource) Fetch(ctx context.Context, version, dir string) error {
	platform, err := s.platform()
	if err != nil {
		return err
	}
	b, err := getSmall(ctx, s.BaseURL+"/"+version+"/manifest.json")
	if err != nil {
		return err
	}
	var man claudeManifest
	if err := json.Unmarshal(b, &man); err != nil {
		return fmt.Errorf("parse manifest: %w", err)
	}
	entry, ok := man.Platforms[platform]
	if !ok || len(entry.Checksum) != 64 {
		return fmt.Errorf("platform %s not in the %s manifest", platform, version)
	}
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		return err
	}
	bin := filepath.Join(dir, "bin", "claude")
	sum, size, err := download(ctx, s.BaseURL+"/"+version+"/"+platform+"/claude", bin)
	if err != nil {
		return err
	}
	if !strings.EqualFold(sum, entry.Checksum) {
		return fmt.Errorf("checksum mismatch for claude %s (%s): got %s want %s", version, platform, sum, entry.Checksum)
	}
	if entry.Size > 0 && size != entry.Size {
		return fmt.Errorf("size mismatch for claude %s: got %d want %d", version, size, entry.Size)
	}
	return os.Chmod(bin, 0o755)
}

// CodexSource installs Codex's standalone release package
// (codex-package-<target>.tar.gz, verified against codex-package_SHA256SUMS).
// The package carries bin/codex plus its helpers (bundled rg, bwrap).
type CodexSource struct {
	BaseURL string
	Target  string // override for tests; "" ⇒ detected
	// LatestURL resolves "latest" by following the GitHub redirect; tests set it.
	LatestURL string
}

func (*CodexSource) DefaultChannel() string { return "latest" }

func (s *CodexSource) Latest(ctx context.Context, channel string) (string, error) {
	if channel != "latest" && channel != "stable" {
		return "", fmt.Errorf("unknown Codex channel %q (want latest or a version)", channel)
	}
	url := s.LatestURL
	if url == "" {
		url = s.BaseURL + "/latest"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)
	client := *httpClient
	client.Timeout = time.Minute
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	if loc == "" {
		return "", fmt.Errorf("resolve latest Codex release: no redirect from %s (%s)", url, resp.Status)
	}
	tag := loc[strings.LastIndex(loc, "/")+1:]
	return strings.TrimPrefix(tag, "rust-v"), nil
}

func (s *CodexSource) target() (string, error) {
	if s.Target != "" {
		return s.Target, nil
	}
	arch := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[runtime.GOARCH]
	if arch == "" {
		return "", fmt.Errorf("unsupported architecture %s", runtime.GOARCH)
	}
	switch runtime.GOOS {
	case "darwin":
		return arch + "-apple-darwin", nil
	case "linux":
		return arch + "-unknown-linux-musl", nil
	}
	return "", fmt.Errorf("unsupported OS %s", runtime.GOOS)
}

func (s *CodexSource) Fetch(ctx context.Context, version, dir string) error {
	target, err := s.target()
	if err != nil {
		return err
	}
	base := s.BaseURL + "/download/rust-v" + version + "/"
	asset := "codex-package-" + target + ".tar.gz"
	sums, err := getSmall(ctx, base+"codex-package_SHA256SUMS")
	if err != nil {
		return err
	}
	want := ""
	sc := bufio.NewScanner(strings.NewReader(string(sums)))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == asset {
			want = f[0]
		}
	}
	if len(want) != 64 {
		return fmt.Errorf("%s not listed in codex-package_SHA256SUMS for %s", asset, version)
	}
	archive := filepath.Join(dir, ".download.tar.gz")
	sum, _, err := download(ctx, base+asset, archive)
	if err != nil {
		return err
	}
	if !strings.EqualFold(sum, want) {
		return fmt.Errorf("checksum mismatch for %s: got %s want %s", asset, sum, want)
	}
	if err := extractTarGz(archive, dir); err != nil {
		return err
	}
	return os.Remove(archive)
}

// extractTarGz unpacks regular files, directories and in-tree symlinks,
// rejecting any entry that would escape dir.
func extractTarGz(archive, dir string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	inside := func(p string) bool {
		rel, err := filepath.Rel(dir, p)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(dir, filepath.Clean("/"+h.Name))
		if !inside(target) {
			return fmt.Errorf("archive entry %q escapes the install directory", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(h.Mode)&0o755|0o600)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if filepath.IsAbs(h.Linkname) || !inside(filepath.Join(filepath.Dir(target), h.Linkname)) {
				return fmt.Errorf("archive symlink %q -> %q escapes the install directory", h.Name, h.Linkname)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(h.Linkname, target); err != nil {
				return err
			}
		default:
			// Hard links, devices, FIFOs: never needed by a CLI package.
		}
	}
}
