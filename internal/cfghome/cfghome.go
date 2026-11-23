// Package cfghome templates a harness's user-level configuration into an agent's
// sandbox and feeds the agent's edits back to amux.
//
// The model: the user's own config dir (~/.claude, $CODEX_HOME) is a TEMPLATE.
// Each agent gets a private COPY of it under its sandbox dir, and the harness is
// pointed at the copy (CLAUDE_CONFIG_DIR, CODEX_HOME). Nothing from the user's
// home is mounted into the sandbox any more except the one thing that must stay
// shared — authentication. A harness can use a dedicated shared credential
// directory or links back to the template, bound read-write in the sandbox.
// The agent may edit its config copy freely: that is its own configuration.
//
// Because the copy's initial content is recorded in a manifest that lives
// OUTSIDE the sandbox (under amux's state dir, which is never bound in), amux
// can later tell exactly what the agent changed, what the user changed in the
// template since, and where the two disagree. Scan reports that; Promote and
// Reset act on it (propagate an agent's edit into the template, or discard it).
// The daemon surfaces pending edits on the rail, `amux sandbox drift` lists
// them, so a change an agent made to its own config never propagates silently
// and never gets lost silently either.
package cfghome

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"amux/internal/core"
	"amux/internal/hostprep"
)

// Spec describes how one harness's configuration is templated into one agent's
// sandbox. Harnesses build it (agent.Harness.Config); this package executes it.
type Spec struct {
	Kind    string // harness kind (claude, codex) — labels the manifest and messages
	AgentID string // the agent whose copy this is; keys the seed manifest
	// Env is the environment variable the harness reads its config dir from
	// (CLAUDE_CONFIG_DIR, CODEX_HOME). Set to Dir for every pane of the agent.
	Env string
	// Template is the user's system-level config dir — the source of the copy.
	Template string
	// Dir is the agent's private copy, under its sandbox dir so it is writable
	// inside the scope without a mount of its own and dies with the agent.
	Dir string
	// Root is the authoritative session directory containing Dir. Host-side
	// reads and writes of the private copy are descriptor-anchored here; Template
	// is a separate trusted source and is never treated as a destination link.
	Root string
	// Entries are the template's configuration paths (relative to both Template
	// and Dir): what a fresh copy is seeded with and what Scan watches. A
	// directory entry covers its whole tree. Per-machine state — transcripts,
	// caches, history — is deliberately not an entry: it stays private to each
	// home and is never compared.
	Entries []Entry
	// Shared are auth files and lock directories (relative to Template) the copy
	// must not own: the copy links to the template and the sandbox binds the path
	// back at its template path (Binds), so every agent — and the user — refresh
	// the same token instead of each holding a copy that rotation would strand.
	Shared []string
	// HardlinkShared is the subset of Shared requiring a regular file, because
	// the harness refuses symlinks when refreshing it. These use hard links and
	// require the template and copy to reside on the same filesystem.
	HardlinkShared []string
	// AuthDir is an optional dedicated credential directory. Bind the directory,
	// not its files: credential writers atomically replace files and create locks.
	// AuthEnv tells the harness to use it independently of its private config.
	AuthDir string
	AuthEnv string
	// AuthValue can select an OS credential store independently of a filesystem
	// grant. In particular, an empty Claude store selector means the default
	// macOS Keychain entry, even with a private CLAUDE_CONFIG_DIR.
	AuthValue string
	// AuthUnsetEnv clears inherited credential overrides when the user opts in
	// to this store. Otherwise an old environment token could shadow a new login.
	AuthUnsetEnv []string
	// Retire are formerly Shared paths whose link in an existing copy must go:
	// a symlink there is removed without being followed, so nothing is written
	// through to the template. A regular file (a detached private credential)
	// is never deleted.
	Retire []string
}

