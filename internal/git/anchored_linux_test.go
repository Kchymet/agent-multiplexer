//go:build linux

package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func countOpenDescriptorsBelow(t *testing.T, root string) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("/proc/self/fd unavailable: %v", err)
	}
	root += string(filepath.Separator)
	count := 0
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
		if err == nil && strings.HasPrefix(target, root) {
			count++
		}
	}
	return count
}

func TestMkdirAllAnchoredClosesEveryOwnedDescriptor(t *testing.T) {
	root := t.TempDir()
	before := countOpenDescriptorsBelow(t, root)
	for i := 0; i < 32; i++ {
		target := filepath.Join(root, "session", "nested", "dir", strings.Repeat("x", i+1))
		if err := mkdirAllAnchored(root, target, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	after := countOpenDescriptorsBelow(t, root)
	if after != before {
		t.Fatalf("mkdirAllAnchored leaked managed-root descriptors: before=%d after=%d", before, after)
	}
}
