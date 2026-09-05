package codexapp

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// TestSmokeRealAppServer is the AGE-181 minimal real-runtime smoke test: it drives
// the supervisor against an ACTUAL `codex app-server` bound to a Unix socket in a
// throwaway sandbox, with a tiny prompt and no repo mutation. It is OPT-IN and
// self-skipping:
//
//   - requires AMUX_CODEX_APP_SERVER_SMOKE=1 (so CI / a plain `go test ./...` never
//     spawns a real model turn or touches credentials), AND
//   - requires a `codex` binary on PATH (this repo installs none; the ticket forbids
//     installing one). When absent it SKIPS with the reason — it never fudges a pass,
//     and a skip is not live verification.
//
// What it verifies when it runs:
//   - the pinned binary version (logged verbatim),
//   - the exact `codex app-server --listen unix://<socket>` invocation binds a
//     socket the supervisor dials and completes the initialize → thread/start
//     handshake against (returns a thread id),
//   - a turn produces a bracketed, normalized event stream (turn_start … turn_end),
//   - and the server accepts a SECOND concurrent client on the same socket — the
//     multi-client property the native `codex --remote resume <thread-id>` attach
//     depends on.
//
// What it deliberately does NOT claim: that the native Codex CLI, launched with
// `codex --remote unix://<socket> resume <thread-id>`, attaches to THIS running
// server/thread rather than starting its own process. That must be confirmed
// interactively on the host with the pinned CLI; this test proves the socket and
// protocol, not the CLI's attach semantics.
func TestSmokeRealAppServer(t *testing.T) {
	if os.Getenv("AMUX_CODEX_APP_SERVER_SMOKE") == "" {
		t.Skip("live smoke test disabled; set AMUX_CODEX_APP_SERVER_SMOKE=1 (and install a pinned codex) to run")
	}
	bin, err := exec.LookPath("codex")
	if err != nil {
		t.Skipf("codex binary not found on PATH: %v (install a pinned codex to run the live smoke test)", err)
	}
	verOut, verErr := exec.Command(bin, "--version").CombinedOutput()
	t.Logf("codex binary: %s", bin)
	t.Logf("codex --version: %s (err=%v)", strings.TrimSpace(string(verOut)), verErr)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	sandbox := t.TempDir()
	socket := filepath.Join(sandbox, "app.sock")
	t.Logf("app-server argv: %v", AppServerArgv(bin, socket))

	sup := New(Config{SessionID: "smoke", Bin: bin, Dir: sandbox, SocketPath: socket})
	if err := sup.Start(ctx); err != nil {
		t.Fatalf("Start (spawn app-server + dial socket + handshake): %v", err)
	}
	defer sup.Close()

	threadID := sup.ThreadID()
	if threadID == "" {
		t.Fatal("real binary returned an empty thread id")
	}
	t.Logf("handshake OK; thread id = %s", threadID)

	// A second client must be able to dial the same socket — the multi-client
	// property native `--remote` attach relies on.
	if c2, err := net.Dial("unix", socket); err != nil {
		t.Fatalf("second client could not dial the same socket: %v", err)
	} else {
		_ = c2.Close()
	}

	col := subscribeCollector(ctx, sup)
	var start, end int
	go func() {
		for b := range col.ch {
			for _, e := range b.Events {
				switch e.Type {
				case harnessproto.TypeTurnStart:
					start++
				case harnessproto.TypeTurnEnd:
					end++
				}
			}
		}
	}()

	pctx, pcancel := context.WithTimeout(ctx, 90*time.Second)
	defer pcancel()
	if err := sup.Prompt(pctx, "Reply with the single word: pong"); err != nil {
		t.Logf("Prompt returned: %v (stderr may explain, e.g. missing auth) — the handshake already proved the wire", err)
	}
	time.Sleep(200 * time.Millisecond) // let the drain goroutine observe turn_end
	if start == 0 || end == 0 {
		t.Fatalf("turn not bracketed against the real binary (turn_start=%d turn_end=%d)", start, end)
	}
	t.Logf("turn bracketed OK: turn_start=%d turn_end=%d", start, end)
}
