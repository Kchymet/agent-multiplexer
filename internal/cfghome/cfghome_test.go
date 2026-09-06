package cfghome

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// spec builds a two-entry spec (a file with a Normalize hook stripping a
// "# state" trailer, and a directory) over fresh template/copy dirs, with the
// manifest state isolated under a temp HOME.
func spec(t *testing.T) Spec {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	tmpl := filepath.Join(root, "template")
	must(t, os.MkdirAll(filepath.Join(tmpl, "commands"), 0o755))
	must(t, os.WriteFile(filepath.Join(tmpl, "settings.json"), []byte(`{"a":1}`+"\n# state 1\n"), 0o600))
	must(t, os.WriteFile(filepath.Join(tmpl, "commands", "x.md"), []byte("x"), 0o644))
	must(t, os.WriteFile(filepath.Join(tmpl, "auth.json"), []byte("secret"), 0o600))
	// State that must never be copied.
	must(t, os.MkdirAll(filepath.Join(tmpl, "projects", "p"), 0o755))
	must(t, os.WriteFile(filepath.Join(tmpl, "projects", "p", "t.jsonl"), []byte("{}"), 0o644))
	return Spec{
		Kind: "k", AgentID: "a1", Env: "K_HOME",
		Template: tmpl, Dir: filepath.Join(root, "agent", ".amux", "k"),
		Entries: []Entry{
			{Rel: "settings.json", Normalize: stripState},
			{Rel: "commands"},
		},
		Shared: []string{"auth.json"},
	}
}

// stripState is the test Normalize: it drops the "# state …" trailer (the
// harness's own churn) and the "# seeded" header a Seed transform may add, so
// both sides compare on the configuration proper.
func stripState(_ Spec, b []byte) []byte {
	b = bytes.TrimPrefix(b, []byte("# seeded\n"))
	if i := bytes.Index(b, []byte("# state")); i >= 0 {
		return b[:i]
	}
	return b
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	must(t, os.MkdirAll(filepath.Dir(path), 0o755))
	must(t, os.WriteFile(path, []byte(content), 0o644))
}

func statuses(t *testing.T, sp Spec) map[string]Status {
	t.Helper()
	changes, err := Scan(sp)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]Status{}
	for _, c := range changes {
		out[c.Rel] = c.Status
	}
	return out
}

// Seed copies exactly the config entries — not state — links the shared auth
// file rather than copying it, preserves file modes, and is idempotent: a second
// Seed leaves the agent's edits alone.
func TestSeedCopiesConfigLinksAuthSkipsState(t *testing.T) {
	sp := spec(t)
	fresh, err := Seed(sp)
	if err != nil || !fresh {
		t.Fatalf("Seed = %v, %v; want fresh", fresh, err)
	}
	if b, _ := os.ReadFile(filepath.Join(sp.Dir, "settings.json")); !bytes.Contains(b, []byte(`{"a":1}`)) {
		t.Fatalf("settings.json not seeded: %q", b)
	}
	if fi, err := os.Stat(filepath.Join(sp.Dir, "settings.json")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("settings.json mode not preserved: %v %v", fi, err)
	}
	if _, err := os.Stat(filepath.Join(sp.Dir, "commands", "x.md")); err != nil {
		t.Fatal("commands tree not seeded")
	}
	if _, err := os.Stat(filepath.Join(sp.Dir, "projects")); !os.IsNotExist(err) {
		t.Fatal("state (projects/) must not be copied")
	}
	link, err := os.Readlink(filepath.Join(sp.Dir, "auth.json"))
	if err != nil || link != filepath.Join(sp.Template, "auth.json") {
		t.Fatalf("auth.json should be a symlink to the template's, got %q %v", link, err)
	}
	if got := sp.EnvEntry(); got != "K_HOME="+sp.Dir {
		t.Fatalf("EnvEntry = %q", got)
	}
	// Binds expose only the shared file, at its template path.
	binds := Binds(sp)
	if len(binds) != 1 || binds[0][1] != filepath.Join(sp.Template, "auth.json") || binds[0][2] != binds[0][1] {
		t.Fatalf("Binds = %v, want the auth file bound at its own path", binds)
	}
	// Idempotent: an edit survives a re-seed.
	write(t, filepath.Join(sp.Dir, "settings.json"), `{"a":2}`)
	if fresh, err := Seed(sp); err != nil || fresh {
		t.Fatalf("second Seed = %v, %v; want not fresh", fresh, err)
	}
	if b, _ := os.ReadFile(filepath.Join(sp.Dir, "settings.json")); string(b) != `{"a":2}` {
		t.Fatalf("re-seed clobbered the agent's edit: %q", b)
	}
	if len(statuses(t, sp)) == 0 {
		t.Fatal("the edit should register as drift")
	}
}

