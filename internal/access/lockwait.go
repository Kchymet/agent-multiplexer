package access

import (
	"context"
	"sync/atomic"
	"time"
)

// LockWait accumulates the time a caller spent waiting for the authority's
// lock, so a slow request can say whether it was the lock or the work.
type LockWait struct{ nanos atomic.Int64 }

// Total is the accumulated wait.
func (w *LockWait) Total() time.Duration {
	if w == nil {
		return 0
	}
	return time.Duration(w.nanos.Load())
}

type lockWaitKey struct{}

// WithLockWait returns a context whose authority leases (Verify, WithValid)
// add their lock wait to the returned LockWait. Valid takes no lock.
func WithLockWait(ctx context.Context) (context.Context, *LockWait) {
	w := &LockWait{}
	return context.WithValue(ctx, lockWaitKey{}, w), w
}

// rlock takes mu for reading, recording the wait when ctx asks for it.
func (a *FileAuthority) rlock(ctx context.Context) {
	var w *LockWait
	if ctx != nil {
		w, _ = ctx.Value(lockWaitKey{}).(*LockWait)
	}
	if w == nil {
		a.mu.RLock()
		return
	}
	start := time.Now()
	a.mu.RLock()
	w.nanos.Add(int64(time.Since(start)))
}
