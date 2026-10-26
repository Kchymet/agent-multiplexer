package sessionrpc

import (
	"os"
	"sync"
	"time"
)

// WatchFallbackInterval bounds how long a missed directory event can delay
// service. Mailboxes whose directories cannot be watched keep the poll
// interval instead.
const WatchFallbackInterval = time.Second

// dirWatch is one registration on the process-wide directory watcher (one
// inotify instance or kqueue per process, never one per mailbox). Its notify
// callback runs on the watcher goroutine after the directory's entries may
// have changed: a rename into it, or a write that closed. Events are only
// wake hints. Every consumer re-lists through its own descriptor, so a missed,
// spurious or late event costs latency, never correctness, and a callback may
// still run briefly after Close returns.
type dirWatch struct {
	once   sync.Once
	cancel func()
}

func (w *dirWatch) Close() {
	if w != nil {
		w.once.Do(w.cancel)
	}
}

// watchDir watches dir, which was opened at path. Implementations prefer the
// held descriptor and use path only where the platform requires a name.
// notify must not block.
func watchDir(dir *os.File, path string, notify func()) (*dirWatch, error) {
	return platformWatchDir(dir, path, notify)
}

// wakeSignal coalesces wake hints into one pending token.
type wakeSignal chan struct{}

func newWakeSignal() wakeSignal { return make(wakeSignal, 1) }

func (w wakeSignal) notify() {
	select {
	case w <- struct{}{}:
	default:
	}
}

// waitWake blocks until a wake, the fallback interval, or ctx ends.
func waitWake(done <-chan struct{}, wake wakeSignal, fallback time.Duration) {
	timer := time.NewTimer(fallback)
	defer timer.Stop()
	select {
	case <-done:
	case <-wake:
	case <-timer.C:
	}
}