// Entry is one configuration path of a template.
type Entry struct {
	Rel string // path relative to the template dir (and to the copy)
	// Src, when set, is the template-side absolute path when it is not simply
	// Template/Rel — Claude's default layout keeps .claude.json beside ~/.claude.
	Src string
	// Seed, when set, transforms the template's bytes as they are copied (e.g.
	// drop per-project trust entries, rewrite absolute template paths to the
	// copy). Only meaningful for a file entry.
	Seed func(spec Spec, b []byte) []byte
	// Normalize, when set, projects a file's bytes onto the configuration that
	// matters before hashing or comparing, so the harness's own churn in the same
	// file (counters, caches, trust entries it writes at launch) never reads as
	// an edit. Both sides are normalized the same way. Only for a file entry.
	Normalize func(spec Spec, b []byte) []byte
	// Merge, when set, folds a promoted copy into the template's current file
	// instead of overwriting it wholesale — for a file the template holds state
	// in that must survive (Claude's .claude.json). Only for a file entry.
	Merge func(spec Spec, template, copy []byte) ([]byte, error)
}

// Status classifies one path's divergence between the agent's copy and the
// template, relative to what the copy was seeded with.
type Status string

const (
	// AgentChanged: the agent edited its copy; the template still has the seeded
	// version. The candidate to propagate (Promote) or discard (Reset).
	AgentChanged Status = "agent-changed"
	// AgentAdded: the agent created a file the template does not have.
	AgentAdded Status = "agent-added"
	// AgentRemoved: the agent deleted a file the template still has.
	AgentRemoved Status = "agent-removed"
	// TemplateChanged: the user changed the template since the copy was seeded;
	// the agent's copy is stale. Reset pulls the new version in.
	TemplateChanged Status = "template-changed"
	// Conflict: both sides changed the same path differently since the seed.
	Conflict Status = "conflict"
	// SharedDetached: an auth file no longer has the expected link to the
	// template, so the agent holds a credential copy that rotation may strand.
	SharedDetached Status = "shared-detached"
)

// Change is one path Scan found diverged.
type Change struct {
	Kind   string `json:"kind"`   // harness kind
	Rel    string `json:"path"`   // template-relative path
	Status Status `json:"status"` // see Status
	// Copy and Template are the absolute paths of the two versions, for a user
	// who wants to diff them by hand ("" when that side has no file).
	Copy     string `json:"copy,omitempty"`
	Template string `json:"template,omitempty"`
}

// String renders a change as one human-readable line.
func (c Change) String() string { return fmt.Sprintf("%-16s %s", c.Status, c.Rel) }

// manifest records what a copy was seeded with: the normalized content hash of
// every file, so a later Scan can attribute a difference to the agent, the
// template, or both. Kept under the state dir — never inside the sandbox — so
// the agent cannot rewrite its own baseline.
type manifest struct {
	Kind     string            `json:"kind"`
	Template string            `json:"template"`
	Dir      string            `json:"dir"`
	Seeded   int64             `json:"seeded"` // unix millis
	Files    map[string]string `json:"files"`  // rel -> sha256 of normalized content
}

// StateDir holds the per-agent seed manifests.
func StateDir() string { return filepath.Join(core.StateDir(), "cfghome") }

func manifestPath(sp Spec) string {
	return filepath.Join(StateDir(), sanitize(sp.AgentID)+"."+sanitize(sp.Kind)+".json")
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, s)
}

type destination struct {
	root  *hostprep.Root
	home  string
	owned bool
}

func openDestination(sp Spec) (*destination, error) {
	if sp.Root == "" || sp.Dir == "" {
		return nil, errors.New("cfghome: private destination needs Root and Dir")
	}
	r, err := hostprep.OpenSession(sp.Root)
	if err != nil {
		return nil, err
	}
	rel, err := r.Rel(sp.Dir)
	if err != nil || rel == "." {
		_ = r.Close()
		if err != nil {
			return nil, err
		}
		return nil, errors.New("cfghome: private home must be below session root")
	}
	return &destination{root: r, home: rel, owned: true}, nil
}

func destinationAt(root *hostprep.Root, sp Spec) (*destination, error) {
	rootName, err := filepath.Abs(sp.Root)
	if err != nil {
		return nil, err
	}
	if filepath.Clean(rootName) != filepath.Clean(root.Name()) {
		return nil, hostprep.ErrUnsafe
	}
	rel, err := root.Rel(sp.Dir)
	if err != nil || rel == "." {
		if err != nil {
			return nil, err
		}
		return nil, errors.New("cfghome: private home must be below session root")
	}
	return &destination{root: root, home: rel}, nil
}

