//go:build linux

package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"amux/internal/claudecfg"
	"amux/internal/codexcfg"
	"amux/internal/core"
	"amux/internal/store"
)

// discoveryPopulation writes a host shaped like a long-lived amux install:
// many agent homes (live and archived), each with a few transcripts whose cwd
// appears only after a large leading record, plus the user's own homes.
func discoveryPopulation(tb testing.TB, agents, perHome int) int {
	tb.Helper()
	home := tb.TempDir()
	tb.Setenv("HOME", home)
	tb.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	tb.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	tb.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	lead := `{"type":"file-history-snapshot","snapshot":"` + strings.Repeat("x", 48<<10) + `"}` + "\n"
	tail := strings.Repeat(`{"type":"assistant","message":"`+strings.Repeat("y", 1<<10)+`"}`+"\n", 256)
	write := func(path, cwd string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			tb.Fatal(err)
		}
		body := lead + `{"type":"user","cwd":"` + cwd + `"}` + "\n" + tail
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			tb.Fatal(err)
		}
	}
	db, err := store.Open()
	if err != nil {
		tb.Fatal(err)
	}
	defer db.Close()
	files := 0
	uuid := func(n int) string { return fmt.Sprintf("%08x-0000-4000-8000-%012x", n, n) }
	for i := 0; i < agents; i++ {
		kind := "claude"
		if i%4 == 3 {
			kind = "codex"
		}
		dir := filepath.Join(core.SessionsDir(), "root", fmt.Sprintf("a%04d", i))
		if err := db.PutSession(store.Session{ID: fmt.Sprintf("a%04d", i), RootID: "root", Agent: kind, Dir: dir, Archived: i%3 != 0}); err != nil {
			tb.Fatal(err)
		}
		for j := 0; j < perHome; j++ {
			id := uuid(i*perHome + j + 1)
			if kind == "claude" {
				write(claudecfg.At(claudecfg.AgentHome(dir)).TranscriptPath(dir, id), dir)
			} else {
				write(codexcfg.At(codexcfg.AgentHome(dir)).NewRolloutPath(id), dir)
			}
			files++
		}
	}
	for j := 0; j < agents; j++ {
		write(claudecfg.User().TranscriptPath("/work/user", uuid(1<<20+j)), "/work/user")
		files++
	}
	return files
}

func BenchmarkListSessionRows(b *testing.B) {
	files := discoveryPopulation(b, 200, 8)
	if rows := ListSessionRows(); len(rows) != files {
		b.Fatalf("rows=%d files=%d", len(rows), files)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if rows := ListSessionRows(); len(rows) != files {
			b.Fatalf("rows=%d files=%d", len(rows), files)
		}
	}
}
