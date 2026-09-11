package hostprep

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"path/filepath"
)

// ErrWalkLimit reports that a descriptor-anchored discovery walk exhausted its
// explicit entry budget. Callers must degrade honestly rather than continue
// with an unbounded scan.
var ErrWalkLimit = errors.New("hostprep: bounded walk entry limit reached")

// WalkFilesBounded is WalkFiles with cancellation and a global directory-entry
// budget. Directory batches and recursion are bounded; symlinks and unsafe
// regular files retain WalkFiles' rejection behavior.
func (r *Root) WalkFilesBounded(ctx context.Context, rel string, maxEntries int, fn func(name string, info fs.FileInfo) error) error {
	if maxEntries <= 0 {
		return ErrWalkLimit
	}
	rel, err := r.Rel(rel)
	if err != nil {
		return err
	}
	base := r
	done := func() {}
	if rel != "." {
		base, err = r.Sub(rel, false, 0)
		if err != nil {
			return err
		}
		done = func() { _ = base.Close() }
	}
	defer done()
	remaining := maxEntries
	return base.walkFilesBounded(ctx, "", 0, &remaining, fn)
}

func (r *Root) walkFilesBounded(ctx context.Context, prefix string, depth int, remaining *int, fn func(name string, info fs.FileInfo) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if depth > 32 {
		return ErrWalkLimit
	}
	dir, err := r.dir.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if *remaining <= 0 {
			return ErrWalkLimit
		}
		batch := 64
		if batch > *remaining {
			batch = *remaining
		}
		entries, readErr := dir.ReadDir(batch)
		*remaining -= len(entries)
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			name := entry.Name()
			path := name
			if prefix != "" {
				path = filepath.Join(prefix, name)
			}
			if entry.Type()&fs.ModeSymlink != 0 {
				continue
			}
			if entry.IsDir() {
				child, err := r.openChild(name, false, 0)
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				if err != nil {
					return err
				}
				err = child.walkFilesBounded(ctx, path, depth+1, remaining, fn)
				_ = child.Close()
				if err != nil {
					return err
				}
				continue
			}
			if !entry.Type().IsRegular() {
				continue
			}
			f, err := r.OpenFile(name)
			if err != nil {
				return err
			}
			info, err := f.Stat()
			_ = f.Close()
			if err != nil {
				return err
			}
			if err := fn(path, info); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
		if len(entries) == 0 {
			return nil
		}
	}
}