func (d *destination) close() {
	if d.owned {
		_ = d.root.Close()
	}
}
func (d *destination) path(rel string) string {
	if rel == "" || rel == "." {
		return d.home
	}
	return filepath.Join(d.home, rel)
}
func (d *destination) seeded() (bool, error) {
	sub, err := d.root.Sub(d.home, false, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	_ = sub.Close()
	return true, nil
}
func (d *destination) mkdir(rel string) error {
	sub, err := d.root.Sub(d.path(rel), true, 0o755)
	if err == nil {
		_ = sub.Close()
	}
	return err
}
func (d *destination) lstat(rel string) (fs.FileInfo, error) {
	return d.root.Lstat(d.path(rel))
}
func (d *destination) read(rel string) ([]byte, error) {
	return d.root.ReadFile(d.path(rel))
}
func (d *destination) write(rel string, b []byte, mode fs.FileMode) error {
	return d.root.AtomicWrite(d.path(rel), b, mode)
}
func (d *destination) remove(rel string) error {
	return d.root.Remove(d.path(rel))
}
func (d *destination) symlink(rel, source string) error {
	return d.root.ReplaceSymlink(d.path(rel), source)
}
func (d *destination) hardlink(rel, source string) error {
	return d.root.ReplaceHardlink(d.path(rel), source)
}

// EnvEntry is the KEY=VALUE that points the harness at the agent's copy.
func (sp Spec) EnvEntry() string { return sp.Env + "=" + sp.Dir }

// EnvEntries routes configuration and, when configured, shared authentication.
func (sp Spec) EnvEntries() []string {
	env := []string{sp.EnvEntry()}
	if sp.AuthEnv != "" {
		env = append(env, sp.AuthEnv+"="+sp.AuthValue)
	}
	for _, key := range sp.AuthUnsetEnv {
		env = append(env, key+"=")
	}
	return env
}

// Seeded reports whether the agent's copy exists.
func Seeded(sp Spec) bool {
	d, err := openDestination(sp)
	if err != nil {
		return false
	}
	defer d.close()
	ok, _ := d.seeded()
	return ok
}

// Seed creates the agent's private copy from the template if it does not exist
// yet, and records the manifest. It is idempotent: an existing copy is left
// as the agent has it (its edits are the point), except that missing shared
// auth links are filled in. A missing manifest is rebuilt from the copy's
// current content. Returns whether a fresh copy was made.
// A template that is missing entirely still yields an (empty)
// copy, so a harness never falls back to the user's home by accident.
func Seed(sp Spec) (fresh bool, err error) {
	if sp.Dir == "" || sp.Template == "" || sp.Root == "" {
		return false, errors.New("cfghome: spec needs Root, Dir, and Template")
	}
	d, err := openDestination(sp)
	if err != nil {
		return false, err
	}
	defer d.close()
	return seedDestination(d, sp)
}

// SeedRooted performs Seed using a session root the launch caller already
// pinned, so replacing the stored session pathname between preparation steps
// cannot redirect this transaction.
func SeedRooted(root *hostprep.Root, sp Spec) (fresh bool, err error) {
	if sp.Dir == "" || sp.Template == "" || sp.Root == "" {
		return false, errors.New("cfghome: spec needs Root, Dir, and Template")
	}
	d, err := destinationAt(root, sp)
	if err != nil {
		return false, err
	}
	return seedDestination(d, sp)
}

func seedDestination(d *destination, sp Spec) (fresh bool, err error) {
	templateMu.RLock()
	defer templateMu.RUnlock()
	defer lockAgent(sp)()
	seeded, err := d.seeded()
	if err != nil {
		return false, err
	}
	if seeded {
		// New shared entries and logins made after this home was seeded should
		// become available on the next launch. Preserve detached credentials and
		// existing links, including dangling ones; Reset is the explicit repair.
		for _, rel := range sp.Shared {
			if _, err := d.lstat(rel); errors.Is(err, fs.ErrNotExist) {
				if err := linkShared(d, sp, rel); err != nil {
					return false, err
				}
			} else if err != nil {
				return false, err
			}
		}
		if err := retireLinks(d, sp); err != nil {
			return false, err
		}
		if _, err := readManifest(sp); err == nil {
			return false, nil
		}
		// The copy exists but its baseline is gone (pre-manifest copy, or a wiped
		// state dir): adopt the copy's current content as the baseline rather than
		// reporting every file as an edit.
		snapshot, err := snapshotDestination(d, sp, nil)
		if err != nil {
			return false, err
		}
		return false, writeManifest(sp, snapshot)
	}
	if err := d.mkdir(""); err != nil {
		return false, err
	}
	for _, e := range sp.Entries {
		if err := seedEntry(d, sp, e); err != nil {
			return false, err
		}
	}
	for _, rel := range sp.Shared {
		if err := linkShared(d, sp, rel); err != nil {
			return false, err
		}
	}
	snapshot, err := snapshotDestination(d, sp, nil)
	if err != nil {
		return false, err
	}
	return true, writeManifest(sp, snapshot)
}

// seedEntry copies one entry (file or tree) from the template into the copy,
// applying the entry's Seed transform to file content. A missing template path
// is simply absent from the copy.
func seedEntry(dst *destination, sp Spec, e Entry) error {
	src := e.src(sp)
	fi, err := os.Stat(src)
	if err != nil {
		return nil
	}
	if !fi.IsDir() {
		b, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		if e.Seed != nil {
			b = e.Seed(sp, b)
		}
		return dst.write(e.Rel, b, fi.Mode().Perm())
	}
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip what we can't read; the rest still seeds
		}
		if skipDir(d) {
			return fs.SkipDir
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(e.Rel, rel)
		if d.IsDir() {
			return dst.mkdir(target)
		}
		if !d.Type().IsRegular() {
			return nil // symlinks and specials aren't config we template
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		info, _ := d.Info()
		mode := fs.FileMode(0o644)
		if info != nil {
			mode = info.Mode().Perm()
		}
		return dst.write(target, b, mode)
	})
}

