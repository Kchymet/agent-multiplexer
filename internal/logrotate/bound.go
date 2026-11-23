package logrotate

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// DefaultMaxTotal bounds the rotated generations together. Rotation alone did
// not bound disk: a daemon.log.1 that predates rotation (or grew while the
// daemon's log was not ours to rotate) is kept forever until enough later
// rotations push it out, and one such file reached 9.1 GB.
const DefaultMaxTotal = 256 << 20

// Generation is one rotated file of a log.
type Generation struct {
	Path string
	N    int
	Size int64
	Gzip bool
}

// Generations lists path's rotated generations (path.N and path.N.gz),
// oldest (highest N) first.
func Generations(path string) []Generation {
	dir, base := filepath.Dir(path), filepath.Base(path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Generation
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, base+".") || !e.Type().IsRegular() {
			continue
		}
		rest := strings.TrimPrefix(name, base+".")
		gz := strings.HasSuffix(rest, ".gz")
		rest = strings.TrimSuffix(rest, ".gz")
		n, err := strconv.Atoi(rest)
		if err != nil || n < 1 {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, Generation{Path: filepath.Join(dir, name), N: n, Size: info.Size(), Gzip: gz})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].N != out[j].N {
			return out[i].N > out[j].N
		}
		return out[i].Gzip && !out[j].Gzip
	})
	return out
}

// Usage is the disk the live log and its generations take together.
func Usage(path string) (live, rotated int64) {
	if fi, err := os.Stat(path); err == nil {
		live = fi.Size()
	}
	for _, g := range Generations(path) {
		rotated += g.Size
	}
	return live, rotated
}

// Bound keeps p's rotated generations within p.MaxTotal: it removes
// generations beyond p.Keep, trims an outsized generation to its newest
// p.MaxBytes, gzips generations 2 and older, and then removes the oldest
// generations until the total fits. It never touches the live log.
func Bound(p Policy) error {
	if p.Keep < 1 || p.MaxBytes <= 0 {
		return nil
	}
	var errs []error
	for _, g := range Generations(p.Path) {
		if g.N > p.Keep {
			errs = append(errs, removeIfExists(g.Path))
			continue
		}
		if !g.Gzip && g.Size > 2*p.MaxBytes {
			if err := keepTail(g.Path, p.MaxBytes); err != nil {
				errs = append(errs, err)
				continue
			}
		}
		if p.Compress && !g.Gzip && g.N >= 2 {
			errs = append(errs, gzipFile(g.Path))
		}
	}
	if p.MaxTotal > 0 {
		gens := Generations(p.Path)
		var total int64
		for _, g := range gens {
			total += g.Size
		}
		for _, g := range gens { // oldest first
			if total <= p.MaxTotal {
				break
			}
			if err := removeIfExists(g.Path); err != nil {
				errs = append(errs, err)
				continue
			}
			total -= g.Size
		}
	}
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// keepTail replaces path with its last n bytes.
func keepTail(path string, n int64) error {
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer src.Close()
	fi, err := src.Stat()
	if err != nil {
		return err
	}
	if fi.Size() > n {
		if _, err := src.Seek(fi.Size()-n, io.SeekStart); err != nil {
			return err
		}
	}
	tmp := path + ".trim"
	dst, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, fi.Mode().Perm())
	if err != nil {
		return err
	}
	fmt.Fprintf(dst, "[amux: log trimmed to its last %d MiB of %d MiB]\n", n>>20, fi.Size()>>20)
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := dst.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// gzipFile replaces path with path.gz.
func gzipFile(path string) error {
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer src.Close()
	fi, err := src.Stat()
	if err != nil {
		return err
	}
	tmp := path + ".gz.tmp"
	dst, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, fi.Mode().Perm())
	if err != nil {
		return err
	}
	zw := gzip.NewWriter(dst)
	_, err = io.Copy(zw, src)
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path+".gz")
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Remove(path)
}
