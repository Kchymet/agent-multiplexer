//go:build darwin

package sessionrpc

import (
	"errors"
	"fmt"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

// A directory's vnode reports NOTE_WRITE when an entry is added, removed or
// renamed into it, which is how requests and responses are published.
const kqueueFflags = unix.NOTE_WRITE | unix.NOTE_EXTEND | unix.NOTE_DELETE | unix.NOTE_RENAME | unix.NOTE_REVOKE

type kqueueWatcher struct {
	kq      int
	mu      sync.Mutex
	watches map[int]func()
}

var (
	kqueueOnce   sync.Once
	kqueueShared *kqueueWatcher
	kqueueErr    error
)

func sharedKqueue() (*kqueueWatcher, error) {
	kqueueOnce.Do(func() {
		kq, err := unix.Kqueue()
		if err != nil {
			kqueueErr = fmt.Errorf("kqueue: %w", err)
			return
		}
		unix.CloseOnExec(kq)
		kqueueShared = &kqueueWatcher{kq: kq, watches: make(map[int]func())}
		go kqueueShared.read()
	})
	return kqueueShared, kqueueErr
}

func platformWatchDir(dir *os.File, _ string, notify func()) (*dirWatch, error) {
	if dir == nil || notify == nil {
		return nil, ErrInvalidRecord
	}
	w, err := sharedKqueue()
	if err != nil {
		return nil, err
	}
	// The registration owns a duplicate of the held descriptor, so closing the
	// caller's directory and this watch are independent. No path is opened.
	// The duplicate is close-on-exec atomically: the daemon spawns sandboxed
	// processes concurrently, and none may inherit a host mailbox descriptor.
	fd, err := unix.FcntlInt(dir.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("kqueue watch: %w", err)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	var change [1]unix.Kevent_t
	unix.SetKevent(&change[0], fd, unix.EVFILT_VNODE, unix.EV_ADD|unix.EV_CLEAR|unix.EV_ENABLE)
	change[0].Fflags = kqueueFflags
	if _, err := unix.Kevent(w.kq, change[:], nil, nil); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("kqueue watch: %w", err)
	}
	w.watches[fd] = notify
	watch := &dirWatch{}
	watch.cancel = func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		delete(w.watches, fd)
		// Closing the descriptor removes its kevent registration.
		_ = unix.Close(fd)
	}
	return watch, nil
}

func (w *kqueueWatcher) read() {
	events := make([]unix.Kevent_t, 64)
	for {
		n, err := unix.Kevent(w.kq, nil, events, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			// The queue is process-wide and never closed. Consumers keep their
			// fallback interval, so service degrades to polling rather than stops.
			return
		}
		var notify []func()
		w.mu.Lock()
		for _, event := range events[:n] {
			if fn := w.watches[int(event.Ident)]; fn != nil {
				notify = append(notify, fn)
			}
		}
		w.mu.Unlock()
		for _, fn := range notify {
			fn()
		}
	}
}
