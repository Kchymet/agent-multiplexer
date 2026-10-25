// Package prefixcache memoizes a value derived from a bounded prefix of an
// append-only file, keyed by the file's identity. Transcript discovery parses
// the leading records of every conversation on the host to learn its cwd; that
// prefix never changes once written, so re-reading it on every listing turns a
// directory enumeration into gigabytes of I/O on a long-lived install.
//
// The cache never replaces the caller's open/validation: callers still open
// each file through their safe resolver and pass the verified FileInfo. Only
// the prefix parse is skipped.
package prefixcache

import (
	"os"
	"sync"
	"syscall"
)

type entry struct {
	dev, ino uint64
	size     int64
	value    string
}

// Cache holds the latest completed scan of each scope (one listing root).
type Cache struct {
	mu     sync.Mutex
	scopes map[string]map[string]entry
}

// Scan accumulates one listing of a scope. Entries not visited by the scan are
// dropped at Commit, so deleted files do not pin memory.
type Scan struct {
	cache *Cache
	scope string
	prev  map[string]entry
	next  map[string]entry
}

func (c *Cache) Begin(scope string) *Scan {
	c.mu.Lock()
	prev := c.scopes[scope]
	c.mu.Unlock()
	return &Scan{cache: c, scope: scope, prev: prev, next: make(map[string]entry, len(prev))}
}

// Value returns the cached value for name when info is the same file (device
// and inode) whose prefix produced it, reading it with read otherwise. A found
// value is final while the file only grows. An empty value is reused only while
// the file is unchanged in size or already past limit, the most bytes read
// ever consults.
func (s *Scan) Value(name string, info os.FileInfo, limit int64, read func() string) string {
	dev, ino, ok := identity(info)
	if !ok {
		return read()
	}
	size := info.Size()
	if old, hit := s.prev[name]; hit && old.dev == dev && old.ino == ino && size >= old.size &&
		(old.value != "" || size == old.size || old.size >= limit) {
		s.next[name] = old
		return old.value
	}
	value := read()
	s.next[name] = entry{dev: dev, ino: ino, size: size, value: value}
	return value
}

func (s *Scan) Commit() {
	s.cache.mu.Lock()
	defer s.cache.mu.Unlock()
	if s.cache.scopes == nil {
		s.cache.scopes = make(map[string]map[string]entry)
	}
	s.cache.scopes[s.scope] = s.next
}

func identity(info os.FileInfo) (dev, ino uint64, ok bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return uint64(st.Dev), uint64(st.Ino), true
}
