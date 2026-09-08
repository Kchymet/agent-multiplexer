package hostprep

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRootPinsSessionAcrossParentReplacement(t *testing.T) {
	parent := t.TempDir()
	session := filepath.Join(parent, "session")
	outside := filepath.Join(parent, "outside")
	if err := os.MkdirAll(session, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := OpenSession(session)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	moved := session + ".moved"
	if err := os.Rename(session, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, session); err != nil {
		t.Fatal(err)
	}
	if err := r.AtomicWrite(".amux/codex/config.toml", []byte("inside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(moved, ".amux", "codex", "config.toml")); err != nil || string(b) != "inside" {
		t.Fatalf("pinned destination = %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(outside, ".amux", "codex", "config.toml")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("replacement target was touched: %v", err)
	}
}

func TestOpenFileRejectsRegularToFIFOSwapWithoutBlocking(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix FIFO regression")
	}
	session := t.TempDir()
	path := filepath.Join(session, "config.toml")
	if err := os.WriteFile(path, []byte("safe"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := OpenSession(session)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	original := openReadFile
	defer func() { openReadFile = original }()
	openReadFile = func(root *os.Root, name string) (*os.File, error) {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			t.Fatal(err)
		}
		return original(root, name)
	}
	done := make(chan error, 1)
	go func() {
		_, err := r.ReadFile("config.toml")
		done <- err
	}()
	select {
	case err := <-done:
		if !IsUnsafe(err) {
			t.Fatalf("FIFO replacement error = %v, want unsafe refusal", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("regular-to-FIFO replacement blocked preparation")
	}
}

func TestReadFileHasExplicitWholeFileLimit(t *testing.T) {
	session := t.TempDir()
	path := filepath.Join(session, "config.toml")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(MaxReadFileSize + 1); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := OpenSession(session)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.ReadFile("config.toml"); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("ReadFile error = %v, want ErrFileTooLarge", err)
	}
}

func TestRootRejectsDestinationAliasesAndHardlinks(t *testing.T) {
	for _, tc := range []struct {
		name  string
		plant func(t *testing.T, session, outside, canary string)
		rel   string
	}{
		{
			name: "directory symlink",
			plant: func(t *testing.T, session, outside, _ string) {
				t.Helper()
				mustMkdir(t, filepath.Join(session, ".amux"))
				if err := os.Symlink(outside, filepath.Join(session, ".amux", "codex")); err != nil {
					t.Fatal(err)
				}
			},
			rel: ".amux/codex/config.toml",
		},
		{
			name: "final symlink read",
			plant: func(t *testing.T, session, _, canary string) {
				t.Helper()
				mustMkdir(t, filepath.Join(session, ".amux", "codex"))
				if err := os.Symlink(canary, filepath.Join(session, ".amux", "codex", "config.toml")); err != nil {
					t.Fatal(err)
				}
			},
			rel: ".amux/codex/config.toml",
		},
		{
			name: "final hardlink read",
			plant: func(t *testing.T, session, _, canary string) {
				t.Helper()
				if runtime.GOOS == "windows" {
					t.Skip("Unix hard-link policy")
				}
				mustMkdir(t, filepath.Join(session, ".amux", "codex"))
				if err := os.Link(canary, filepath.Join(session, ".amux", "codex", "config.toml")); err != nil {
					t.Fatal(err)
				}
			},
			rel: ".amux/codex/config.toml",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			session := filepath.Join(base, "session")
			outside := filepath.Join(base, "outside")
			mustMkdir(t, session)
			mustMkdir(t, outside)
			canary := filepath.Join(outside, "canary")
			if err := os.WriteFile(canary, []byte("outside-canary"), 0o600); err != nil {
				t.Fatal(err)
			}
			tc.plant(t, session, outside, canary)
			r, err := OpenSession(session)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			if _, err := r.ReadFile(tc.rel); err == nil {
				t.Fatal("unsafe destination read succeeded")
			}
			if b, err := os.ReadFile(canary); err != nil || string(b) != "outside-canary" {
				t.Fatalf("outside canary changed: %q, %v", b, err)
			}
		})
	}
}

func TestAtomicWriteIgnoresPredictableTempAndReplacesFinalAlias(t *testing.T) {
	base := t.TempDir()
	session := filepath.Join(base, "session")
	out := filepath.Join(base, "outside")
	mustMkdir(t, filepath.Join(session, ".amux", "codex"))
	mustMkdir(t, out)
	canary := filepath.Join(out, "canary")
	if err := os.WriteFile(canary, []byte("outside-canary"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(session, ".amux", "codex")
	if err := os.Symlink(canary, filepath.Join(dir, "config.toml.amux.tmp")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(canary, filepath.Join(dir, "config.toml")); err != nil {
		t.Fatal(err)
	}
	r, err := OpenSession(session)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.AtomicWrite(".amux/codex/config.toml", []byte("trusted"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(canary); err != nil || string(b) != "outside-canary" {
		t.Fatalf("outside canary changed: %q, %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "config.toml")); err != nil || string(b) != "trusted" {
		t.Fatalf("published file = %q, %v", b, err)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".amux-") && strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("temporary file leaked: %s", e.Name())
		}
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}
