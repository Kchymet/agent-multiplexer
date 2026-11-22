package selfupdate

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// DefaultRepoURL is where amux releases are published.
const DefaultRepoURL = "https://github.com/Kchymet/agent-multiplexer"

// RepoURLEnv overrides the release repository (a fork, a mirror, or a test
// server serving the same /releases/... layout).
const RepoURLEnv = "AMUX_UPDATE_REPO_URL"

const (
	maxSmallAsset  = 1 << 20   // manifest, checksums, shim
	maxBinaryAsset = 512 << 20 // release binary
)

// Source reads releases through GitHub's stable download URLs rather than its
// REST API: /releases/latest/download/<asset> redirects to the newest release's
// asset and is not subject to the unauthenticated API rate limit. Only the
// manifest is read through "latest"; every other asset is fetched from the
// manifest's exact tag so a release published mid-update cannot mix assets.
type Source struct {
	RepoURL string
	Client  *http.Client
}

// DefaultSource is the configured release repository.
func DefaultSource() Source {
	url := strings.TrimSpace(os.Getenv(RepoURLEnv))
	if url == "" {
		url = DefaultRepoURL
	}
	return Source{RepoURL: strings.TrimRight(url, "/"), Client: &http.Client{Timeout: 10 * time.Minute}}
}

func (s Source) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return http.DefaultClient
}

// Tag is the release tag for a version.
func Tag(version string) string { return "v" + strings.TrimPrefix(version, "v") }

func (s Source) latestURL(asset string) string {
	return s.RepoURL + "/releases/latest/download/" + asset
}

func (s Source) assetURL(version, asset string) string {
	return s.RepoURL + "/releases/download/" + Tag(version) + "/" + asset
}

// Latest returns the newest release's manifest.
func (s Source) Latest(ctx context.Context) (Manifest, error) {
	b, err := s.get(ctx, s.latestURL(ManifestAsset), maxSmallAsset)
	if err != nil {
		return Manifest{}, err
	}
	return ParseManifest(b)
}

// Sums returns the checksum table of a release.
func (s Source) Sums(ctx context.Context, version string) (map[string]string, error) {
	b, err := s.get(ctx, s.assetURL(version, SumsAsset), maxSmallAsset)
	if err != nil {
		return nil, err
	}
	return ParseSums(b)
}

// Asset reads a small release asset into memory.
func (s Source) Asset(ctx context.Context, version, asset string) ([]byte, error) {
	return s.get(ctx, s.assetURL(version, asset), maxSmallAsset)
}

// Download streams a release asset to w, bounded to the binary size limit.
func (s Source) Download(ctx context.Context, version, asset string, w io.Writer) error {
	body, err := s.open(ctx, s.assetURL(version, asset))
	if err != nil {
		return err
	}
	defer body.Close()
	n, err := io.Copy(w, io.LimitReader(body, maxBinaryAsset+1))
	if err != nil {
		return fmt.Errorf("download %s: %w", asset, err)
	}
	if n > maxBinaryAsset {
		return fmt.Errorf("download %s: larger than %d bytes", asset, maxBinaryAsset)
	}
	return nil
}

func (s Source) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	body, err := s.open(ctx, url)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	b, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("GET %s: response larger than %d bytes", url, limit)
	}
	return b, nil
}

func (s Source) open(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "amux-selfupdate")
	resp, err := s.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("GET %s: not found (no published release yet?)", url)
		}
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return resp.Body, nil
}
