package directsvc

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ManagedFlag is the `amux direct serve` flag the daemon starts its child with.
// The child then reads its whole configuration from the saved setting, so the
// command line is constant and carries nothing worth reading.
const ManagedFlag = "--managed"

// maxLogBytes bounds direct.log: it is rotated to direct.log.1 when a child
// starts and finds it larger than this.
const maxLogBytes = 8 << 20

// ExecStarter starts `<self> direct serve --managed`, appending its output to
// logPath.
func ExecStarter(self, logPath string) Starter {
	return func(Settings) (Process, error) {
		if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
			return nil, err
		}
		rotateLog(logPath)
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return nil, err
		}
		tail := &lastLine{}
		cmd := exec.Command(self, ManagedArgs()...)
		cmd.Stdout = io.MultiWriter(f, tail)
		cmd.Stderr = cmd.Stdout
		cmd.Env = os.Environ()
		configureChild(cmd)
		if err := cmd.Start(); err != nil {
			_ = f.Close()
			return nil, err
		}
		return &execProcess{cmd: cmd, log: f, tail: tail}, nil
	}
}

// ManagedArgs is the managed child's argv after the binary.
func ManagedArgs() []string { return []string{"direct", "serve", ManagedFlag} }

type execProcess struct {
	cmd  *exec.Cmd
	log  *os.File
	tail *lastLine
}

func (p *execProcess) Pid() int { return p.cmd.Process.Pid }

func (p *execProcess) Wait() error {
	err := p.cmd.Wait()
	_ = p.log.Close()
	if line := p.tail.String(); line != "" {
		if err == nil {
			return fmt.Errorf("exited: %s", line)
		}
		return fmt.Errorf("%v: %s", err, line)
	}
	return err
}

func (p *execProcess) Terminate() error { return p.cmd.Process.Signal(syscall.SIGTERM) }
func (p *execProcess) Kill() error      { return p.cmd.Process.Kill() }

func rotateLog(path string) {
	if st, err := os.Stat(path); err == nil && st.Size() > maxLogBytes {
		_ = os.Rename(path, path+".1")
	}
}

// lastLine keeps the most recent non-empty line written to it.
type lastLine struct {
	mu   sync.Mutex
	buf  []byte
	last string
}

func (l *lastLine) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf = append(l.buf, p...)
	for {
		i := bytes.IndexByte(l.buf, '\n')
		if i < 0 {
			break
		}
		if s := strings.TrimSpace(string(l.buf[:i])); s != "" {
			l.last = s
		}
		l.buf = l.buf[i+1:]
	}
	if len(l.buf) > 4096 {
		l.buf = l.buf[len(l.buf)-4096:]
	}
	return len(p), nil
}

func (l *lastLine) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if s := strings.TrimSpace(string(l.buf)); s != "" {
		return s
	}
	return l.last
}

// WatchParent calls stop once this process's parent changes — the managed
// child's daemon has exited and the child was re-parented — so a daemon that
// died without stopping its child does not leave the listener running and
// holding the direct store. It polls every interval until done is closed.
func WatchParent(interval time.Duration, done <-chan struct{}, stop func()) {
	parent := os.Getppid()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			if os.Getppid() != parent {
				stop()
				return
			}
		}
	}
}
