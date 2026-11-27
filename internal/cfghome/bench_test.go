package cfghome

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// The drift-summary benchmark builds a reproducible synthetic machine: one
// Claude-shaped template (a .claude.json kept beside the home with Seed and
// Normalize hooks, settings.json whose Seed rewrites template paths, and large
// plugins/ and skills/ trees) seeded into N agent copies, a few of which carry
// agent-side edits. Sizes default to a real ~/.claude (≈800 plugin files, ≈230
// skill files) and are overridable for quick runs:
//
//	AMUX_BENCH_AGENTS (80)  AMUX_BENCH_PLUGIN_FILES (800)
//	AMUX_BENCH_SKILL_FILES (230)  AMUX_BENCH_FILE_BYTES (8192)
//
//	go test ./internal/cfghome -run '^$' -bench DriftSummary -benchtime 3x

type driftFixture struct {
	specs []Spec
	edits int // agent-side edits planted, i.e. the expected Pending total
}

func benchEnv(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return def
}

func benchSeed(s Spec, b []byte) []byte {
	return bytes.ReplaceAll(b, []byte(s.Template), []byte(s.Dir))
}

func benchRestore(s Spec, b []byte) []byte {
	return bytes.ReplaceAll(b, []byte(s.Dir), []byte(s.Template))
}

func buildDriftFixture(tb testing.TB) driftFixture {
	tb.Helper()
	agents := benchEnv("AMUX_BENCH_AGENTS", 80)
	plugins := benchEnv("AMUX_BENCH_PLUGIN_FILES", 800)
	skills := benchEnv("AMUX_BENCH_SKILL_FILES", 230)
	size := benchEnv("AMUX_BENCH_FILE_BYTES", 8192)

	root := tb.TempDir()
	if t, ok := tb.(*testing.T); ok {
		t.Setenv("HOME", filepath.Join(root, "home"))
	} else {
		tb.(*testing.B).Setenv("HOME", filepath.Join(root, "home"))
	}
	tmpl := filepath.Join(root, "user", ".claude")
	claudeJSON := filepath.Join(root, "user", ".claude.json")
	body := bytes.Repeat([]byte("x"), size)
	put := func(p string, b []byte) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			tb.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			tb.Fatal(err)
		}
	}
	put(claudeJSON, []byte(`{"mcpServers":{},"model":"opus"}`+"\n# state 1\n"))
	put(filepath.Join(tmpl, "settings.json"), []byte(`{"statusLine":"`+tmpl+`/statusline.sh"}`))
	put(filepath.Join(tmpl, "CLAUDE.md"), []byte("# memory\n"))
	for i := 0; i < plugins; i++ {
		put(filepath.Join(tmpl, "plugins", "marketplaces", fmt.Sprintf("m%d", i%7), fmt.Sprintf("p%d", i%40), fmt.Sprintf("f%04d.md", i)),
			append([]byte(fmt.Sprintf("plugin %d\n", i)), body...))
	}
	for i := 0; i < skills; i++ {
		put(filepath.Join(tmpl, "skills", fmt.Sprintf("s%d", i%30), fmt.Sprintf("f%04d.md", i)),
			append([]byte(fmt.Sprintf("skill %d\n", i)), body...))
	}
	// A .git tree inside a plugin marketplace is skipped on both sides.
	put(filepath.Join(tmpl, "plugins", "marketplaces", "m0", ".git", "HEAD"), []byte("ref: x\n"))

	var fx driftFixture
	for i := 0; i < agents; i++ {
		sess := filepath.Join(root, "sessions", fmt.Sprintf("a%03d", i))
		if err := os.MkdirAll(sess, 0o755); err != nil {
			tb.Fatal(err)
		}
		sp := Spec{
			Kind: "claude", AgentID: fmt.Sprintf("a%03d", i), Env: "CLAUDE_CONFIG_DIR",
			Template: tmpl, Root: sess, Dir: filepath.Join(sess, ".amux", "claude"),
			Entries: []Entry{
				{Rel: ".claude.json", Src: claudeJSON, Normalize: stripState},
				{Rel: "settings.json", Seed: benchSeed, Normalize: benchRestore},
				{Rel: "CLAUDE.md"},
				{Rel: "commands"},
				{Rel: "skills"},
				{Rel: "plugins"},
			},
		}
		if _, err := Seed(sp); err != nil {
			tb.Fatal(err)
		}
		// Every tenth agent edits a skill and adds a command; the harness's own
		// churn in .claude.json (normalized away) must not count.
		if i%10 == 0 {
			put(filepath.Join(sp.Dir, "skills", "s0", "f0000.md"), []byte("agent edit\n"))
			put(filepath.Join(sp.Dir, "commands", "new.md"), []byte("new\n"))
			fx.edits += 2
		}
		put(filepath.Join(sp.Dir, ".claude.json"), []byte(`{"mcpServers":{},"model":"opus"}`+"\n# state 99\n"))
		fx.specs = append(fx.specs, sp)
	}
	return fx
}

// runSummary is the production doctor/drift summary (DriftSummary, which
// `amux doctor` and `amux harness sandbox drift` call after resolving agents through
// the daemon). The baseline before this change was the equivalent sequential
// loop of Scan + Pending per agent.
func runSummary(tb testing.TB, specs []Spec, workers int) (agents, edits int) {
	agents, edits, err := DriftSummary(specs, ScanOptions{Workers: workers})
	if err != nil {
		tb.Fatal(err)
	}
	return agents, edits
}

// resetProcess drops in-process memo state so an iteration costs what a fresh
// `amux doctor` process pays.
func resetProcess() {
	hashMu.Lock()
	hashCache = map[string]hashEntry{}
	hashMu.Unlock()
}

// dropSidecars removes every stat sidecar: the state of an existing
// deployment's manifests on its first run after upgrading (legacy manifests).
func dropSidecars(tb testing.TB, specs []Spec) {
	for _, sp := range specs {
		if err := os.Remove(statCachePath(sp)); err != nil && !os.IsNotExist(err) {
			tb.Fatal(err)
		}
	}
}

func BenchmarkDriftSummary(b *testing.B) {
	fx := buildDriftFixture(b)
	// Steady state: config files are older than the reuse settle window.
	time.Sleep(hashSettle + 100*time.Millisecond)
	wantAgents := (len(fx.specs) + 9) / 10
	for _, bc := range []struct {
		name    string
		workers int
		legacy  bool
	}{
		{"legacy-manifests", 0, true},
		{"legacy-manifests/workers=1", 1, true},
		{"warm", 0, false},
		{"warm/workers=1", 1, false},
	} {
		b.Run(bc.name, func(b *testing.B) {
			if !bc.legacy {
				resetProcess()
				runSummary(b, fx.specs, bc.workers) // record sidecars, untimed
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				resetProcess()
				if bc.legacy {
					dropSidecars(b, fx.specs)
				}
				b.StartTimer()
				agents, edits := runSummary(b, fx.specs, bc.workers)
				if agents != wantAgents || edits != fx.edits {
					b.Fatalf("summary = %d agents/%d edits, want %d/%d", agents, edits, wantAgents, fx.edits)
				}
			}
		})
	}
}
