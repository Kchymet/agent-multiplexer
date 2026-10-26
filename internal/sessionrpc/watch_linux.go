//go:build linux

package sessionrpc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

// Renames publish requests and responses; a close after a direct write covers
// writers that do not rename. Publication temporaries are filtered by name.
const inotifyMask = unix.IN_MOVED_TO | unix.IN_CLOSE_WRITE | unix.IN_ONLYDIR

type inotifyWatcher struct {
	fd      int
	mu      sync.Mutex
	watches map[int32]map[*dirWatch]func()
}

var (
	inotifyOnce   sync.Once
	inotifyShared *inotifyWatcher
	inotifyErr    error
)

func sharedInotify() (*inotifyWatcher, error) {
	inotifyOnce.Do(func() {
		fd, err := unix.InotifyInit1(unix.IN_CLOEXEC)
		if err != nil {
			inotifyErr = fmt.Errorf("inotify: %w", err)
			return
		}
		inotifyShared = &inotifyWatcher{fd: fd, watches: make(map[int32]map[*dirWatch]func())}
		go inotifyShared.read()
	})
	return inotifyShared, inotifyErr
}

func platformWatchDir(dir *os.File, path string, notify func()) (*dirWatch, error) {
	if dir == nil || notify == nil {
		return nil, ErrInvalidRecord
	}
	w, err := sharedInotify()
	if err != nil {
		return nil, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	// inotify needs a name. The held descriptor's /proc link names exactly the
	// opened directory; the path is the fallback where /proc is not mounted.
	wd, err := unix.InotifyAddWatch(w.fd, fmt.Sprintf("/proc/self/fd/%d", dir.Fd()), inotifyMask)
	if err != nil && path != "" {
		wd, err = unix.InotifyAddWatch(w.fd, path, inotifyMask)
	}
	if err != nil {
		return nil, fmt.Errorf("inotify watch: %w", err)
	}
	id := int32(wd)
	watch := &dirWatch{}
	watch.cancel = func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		subs := w.watches[id]
		delete(subs, watch)
		if len(subs) == 0 && subs != nil {
			delete(w.watches, id)
			_, _ = unix.InotifyRmWatch(w.fd, uint32(id))
		}
	}
	if w.watches[id] == nil {
		// Watching the same directory again returns the same descriptor.
		w.watches[id] = make(map[*dirWatch]func())
	}
	w.watches[id][watch] = notify
	return watch, nil
}

func (w *inotifyWatcher) read() {
	buf := make([]byte, 64*1024)
	for {
		n, err := unix.Read(w.fd, buf)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil || n <= 0 {
			// The instance is process-wide and never closed. Consumers keep their
			// fallback interval, so service degrades to polling rather than stops.
			return
		}
		woken := map[int32]bool{}
		var ignored []int32
		all := false
		for offset := 0; offset+unix.SizeofInotifyEvent <= n; {
			wd := int32(binary.NativeEndian.Uint32(buf[offset:]))
			mask := binary.NativeEndian.Uint32(buf[offset+4:])
			length := int(binary.NativeEndian.Uint32(buf[offset+12:]))
			start := offset + unix.SizeofInotifyEvent
			end := min(start+length, n)
			name := strings.TrimRight(string(buf[start:end]), "\x00")
			offset = start + length
			switch {
			case mask&unix.IN_Q_OVERFLOW != 0:
				all = true
			case strings.HasPrefix(name, ".tmp-") && mask&unix.IN_IGNORED == 0:
				// A temporary's own close precedes its rename into place.
			default:
				woken[wd] = true
			}
			if mask&unix.IN_IGNORED != 0 {
				ignored = append(ignored, wd)
			}
		}
		var notify []func()
		w.mu.Lock()
		for wd, subs := range w.watches {
			if all || woken[wd] {
				for _, fn := range subs {
					notify = append(notify, fn)
				}
			}
		}
		// The kernel dropped these watches (the directory went away); their
		// subscribers were just woken and continue on their fallback interval.
		for _, wd := range ignored {
			delete(w.watches, wd)
		}
		w.mu.Unlock()
		for _, fn := range notify {
			fn()
		}
	}
}
