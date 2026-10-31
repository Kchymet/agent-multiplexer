package daemon

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestSIGUSR1WritesGoroutineDump(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out lockedBuffer
	DumpGoroutinesOnSignal(ctx, &out)
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGUSR1); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out.String(), "=== end goroutine dump ===") {
		if time.Now().After(deadline) {
			t.Fatalf("no goroutine dump after SIGUSR1; got %q", out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	dump := out.String()
	if !strings.Contains(dump, "=== amux daemon goroutine dump (SIGUSR1)") || !strings.Contains(dump, "TestSIGUSR1WritesGoroutineDump") {
		t.Fatalf("dump lacks its header or this test's stack:\n%s", dump)
	}
}
