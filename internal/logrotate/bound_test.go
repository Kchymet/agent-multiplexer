package logrotate

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSized(t *testing.T, path string, n int, fill byte) {
	t.Helper()
	if err := os.WriteFile(path, bytes.Repeat([]byte{fill}, n), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A legacy daemon.log.1 far larger than rotation allows (one reached 9.1 GB)
// is trimmed to its newest MaxBytes; older generations are gzipped; anything
// past Keep is removed; and the live log is never touched.
func TestBoundTrimsCompressesAndCaps(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")
	writeSized(t, path, 5000, 'L')
	if err := os.WriteFile(path+".1", append(bytes.Repeat([]byte{'a'}, 9900), bytes.Repeat([]byte{'z'}, 100)...), 0o644); err != nil {
		t.Fatal(err)
	}
	writeSized(t, path+".2", 300, 'b')
	writeSized(t, path+".7", 300, 'c')
	p := Policy{Path: path, MaxBytes: 1000, Keep: 3, MaxTotal: 4000, Compress: true}
	if err := Bound(p); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Size() != 5000 {
		t.Fatalf("live log touched: %v %v", fi, err)
	}
	if _, err := os.Stat(path + ".7"); !os.IsNotExist(err) {
		t.Fatalf("generation past Keep survived: %v", err)
	}
	one, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if len(one) > 1100 || !strings.HasSuffix(string(one), strings.Repeat("z", 100)) || !strings.HasPrefix(string(one), "[amux: log trimmed") {
		t.Fatalf("outsized generation not trimmed to its tail: %d bytes", len(one))
	}
	if _, err := os.Stat(path + ".2"); !os.IsNotExist(err) {
		t.Fatalf("generation 2 left uncompressed: %v", err)
	}
	f, err := os.Open(path + ".2.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := io.ReadAll(zr); string(got) != strings.Repeat("b", 300) {
		t.Fatalf("gzip round trip = %q", got)
	}
	if _, rotated := Usage(path); rotated > p.MaxTotal {
		t.Fatalf("rotated total %d over %d", rotated, p.MaxTotal)
	}
}

func TestBoundRemovesOldestOverTotal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")
	writeSized(t, path+".1", 900, 'a')
	writeSized(t, path+".2", 900, 'b')
	writeSized(t, path+".3", 900, 'c')
	if err := Bound(Policy{Path: path, MaxBytes: 1000, Keep: 3, MaxTotal: 2000}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Fatal("oldest generation kept over the total cap")
	}
	for _, g := range []string{".1", ".2"} {
		if _, err := os.Stat(path + g); err != nil {
			t.Fatalf("newer generation %s removed: %v", g, err)
		}
	}
}

// Rotation shifts compressed generations along with plain ones.
func TestRotateShiftsCompressedGenerations(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	writeSized(t, path+".1", 10, 'a')
	writeSized(t, path+".2.gz", 10, 'b')
	if _, err := f.Write(bytes.Repeat([]byte{'x'}, 100)); err != nil {
		t.Fatal(err)
	}
	fd := int(f.Fd())
	rotated, err := RotateIfLarge(Policy{Path: path, MaxBytes: 50, Keep: 3, FDs: []int{fd}})
	if err != nil || !rotated {
		t.Fatalf("rotate = %v, %v", rotated, err)
	}
	for _, name := range []string{".1", ".2", ".3.gz"} {
		if _, err := os.Stat(path + name); err != nil {
			t.Fatalf("missing %s after rotation: %v", name, err)
		}
	}
}
