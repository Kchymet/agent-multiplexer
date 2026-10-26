package prefixcache

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValueIsReusedOnlyForTheSameGrowingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.jsonl")
	write := func(body string) {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	appendTo := func(body string) {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(body); err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	var c Cache
	reads := 0
	lookup := func(limit int64) string {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(path)
		scan := c.Begin(dir)
		defer scan.Commit()
		return scan.Value("t.jsonl", info, limit, func() string {
			reads++
			if len(data) >= 4 {
				return string(data[:4])
			}
			return ""
		})
	}

	write("ab")
	if got := lookup(1 << 20); got != "" || reads != 1 {
		t.Fatalf("first = %q reads=%d", got, reads)
	}
	if got := lookup(1 << 20); got != "" || reads != 1 {
		t.Fatalf("unchanged empty value re-read: %q reads=%d", got, reads)
	}
	appendTo("cd")
	if got := lookup(1 << 20); got != "abcd" || reads != 2 {
		t.Fatalf("grown file without a value not re-read: %q reads=%d", got, reads)
	}
	appendTo("efgh")
	if got := lookup(1 << 20); got != "abcd" || reads != 2 {
		t.Fatalf("found value re-read after append: %q reads=%d", got, reads)
	}

	// Replacement by rename is a different inode even at the same name.
	replacement := filepath.Join(dir, "new")
	if err := os.WriteFile(replacement, []byte("wxyz"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if got := lookup(1 << 20); got != "wxyz" || reads != 3 {
		t.Fatalf("replaced file served stale value: %q reads=%d", got, reads)
	}
	// A shrink (in-place truncate and rewrite) is never trusted.
	write("qr")
	if got := lookup(1 << 20); got != "" || reads != 4 {
		t.Fatalf("truncated file served stale value: %q reads=%d", got, reads)
	}
	// An empty value whose read already covered the whole limit stays final:
	// the 2-byte entry above saw everything a 2-byte limit ever reads.
	appendTo("st")
	if got := lookup(2); got != "" || reads != 4 {
		t.Fatalf("empty value past the read limit re-read: %q reads=%d", got, reads)
	}
}

func TestCommitDropsUnvisitedEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	var c Cache
	scan := c.Begin("scope")
	scan.Value("t", info, 1, func() string { return "v" })
	scan.Commit()
	c.Begin("scope").Commit() // a listing that no longer sees the file
	if n := len(c.scopes["scope"]); n != 0 {
		t.Fatalf("deleted file still cached: %d entries", n)
	}
}
