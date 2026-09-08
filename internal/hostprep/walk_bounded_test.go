package hostprep

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestWalkFilesBoundedIsAnchoredCancellableAndLimited(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "tree", "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tree", "nested", "inside"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "tree", "alias")); err != nil {
		t.Fatal(err)
	}
	root, err := OpenSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	var names []string
	if err := root.WalkFilesBounded(context.Background(), "tree", 8, func(name string, _ fs.FileInfo) error {
		names = append(names, name)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != filepath.Join("nested", "inside") {
		t.Fatalf("walk names = %v", names)
	}
	if err := root.WalkFilesBounded(context.Background(), "tree", 1, func(string, fs.FileInfo) error { return nil }); !errors.Is(err, ErrWalkLimit) {
		t.Fatalf("limited walk error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := root.WalkFilesBounded(ctx, "tree", 8, func(string, fs.FileInfo) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled walk error = %v", err)
	}
}