// skipDir names subtrees never templated or compared: VCS internals (a plugin
// marketplace is a git clone whose objects churn on every refresh).
func skipDir(d fs.DirEntry) bool { return d.IsDir() && d.Name() == ".git" }

// linkShared points the copy's rel at the template's file, so the harness reads
// and refreshes the one shared credential. Nothing is linked when the template
// has no such file (the user hasn't logged in on this machine).
func linkShared(dst *destination, sp Spec, rel string) error {
	src := filepath.Join(sp.Template, rel)
	if _, err := os.Lstat(src); err != nil {
		return nil
	}
	if hardlinkShared(sp, rel) {
		if err := dst.hardlink(rel, src); err != nil {
			return fmt.Errorf("sharing %s requires a hard link (template and agent must be on the same filesystem): %w", rel, err)
		}
		return nil
	}
	return dst.symlink(rel, src)
}

// retireLinks unlinks retired symlinks in the copy. Lstat never follows the
// link and Remove deletes only the link itself, not the file it names.
func retireLinks(dst *destination, sp Spec) error {
	for _, rel := range sp.Retire {
		fi, err := dst.lstat(rel)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if fi.Mode()&fs.ModeSymlink == 0 {
			continue
		}
		if err := dst.remove(rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("retire shared %s link: %w", rel, err)
		}
	}
	return nil
}

// Binds are the bubblewrap arguments that make the shared (auth) files reachable
// inside the scope: each is bound read-write at its own template path, which is
// exactly where the copy's symlink points. Everything else the harness needs is
// already inside the agent's dir, so these are the only binds a templated config
// requires — down from the whole config tree.
func Binds(sp Spec) [][]string {
	var out [][]string
	if sp.AuthDir != "" {
		// Required, not --bind-try: silently missing auth would fall back to a
		// stale private credential and recreate the split refresh chain.
		out = append(out, []string{"--bind", sp.AuthDir, sp.AuthDir})
	}
	for _, rel := range sp.Shared {
		p := filepath.Join(sp.Template, rel)
		if fi, err := os.Stat(p); err == nil {
			out = append(out, []string{"--bind-try", p, p})
			// Existing homes can already have a private lock directory. Overlay
			// it with the shared one in the sandbox so token refreshes coordinate
			// with the user and other agents, without deleting local state.
			if fi.IsDir() {
				dst := filepath.Join(sp.Dir, rel)
				if local, err := os.Lstat(dst); err == nil && local.IsDir() {
					out = append(out, []string{"--bind-try", p, dst})
				}
			}
		}
	}
	return out
}