// Scan attributes each divergence: the agent's edits, the template's, both, and
// a copy that matches the template (converged) settles the baseline.
func TestScanClassifies(t *testing.T) {
	sp := spec(t)
	if _, err := Seed(sp); err != nil {
		t.Fatal(err)
	}
	if got := statuses(t, sp); len(got) != 0 {
		t.Fatalf("fresh copy should have no drift, got %v", got)
	}

	// Harness churn in the normalized trailer is not an edit.
	write(t, filepath.Join(sp.Dir, "settings.json"), `{"a":1}`+"\n# state 999\n")
	if got := statuses(t, sp); len(got) != 0 {
		t.Fatalf("normalized churn must not register, got %v", got)
	}

	write(t, filepath.Join(sp.Dir, "settings.json"), `{"a":2}`)       // agent edit
	write(t, filepath.Join(sp.Dir, "commands", "new.md"), "n")        // agent add
	must(t, os.Remove(filepath.Join(sp.Dir, "commands", "x.md")))     // agent remove
	write(t, filepath.Join(sp.Template, "commands", "tpl.md"), "tpl") // template add
	must(t, os.Remove(filepath.Join(sp.Dir, "auth.json")))            // detach the shared link…
	write(t, filepath.Join(sp.Dir, "auth.json"), "rotated")           // …into a private copy
	got := statuses(t, sp)
	want := map[string]Status{
		"settings.json":   AgentChanged,
		"commands/new.md": AgentAdded,
		"commands/x.md":   AgentRemoved,
		"commands/tpl.md": TemplateChanged,
		"auth.json":       SharedDetached,
	}
	for rel, st := range want {
		if got[rel] != st {
			t.Errorf("%s = %q, want %q (all: %v)", rel, got[rel], st, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("extra changes: %v", got)
	}
	if n := Pending(mustScan(t, sp)); n != 4 {
		t.Errorf("Pending = %d, want 4 (template-changed is not the agent's)", n)
	}

	// Template moves the same file the agent edited, differently: conflict.
	write(t, filepath.Join(sp.Template, "settings.json"), `{"a":3}`)
	if got := statuses(t, sp)["settings.json"]; got != Conflict {
		t.Errorf("both-sides edit = %q, want conflict", got)
	}
	// The user then adopts the agent's value by hand: converged, and settled.
	write(t, filepath.Join(sp.Template, "settings.json"), `{"a":2}`)
	if got := statuses(t, sp)["settings.json"]; got != "" {
		t.Errorf("converged file still reported: %q", got)
	}
	write(t, filepath.Join(sp.Template, "settings.json"), `{"a":4}`)
	if got := statuses(t, sp)["settings.json"]; got != TemplateChanged {
		t.Errorf("after convergence a template edit = %q, want template-changed", got)
	}
}

func mustScan(t *testing.T, sp Spec) []Change {
	t.Helper()
	c, err := Scan(sp)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Promote propagates the agent's version into the template (honoring Merge);
// Reset re-copies the template's (honoring Seed) — and both settle the path.
func TestPromoteAndReset(t *testing.T) {
	sp := spec(t)
	sp.Entries[0].Seed = func(_ Spec, b []byte) []byte { return append([]byte("# seeded\n"), b...) }
	sp.Entries[0].Merge = func(_ Spec, tmpl, copy []byte) ([]byte, error) {
		return append(append([]byte{}, copy...), []byte("# state kept\n")...), nil
	}
	if _, err := Seed(sp); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(sp.Dir, "settings.json")); !bytes.HasPrefix(b, []byte("# seeded\n")) {
		t.Fatalf("Seed transform not applied: %q", b)
	}
	if len(statuses(t, sp)) != 0 {
		t.Fatal("the seed transform must not read as drift")
	}

	write(t, filepath.Join(sp.Dir, "settings.json"), `{"a":9}`)
	write(t, filepath.Join(sp.Dir, "commands", "new.md"), "n")
	must(t, Promote(sp, "settings.json"))
	must(t, Promote(sp, "commands/new.md"))
	if b, _ := os.ReadFile(filepath.Join(sp.Template, "settings.json")); string(b) != `{"a":9}`+"# state kept\n" {
		t.Fatalf("Promote should merge into the template: %q", b)
	}
	if _, err := os.Stat(filepath.Join(sp.Template, "commands", "new.md")); err != nil {
		t.Fatal("Promote should add the new file to the template")
	}
	if got := statuses(t, sp); len(got) != 0 {
		t.Fatalf("promoted paths should be settled, got %v", got)
	}

	// Reset a stale copy from the template and drop an agent-added file.
	write(t, filepath.Join(sp.Template, "commands", "x.md"), "x2")
	write(t, filepath.Join(sp.Dir, "commands", "mine.md"), "m")
	must(t, Reset(sp, "commands/x.md"))
	must(t, Reset(sp, "commands/mine.md"))
	if b, _ := os.ReadFile(filepath.Join(sp.Dir, "commands", "x.md")); string(b) != "x2" {
		t.Fatalf("Reset should copy the template's version: %q", b)
	}
	if _, err := os.Stat(filepath.Join(sp.Dir, "commands", "mine.md")); !os.IsNotExist(err) {
		t.Fatal("Reset of a file the template lacks should remove it")
	}
	if got := statuses(t, sp); len(got) != 0 {
		t.Fatalf("reset paths should be settled, got %v", got)
	}

	// Reset re-links a detached credential; Promote refuses to touch it.
	must(t, os.Remove(filepath.Join(sp.Dir, "auth.json")))
	write(t, filepath.Join(sp.Dir, "auth.json"), "rotated")
	if err := Promote(sp, "auth.json"); err == nil {
		t.Fatal("Promote of shared auth must be refused")
	}
	must(t, Reset(sp, "auth.json"))
	if _, err := os.Readlink(filepath.Join(sp.Dir, "auth.json")); err != nil {
		t.Fatal("Reset should re-link the shared file")
	}
	// Paths outside the template are refused.
	for _, bad := range []string{"../x", "/etc/passwd", "projects/p/t.jsonl", ""} {
		if err := Promote(sp, bad); err == nil {
			t.Errorf("Promote(%q) should be refused", bad)
		}
	}
}

// A copy that predates its manifest (or whose state dir was wiped) adopts its
// current content as the baseline instead of reporting every file as an edit.
func TestSeedAdoptsBaselineWhenManifestMissing(t *testing.T) {
	sp := spec(t)
	write(t, filepath.Join(sp.Dir, "settings.json"), `{"a":7}`)
	fresh, err := Seed(sp)
	if err != nil || fresh {
		t.Fatalf("Seed over an existing copy = %v, %v", fresh, err)
	}
	// With no record of the seed, differences are attributed to the template:
	// amux must not suggest promoting unknown content into the user's config.
	got := statuses(t, sp)
	for rel, st := range got {
		if st != TemplateChanged {
			t.Errorf("%s = %q; an adopted baseline attributes differences to the template", rel, st)
		}
	}
	if Pending(mustScan(t, sp)) != 0 {
		t.Fatalf("adopted baseline must not report pending agent edits, got %v", got)
	}
	Forget(sp)
	if _, err := os.Stat(manifestPath(sp)); !os.IsNotExist(err) {
		t.Fatal("Forget should remove the manifest")
	}
}

func TestSeedBackfillsMissingSharedAuth(t *testing.T) {
	for _, baseline := range []bool{false, true} {
		t.Run(map[bool]string{false: "without-manifest", true: "with-manifest"}[baseline], func(t *testing.T) {
			sp := spec(t)
			if baseline {
				_, err := Seed(sp)
				must(t, err)
			}
			write(t, filepath.Join(sp.Dir, "settings.json"), `{"agent":"edit"}`)
			// An auth entry added by an amux upgrade, or a login made later.
			sp.Shared = append(sp.Shared, ".credentials.json")
			sp.HardlinkShared = []string{".credentials.json"}
			if fresh, err := Seed(sp); err != nil || fresh {
				t.Fatalf("Seed before login = %v, %v", fresh, err)
			}
			if _, err := os.Lstat(filepath.Join(sp.Dir, ".credentials.json")); !os.IsNotExist(err) {
				t.Fatal("missing host login must not create credentials")
			}
			write(t, filepath.Join(sp.Template, ".credentials.json"), "test-token")
			if fresh, err := Seed(sp); err != nil || fresh {
				t.Fatalf("Seed after login = %v, %v", fresh, err)
			}
			local, err := os.Lstat(filepath.Join(sp.Dir, ".credentials.json"))
			must(t, err)
			source, err := os.Stat(filepath.Join(sp.Template, ".credentials.json"))
			must(t, err)
			if !os.SameFile(local, source) {
				t.Fatal("existing home did not inherit the shared credential")
			}
			if b, _ := os.ReadFile(filepath.Join(sp.Dir, "settings.json")); string(b) != `{"agent":"edit"}` {
				t.Fatal("backfill overwrote agent configuration")
			}
		})
	}
}

func TestSeedPreservesDetachedSharedAuth(t *testing.T) {
	sp := spec(t)
	sp.HardlinkShared = []string{"auth.json"}
	_, err := Seed(sp)
	must(t, err)
	path := filepath.Join(sp.Dir, "auth.json")
	must(t, os.Remove(path))
	write(t, path, "private-token")
	_, err = Seed(sp)
	must(t, err)
	if b, _ := os.ReadFile(path); string(b) != "private-token" {
		t.Fatal("Seed replaced a detached credential")
	}
	if got := statuses(t, sp)["auth.json"]; got != SharedDetached {
		t.Fatalf("detached hard link status = %q", got)
	}
	must(t, Reset(sp, "auth.json"))
	if got := statuses(t, sp); len(got) != 0 {
		t.Fatalf("Reset did not restore shared auth: %v", got)
	}
	// A dangling symlink is existing state too, not a missing entry to replace.
	must(t, os.Remove(path))
	must(t, os.Symlink(filepath.Join(sp.Dir, "absent"), path))
	_, err = Seed(sp)
	must(t, err)
	if target, err := os.Readlink(path); err != nil || target != filepath.Join(sp.Dir, "absent") {
		t.Fatalf("Seed replaced dangling link: %q, %v", target, err)
	}
}

func TestBindsOverlayExistingSharedLockDirectory(t *testing.T) {
	sp := spec(t)
	sp.Shared = append(sp.Shared, "locks")
	write(t, filepath.Join(sp.Template, "locks", "refresh.lock"), "")
	write(t, filepath.Join(sp.Dir, "locks", "local.lock"), "")
	_, err := Seed(sp)
	must(t, err)
	for _, bind := range Binds(sp) {
		if bind[1] == filepath.Join(sp.Template, "locks") && bind[2] == filepath.Join(sp.Dir, "locks") {
			return
		}
	}
	t.Fatal("existing lock directory must be overlaid with shared locks")
}
