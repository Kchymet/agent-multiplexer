package harnessbin

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sha(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func TestClaudeSourceVerifiesManifestChecksum(t *testing.T) {
	bin := []byte("#!/bin/sh\necho 2.1.311 (Claude Code)\n")
	checksum := sha(bin)
	mux := http.NewServeMux()
	mux.HandleFunc("/stable", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "2.1.311\n") })
	mux.HandleFunc("/latest", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "2.1.325\n") })
	mux.HandleFunc("/2.1.311/manifest.json", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"version":"2.1.311","platforms":{"linux-x64":{"binary":"claude","checksum":%q,"size":%d}}}`, checksum, len(bin))
	})
	mux.HandleFunc("/2.1.311/linux-x64/claude", func(w http.ResponseWriter, _ *http.Request) { w.Write(bin) })
	mux.HandleFunc("/9.9.9/manifest.json", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"platforms":{"linux-x64":{"checksum":%q}}}`, strings.Repeat("0", 64))
	})
	mux.HandleFunc("/9.9.9/linux-x64/claude", func(w http.ResponseWriter, _ *http.Request) { w.Write(bin) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	src := &ClaudeSource{BaseURL: srv.URL, Platform: "linux-x64"}
	ctx := context.Background()
	for ch, want := range map[string]string{"stable": "2.1.311", "latest": "2.1.325"} {
		if v, err := src.Latest(ctx, ch); err != nil || v != want {
			t.Fatalf("Latest(%s) = %q, %v", ch, v, err)
		}
	}
	if _, err := src.Latest(ctx, "nightly"); err == nil {
		t.Fatal("unknown channel accepted")
	}
	dir := t.TempDir()
	if err := src.Fetch(ctx, "2.1.311", dir); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "bin", "claude"))
	if err != nil || !bytes.Equal(got, bin) {
		t.Fatalf("binary = %q, %v", got, err)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "bin", "claude")); fi.Mode()&0o111 == 0 {
		t.Fatal("binary not executable")
	}
	if err := src.Fetch(ctx, "9.9.9", t.TempDir()); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("tampered download = %v, want checksum mismatch", err)
	}
	if err := (&ClaudeSource{BaseURL: srv.URL, Platform: "plan9-x64"}).Fetch(ctx, "2.1.311", t.TempDir()); err == nil {
		t.Fatal("missing platform accepted")
	}
}

type tarEntry struct {
	name, link string
	body       string
	mode       int64
}

func makeTarGz(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Mode: e.mode, Size: int64(len(e.body)), Typeflag: tar.TypeReg}
		if e.link != "" {
			h.Typeflag, h.Linkname, h.Size = tar.TypeSymlink, e.link, 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.link == "" {
			tw.Write([]byte(e.body))
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestCodexSourceInstallsVerifiedPackage(t *testing.T) {
	const target = "x86_64-unknown-linux-musl"
	good := makeTarGz(t, []tarEntry{
		{name: "bin/codex", body: "#!/bin/sh\necho codex-cli 0.169.0\n", mode: 0o755},
		{name: "codex-path/rg", body: "rg", mode: 0o755},
		{name: "codex-resources/bwrap", body: "bwrap", mode: 0o755},
	})
	evil := makeTarGz(t, []tarEntry{
		{name: "bin/codex", body: "x", mode: 0o755},
		{name: "bin/escape", link: "../../../../etc/passwd"},
	})
	assets := map[string][]byte{"0.169.0": good, "0.170.0": evil}
	mux := http.NewServeMux()
	mux.HandleFunc("/latest", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/tag/rust-v0.169.0", http.StatusFound)
	})
	for v, archive := range assets {
		v, archive := v, archive
		mux.HandleFunc("/download/rust-v"+v+"/codex-package_SHA256SUMS", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprintf(w, "%s  codex-app-server-package-%s.tar.gz\n%s  codex-package-%s.tar.gz\n", strings.Repeat("a", 64), target, sha(archive), target)
		})
		mux.HandleFunc("/download/rust-v"+v+"/codex-package-"+target+".tar.gz", func(w http.ResponseWriter, _ *http.Request) { w.Write(archive) })
	}
	mux.HandleFunc("/download/rust-v0.171.0/codex-package_SHA256SUMS", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "%s  codex-package-%s.tar.gz\n", strings.Repeat("b", 64), target)
	})
	mux.HandleFunc("/download/rust-v0.171.0/codex-package-"+target+".tar.gz", func(w http.ResponseWriter, _ *http.Request) { w.Write(good) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	src := &CodexSource{BaseURL: srv.URL, Target: target}
	ctx := context.Background()
	if v, err := src.Latest(ctx, "latest"); err != nil || v != "0.169.0" {
		t.Fatalf("Latest = %q, %v", v, err)
	}
	dir := t.TempDir()
	if err := src.Fetch(ctx, "0.169.0", dir); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"bin/codex", "codex-path/rg", "codex-resources/bwrap"} {
		if !executable(filepath.Join(dir, p)) {
			t.Errorf("%s not installed executable", p)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".download.tar.gz")); !os.IsNotExist(err) {
		t.Error("downloaded archive left in the version dir")
	}
	if err := src.Fetch(ctx, "0.170.0", t.TempDir()); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("escaping symlink = %v", err)
	}
	if err := src.Fetch(ctx, "0.171.0", t.TempDir()); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("tampered package = %v", err)
	}
}

// End to end through the Manager with a real HTTP source and the default
// verifier (which executes the downloaded binary).
func TestManagerInstallsFromHTTPSource(t *testing.T) {
	bin := []byte("#!/bin/sh\necho 1.2.3\n")
	mux := http.NewServeMux()
	mux.HandleFunc("/stable", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "1.2.3") })
	mux.HandleFunc("/1.2.3/manifest.json", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"platforms":{"test":{"checksum":%q}}}`, sha(bin))
	})
	mux.HandleFunc("/1.2.3/test/claude", func(w http.ResponseWriter, _ *http.Request) { w.Write(bin) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	m := &Manager{Root: t.TempDir(), Sources: map[string]Source{"claude": &ClaudeSource{BaseURL: srv.URL, Platform: "test"}}}
	v, err := m.Install(context.Background(), "claude", "", true)
	if err != nil || v != "1.2.3" || m.Current("claude") != "1.2.3" {
		t.Fatalf("Install = %q, %v (current %q)", v, err, m.Current("claude"))
	}
}