// Scan compares the agent's copy with the template against the seed manifest
// and returns every diverged path, sorted. Where both sides now agree (the
// agent's edit was promoted, or the user made the same change) the manifest is
// advanced to that content so the path drops out of future scans; otherwise the
// manifest is untouched. An unseeded copy reports nothing.
//
// Unhooked copy files whose stamp still matches the agent's stat sidecar are
// not re-read (see fileStamp.reusable); everything else is read and hashed.
// To scan many agents, use ScanAll or a ScanCache, which share one template
// snapshot per run instead of re-hashing the template for every agent.
func Scan(sp Spec) ([]Change, error) { return scan(nil, sp) }

func scan(cache *ScanCache, sp Spec) ([]Change, error) {
	templateMu.RLock()
	defer templateMu.RUnlock()
	defer lockAgent(sp)()
	d, err := openDestination(sp)
	if err != nil {
		return nil, err
	}
	defer d.close()
	seeded, err := d.seeded()
	if err != nil {
		return nil, err
	}
	if !seeded {
		return nil, nil
	}
	m, err := readManifest(sp)
	if err != nil {
		return nil, err
	}
	sc := loadStatCache(sp)
	copyH, err := snapshotDestination(d, sp, sc)
	if err != nil {
		return nil, err
	}
	_ = sc.save(sp) // best effort: a lost sidecar only costs a re-read
	tmplH := cache.template(sp).hashesFor(sp)

	rels := map[string]bool{}
	for r := range m.Files {
		rels[r] = true
	}
	for r := range copyH {
		rels[r] = true
	}
	for r := range tmplH {
		rels[r] = true
	}

	var out []Change
	converged := false
	for rel := range rels {
		if isShared(sp, rel) {
			continue // auth: compared by link state below, never by content
		}
		base, hasBase := m.Files[rel]
		c, inCopy := copyH[rel]
		t, inTmpl := tmplH[rel]
		if inCopy == inTmpl && c == t {
			if !hasBase || base != c {
				// Both sides agree on something other than the seed: the change has
				// propagated (or never mattered). Advance the baseline — but only
				// if the live template still agrees: the template side may come
				// from a run-scoped snapshot the user has since edited, and a stale
				// agreement must not be persisted.
				if live, ok := liveTemplateHash(sp, rel); ok != inTmpl || live != t {
					continue
				}
				if inCopy {
					m.Files[rel] = c
				} else {
					delete(m.Files, rel)
				}
				converged = true
			}
			continue
		}
		ch := Change{Kind: sp.Kind, Rel: rel}
		if inCopy {
			ch.Copy = filepath.Join(sp.Dir, rel)
		}
		if inTmpl {
			ch.Template = templatePath(sp, rel)
		}
		switch {
		case !hasBase:
			switch {
			case inCopy && !inTmpl:
				ch.Status = AgentAdded
			case !inCopy && inTmpl:
				ch.Status = TemplateChanged // new in the template since the seed
			default:
				ch.Status = Conflict // no baseline to attribute the difference
			}
		case inCopy && c == base:
			ch.Status = TemplateChanged
		case inTmpl && t == base:
			if inCopy {
				ch.Status = AgentChanged
			} else {
				ch.Status = AgentRemoved
			}
		default:
			ch.Status = Conflict
		}
		out = append(out, ch)
	}
	for _, rel := range sp.Shared {
		p := filepath.Join(sp.Dir, rel)
		fi, err := d.lstat(rel)
		if err != nil {
			continue
		}
		src, srcErr := os.Stat(filepath.Join(sp.Template, rel))
		if hardlinkShared(sp, rel) {
			if fi.Mode().IsRegular() && srcErr == nil && os.SameFile(fi, src) {
				continue
			}
		} else if fi.Mode()&os.ModeSymlink != 0 || (fi.IsDir() && srcErr == nil && src.IsDir()) {
			continue // shared lock directories are overlaid by Binds
		}
		out = append(out, Change{Kind: sp.Kind, Rel: rel, Status: SharedDetached,
			Copy: p, Template: filepath.Join(sp.Template, rel)})
	}
	if converged {
		_ = writeManifest(sp, m.Files)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Rel < out[j].Rel })
	return out, nil
}

