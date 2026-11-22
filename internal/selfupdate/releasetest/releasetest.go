// Package releasetest serves a fake amux release repository with GitHub's
// /releases/latest/download and /releases/download/<tag> layout, for
// self-update tests.
package releasetest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"amux/internal/selfupdate"
)

// Repo is a fake release repository.
type Repo struct {
	*httptest.Server
	mu       sync.Mutex
	latest   string
	releases map[string]map[string][]byte // tag → asset → bytes
	Requests []string
}

// New starts an empty repository, closed at test cleanup.
func New(t testing.TB) *Repo {
	r := &Repo{releases: map[string]map[string][]byte{}}
	r.Server = httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.Close)
	return r
}

// Publish adds a release built from m with the given assets (name → bytes),
// writes amux-release.json and a SHA256SUMS covering every asset, and makes it
// the latest. Corrupt overrides individual SHA256SUMS digests.
func (r *Repo) Publish(m selfupdate.Manifest, assets map[string][]byte, corrupt map[string]string) {
	files := map[string][]byte{}
	var names []string
	for name, b := range assets {
		files[name] = b
		names = append(names, name)
	}
	sort.Strings(names)
	var sums strings.Builder
	for _, name := range names {
		sum := sha256.Sum256(assets[name])
		digest := hex.EncodeToString(sum[:])
		if c, ok := corrupt[name]; ok {
			digest = c
		}
		fmt.Fprintf(&sums, "%s  %s\n", digest, name)
	}
	files[selfupdate.SumsAsset] = []byte(sums.String())
	mb, _ := json.Marshal(m)
	files[selfupdate.ManifestAsset] = mb
	r.mu.Lock()
	defer r.mu.Unlock()
	tag := selfupdate.Tag(m.Version)
	r.releases[tag] = files
	r.latest = tag
}

// Source is a selfupdate.Source reading this repository.
func (r *Repo) Source() selfupdate.Source {
	return selfupdate.Source{RepoURL: r.URL, Client: r.Client()}
}

func (r *Repo) serve(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Requests = append(r.Requests, req.URL.Path)
	var tag, asset string
	switch {
	case strings.HasPrefix(req.URL.Path, "/releases/latest/download/"):
		tag, asset = r.latest, strings.TrimPrefix(req.URL.Path, "/releases/latest/download/")
	case strings.HasPrefix(req.URL.Path, "/releases/download/"):
		rest := strings.TrimPrefix(req.URL.Path, "/releases/download/")
		tag, asset, _ = strings.Cut(rest, "/")
	default:
		http.NotFound(w, req)
		return
	}
	b, ok := r.releases[tag][asset]
	if !ok {
		http.NotFound(w, req)
		return
	}
	_, _ = w.Write(b)
}
