package daemon

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime/pprof"
	"syscall"
	"time"
)

// DumpGoroutinesOnSignal writes every goroutine's stack to w each time the
// process receives SIGUSR1, until ctx ends. The daemon's stderr is its log,
// so `kill -USR1 <daemon pid>` records where a live stall is waiting without
// stopping or restarting anything. It returns once the handler is installed.
//
// Before a daemon built with this handler runs, SIGUSR1 keeps Go's default
// action and terminates the process.
func DumpGoroutinesOnSignal(ctx context.Context, w io.Writer) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGUSR1)
	go func() {
		defer signal.Stop(signals)
		for {
			select {
			case <-ctx.Done():
				return
			case <-signals:
				writeGoroutineDump(w, "SIGUSR1")
			}
		}
	}()
}

// writeGoroutineDump renders the dump first and writes it in one call, so it
// is not interleaved with log lines other goroutines write meanwhile.
func writeGoroutineDump(w io.Writer, reason string) {
	var b bytes.Buffer
	fmt.Fprintf(&b, "=== amux daemon goroutine dump (%s) at %s ===\n", reason, time.Now().Format(time.RFC3339Nano))
	if err := pprof.Lookup("goroutine").WriteTo(&b, 2); err != nil {
		fmt.Fprintf(&b, "goroutine dump failed: %v\n", err)
	}
	b.WriteString("=== end goroutine dump ===\n")
	_, _ = w.Write(b.Bytes())
}