// Promote propagates the agent's version of rel into the template — the "yes,
// this change should apply to every agent (and to me)" verb. A file the agent
// removed is removed from the template; a file with a Merge hook is folded into
// the template's current content rather than overwriting it. The manifest is
// advanced so the path is settled.
func Promote(sp Spec, rel string) error {
	rel = filepath.Clean(rel)
	if !covered(sp, rel) {
		return fmt.Errorf("%s is not a templated %s config path", rel, sp.Kind)
	}
	if isShared(sp, rel) {
		return fmt.Errorf("%s is shared auth, not config — nothing to promote", rel)
	}
	templateMu.Lock() // writes the template: exclude every scan and seed
	defer templateMu.Unlock()
	defer lockAgent(sp)()
	d, err := openDestination(sp)
	if err != nil {
		return err
	}
	defer d.close()
	e := entryFor(sp, rel)
	dst := templatePath(sp, rel)
	b, err := d.read(rel)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.Remove(dst); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	case err != nil:
		return err
	default:
		if e.Merge != nil && e.Rel == rel {
			cur, _ := os.ReadFile(dst)
			if b, err = e.Merge(sp, cur, b); err != nil {
				return err
			}
		}
		mode := fs.FileMode(0o644)
		if fi, err := os.Stat(dst); err == nil {
			mode = fi.Mode().Perm()
		}
		if err := writeFile(dst, b, mode); err != nil {
			return err
		}
	}
	return settleDestination(d, sp, rel)
}

// Reset discards the agent's version of rel and re-copies the template's — the
// "no, keep this agent on the shared config" verb, and also how a stale copy
// picks up a template change. A path the template lacks is removed from the
// copy. The manifest is advanced so the path is settled.
func Reset(sp Spec, rel string) error {
	rel = filepath.Clean(rel)
	if !covered(sp, rel) {
		return fmt.Errorf("%s is not a templated %s config path", rel, sp.Kind)
	}
	templateMu.Lock() // as Promote: one writer at a time, never beside a scan
	defer templateMu.Unlock()
	defer lockAgent(sp)()
	d, err := openDestination(sp)
	if err != nil {
		return err
	}
	defer d.close()
	if isShared(sp, rel) {
		return linkShared(d, sp, rel) // re-point a detached credential at the template
	}
	e := entryFor(sp, rel)
	src := templatePath(sp, rel)
	b, err := os.ReadFile(src)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := d.remove(rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	case err != nil:
		return err
	default:
		if e.Seed != nil && e.Rel == rel {
			b = e.Seed(sp, b)
		}
		mode := fs.FileMode(0o644)
		if fi, err := os.Stat(src); err == nil {
			mode = fi.Mode().Perm()
		}
		if err := d.write(rel, b, mode); err != nil {
			return err
		}
	}
	return settleDestination(d, sp, rel)
}

func settleDestination(d *destination, sp Spec, rel string) error {
	m, err := readManifest(sp)
	if err != nil {
		m = &manifest{Files: map[string]string{}}
	}
	if h, ok, err := hashDestination(d, sp, rel); err != nil {
		return err
	} else if ok {
		m.Files[rel] = h
	} else {
		delete(m.Files, rel)
	}
	return writeManifest(sp, m.Files)
}

// Forget drops the agent's manifest — for when the agent (and with it the copy)
// is deleted.
func Forget(sp Spec) {
	defer lockAgent(sp)()
	_ = os.Remove(manifestPath(sp))
	_ = os.Remove(statCachePath(sp))
}

// ---- summary (the rail's view) ------------------------------------------------

// summaryTTL bounds how often the daemon's poll re-scans an agent's copy. A scan
// stats and hashes every templated file, so it is throttled well below the
// 2-second poll; edits still surface within a few seconds.
const summaryTTL = 10 * time.Second

type summaryEntry struct {
	at      time.Time
	changes []Change
}

var (
	summaryMu  sync.Mutex // leaf lock: never held across a scan
	summaries  = map[string]summaryEntry{}
	summaryGen = map[string]uint64{} // bumped by Invalidate
)

// Summary is Scan for the daemon's poll path: cached per copy for summaryTTL,
// never erroring (a scan failure reads as no changes), and logging the moment an
// agent's pending edits first appear or change, so the daemon log records what
// the agent did to its configuration even if nobody is watching the rail.
func Summary(sp Spec) []Change {
	key := sp.Dir
	summaryMu.Lock()
	if e, ok := summaries[key]; ok && time.Since(e.at) < summaryTTL {
		summaryMu.Unlock()
		return e.changes
	}
	gen := summaryGen[key]
	summaryMu.Unlock()

	// Scan outside summaryMu so one agent's scan never blocks another's
	// summary (Scan takes templateMu and the agent lock; see Lock order).
	changes, err := Scan(sp)
	if err != nil {
		changes = nil
	}

	summaryMu.Lock()
	defer summaryMu.Unlock()
	prev := summaries[key].changes
	if summaryGen[key] != gen {
		// Invalidated (a Promote/Reset) while scanning: this result may predate
		// it, so don't cache it; the next Summary rescans.
		return changes
	}
	summaries[key] = summaryEntry{at: time.Now(), changes: changes}
	if !sameChanges(prev, changes) && len(changes) > 0 {
		log.Printf("amux: agent %s edited its %s config: %s", sp.AgentID, sp.Kind, Describe(changes))
	}
	return changes
}

// Invalidate drops the cached summary for a copy so the next Summary rescans —
// called after Promote/Reset so the rail updates promptly.
func Invalidate(sp Spec) {
	summaryMu.Lock()
	delete(summaries, sp.Dir)
	summaryGen[sp.Dir]++
	summaryMu.Unlock()
}

func sameChanges(a, b []Change) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Rel != b[i].Rel || a[i].Status != b[i].Status {
			return false
		}
	}
	return true
}

// Describe renders changes as a compact one-liner ("settings.json (agent-changed), …").
func Describe(changes []Change) string {
	parts := make([]string, 0, len(changes))
	for _, c := range changes {
		parts = append(parts, fmt.Sprintf("%s (%s)", c.Rel, c.Status))
	}
	return strings.Join(parts, ", ")
}

// Pending counts the changes that need a decision from the user — an agent-side
// edit, or a conflict. A stale copy (template-changed) is the user's own doing
// and not counted as pending on the agent's behalf.
func Pending(changes []Change) int {
	n := 0
	for _, c := range changes {
		switch c.Status {
		case AgentChanged, AgentAdded, AgentRemoved, Conflict, SharedDetached:
			n++
		}
	}
	return n
}

// ---- hashing & manifests -------------------------------------------------------

// snapshotDestination hashes every templated file of the untrusted private
// copy, keyed by template-relative path. It never follows destination symlinks
// or reads multiply linked files. With a stat cache, unhooked files whose
// descriptor stamp matches their record are not read (nil: read everything).
func snapshotDestination(dst *destination, sp Spec, sc *statCache) (map[string]string, error) {
	out := map[string]string{}
	for _, e := range sp.Entries {
		fi, err := dst.lstat(e.Rel)
		if err != nil {
			if hostprep.IsUnsafe(err) {
				return nil, err
			}
			continue
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("snapshot %s: %w", e.Rel, hostprep.ErrUnsafe)
		}
		if fi.Mode().IsRegular() {
			if h, ok, err := hashDestinationCached(dst, sp, e.Rel, sc); err != nil {
				return nil, err
			} else if ok {
				out[e.Rel] = h
			}
			continue
		}
		if !fi.IsDir() {
			continue
		}
		sub, err := dst.root.Sub(dst.path(e.Rel), false, 0)
		if err != nil {
			if hostprep.IsUnsafe(err) {
				return nil, err
			}
			continue
		}
		err = sub.WalkOpen(".", func(name string, info fs.FileInfo, f *os.File) error {
			if strings.Contains(filepath.ToSlash(name), "/.git/") || filepath.Base(name) == ".git" {
				return nil
			}
			rel := filepath.Join(e.Rel, name)
			h, err := sc.hash(sp, rel, info, f)
			if err != nil {
				return err
			}
			out[rel] = h
			return nil
		})
		_ = sub.Close()
		if err != nil {
			if hostprep.IsUnsafe(err) {
				return nil, err
			}
			continue
		}
	}
	return out, nil
}

// hashDestinationCached is hashDestination through the stat cache: the file is
// opened (descriptor-anchored, regular, singly linked) and only read when its
// stamp does not vouch for a recorded hash.
func hashDestinationCached(dst *destination, sp Spec, rel string, sc *statCache) (string, bool, error) {
	f, err := dst.root.OpenFile(dst.path(rel))
	if err != nil {
		if hostprep.IsUnsafe(err) {
			return "", false, err
		}
		return "", false, nil
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", false, nil
	}
	h, err := sc.hash(sp, rel, info, f)
	if err != nil {
		return "", false, nil
	}
	return h, true, nil
}

func hashDestination(dst *destination, sp Spec, rel string) (string, bool, error) {
	b, err := dst.read(rel)
	if err != nil {
		if hostprep.IsUnsafe(err) {
			return "", false, err
		}
		return "", false, nil
	}
	return hashDestinationBytes(sp, rel, b), true, nil
}

func hashDestinationBytes(sp Spec, rel string, b []byte) string {
	if e := entryFor(sp, rel); e.Rel == rel && e.Normalize != nil {
		b = e.Normalize(sp, b)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// entryFor finds the entry covering rel: an exact file entry, or the directory
// entry rel lives under. A zero Entry (Rel "") when none does.
func entryFor(sp Spec, rel string) Entry {
	for _, e := range sp.Entries {
		if e.Rel == rel || strings.HasPrefix(rel, e.Rel+string(filepath.Separator)) {
			return e
		}
	}
	return Entry{}
}

// covered reports whether rel is a templated path (an entry, under one, or shared).
func covered(sp Spec, rel string) bool {
	if rel == "" || rel == "." || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return false
	}
	return entryFor(sp, rel).Rel != "" || isShared(sp, rel)
}

func isShared(sp Spec, rel string) bool {
	for _, s := range sp.Shared {
		if s == rel {
			return true
		}
	}
	return false
}

func hardlinkShared(sp Spec, rel string) bool {
	for _, s := range sp.HardlinkShared {
		if s == rel {
			return true
		}
	}
	return false
}

// src is the template-side absolute path of the entry itself.
func (e Entry) src(sp Spec) string {
	if e.Src != "" {
		return e.Src
	}
	return filepath.Join(sp.Template, e.Rel)
}

// templatePath is the template-side absolute path of rel: an exact file entry
// honors its Src override; anything else (a file under a directory entry, a
// shared file) is Template/rel.
func templatePath(sp Spec, rel string) string {
	if e := entryFor(sp, rel); e.Rel == rel {
		return e.src(sp)
	}
	return filepath.Join(sp.Template, rel)
}

func readManifest(sp Spec) (*manifest, error) {
	b, err := os.ReadFile(manifestPath(sp))
	if err != nil {
		return nil, err
	}
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	if m.Files == nil {
		m.Files = map[string]string{}
	}
	return &m, nil
}

func writeManifest(sp Spec, files map[string]string) error {
	if err := os.MkdirAll(StateDir(), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(manifest{
		Kind: sp.Kind, Template: sp.Template, Dir: sp.Dir, Seeded: time.Now().UnixMilli(), Files: files,
	}, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(manifestPath(sp), b, 0o644)
}

// writeFile writes b to path atomically (tmp + rename), creating parents.
func writeFile(path string, b []byte, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".amux.tmp"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
