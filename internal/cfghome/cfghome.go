// Package cfghome templates a harness's user-level configuration into an agent's
// sandbox and feeds the agent's edits back to amux.
//
// The model: the user's own config dir (~/.claude, $CODEX_HOME) is a TEMPLATE.
// Each agent gets a private COPY of it under its sandbox dir, and the harness is
// pointed at the copy (CLAUDE_CONFIG_DIR, CODEX_HOME). Nothing from the user's
// home is mounted into the sandbox any more except the one thing that must stay
// shared — the OAuth credentials, which are symlinked from the copy back to the
// template and bound read-write at their template path. The agent may edit its
// copy freely: that is its own configuration.
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
	// Entries are the template's configuration paths (relative to both Template
	// and Dir): what a fresh copy is seeded with and what Scan watches. A
	// directory entry covers its whole tree. Per-machine state — transcripts,
	// caches, history — is deliberately not an entry: it stays private to each
	// home and is never compared.
	Entries []Entry
	// Shared are auth files (relative to Template) the copy must not own: the
	// copy holds a symlink to the template's file and the sandbox binds that file
	// back at its template path (Binds), so every agent — and the user — refresh
	// the same token instead of each holding a copy that rotation would strand.
	Shared []string
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
	// SharedDetached: an auth file the copy should merely link to has become a
	// real file — the harness rewrote it in place of the symlink, so the agent
	// now holds its own credential copy that rotation may strand.
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

var mu sync.Mutex // serialize manifest read-modify-write across Seed/Scan/Promote/Reset

// EnvEntry is the KEY=VALUE that points the harness at the agent's copy.
func (sp Spec) EnvEntry() string { return sp.Env + "=" + sp.Dir }

// Seeded reports whether the agent's copy exists.
func Seeded(sp Spec) bool {
	fi, err := os.Stat(sp.Dir)
	return err == nil && fi.IsDir()
}

// Seed creates the agent's private copy from the template if it does not exist
// yet, and records the manifest. It is idempotent: an existing copy is left
// exactly as the agent has it (its edits are the point), and only a missing
// manifest is rebuilt from the copy's current content. Returns whether a fresh
// copy was made. A template that is missing entirely still yields an (empty)
// copy, so a harness never falls back to the user's home by accident.
func Seed(sp Spec) (fresh bool, err error) {
	if sp.Dir == "" || sp.Template == "" {
		return false, errors.New("cfghome: spec needs Dir and Template")
	}
	mu.Lock()
	defer mu.Unlock()
	if Seeded(sp) {
		if _, err := readManifest(sp); err == nil {
			return false, nil
		}
		// The copy exists but its baseline is gone (pre-manifest copy, or a wiped
		// state dir): adopt the copy's current content as the baseline rather than
		// reporting every file as an edit.
		return false, writeManifest(sp, snapshot(sp, sp.Dir, false))
	}
	if err := os.MkdirAll(sp.Dir, 0o755); err != nil {
		return false, err
	}
	for _, e := range sp.Entries {
		if err := seedEntry(sp, e); err != nil {
			return false, err
		}
	}
	for _, rel := range sp.Shared {
		if err := linkShared(sp, rel); err != nil {
			return false, err
		}
	}
	return true, writeManifest(sp, snapshot(sp, sp.Dir, false))
}

// seedEntry copies one entry (file or tree) from the template into the copy,
// applying the entry's Seed transform to file content. A missing template path
// is simply absent from the copy.
func seedEntry(sp Spec, e Entry) error {
	src := e.src(sp)
	fi, err := os.Stat(src)
	if err != nil {
		return nil
	}
	dst := filepath.Join(sp.Dir, e.Rel)
	if !fi.IsDir() {
		b, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		if e.Seed != nil {
			b = e.Seed(sp, b)
		}
		return writeFile(dst, b, fi.Mode().Perm())
	}
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip what we can't read; the rest still seeds
		}
		if skipDir(d) {
			return fs.SkipDir
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
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
		return writeFile(target, b, mode)
	})
}

// skipDir names subtrees never templated or compared: VCS internals (a plugin
// marketplace is a git clone whose objects churn on every refresh).
func skipDir(d fs.DirEntry) bool { return d.IsDir() && d.Name() == ".git" }

// linkShared points the copy's rel at the template's file, so the harness reads
// and refreshes the one shared credential. Nothing is linked when the template
// has no such file (the user hasn't logged in on this machine).
func linkShared(sp Spec, rel string) error {
	src := filepath.Join(sp.Template, rel)
	if _, err := os.Lstat(src); err != nil {
		return nil
	}
	dst := filepath.Join(sp.Dir, rel)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	_ = os.Remove(dst)
	return os.Symlink(src, dst)
}

// Binds are the bubblewrap arguments that make the shared (auth) files reachable
// inside the scope: each is bound read-write at its own template path, which is
// exactly where the copy's symlink points. Everything else the harness needs is
// already inside the agent's dir, so these are the only binds a templated config
// requires — down from the whole config tree.
func Binds(sp Spec) [][]string {
	var out [][]string
	for _, rel := range sp.Shared {
		p := filepath.Join(sp.Template, rel)
		if _, err := os.Stat(p); err == nil {
			out = append(out, []string{"--bind-try", p, p})
		}
	}
	return out
}

// Scan compares the agent's copy with the template against the seed manifest
// and returns every diverged path, sorted. Where both sides now agree (the
// agent's edit was promoted, or the user made the same change) the manifest is
// advanced to that content so the path drops out of future scans; otherwise the
// manifest is untouched. An unseeded copy reports nothing.
func Scan(sp Spec) ([]Change, error) {
	mu.Lock()
	defer mu.Unlock()
	if !Seeded(sp) {
		return nil, nil
	}
	m, err := readManifest(sp)
	if err != nil {
		return nil, err
	}
	copyH := snapshot(sp, sp.Dir, false)
	tmplH := snapshot(sp, sp.Template, true)

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
				// propagated (or never mattered). Advance the baseline.
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
		if fi, err := os.Lstat(p); err == nil && fi.Mode()&os.ModeSymlink == 0 {
			out = append(out, Change{Kind: sp.Kind, Rel: rel, Status: SharedDetached,
				Copy: p, Template: filepath.Join(sp.Template, rel)})
		}
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
	mu.Lock()
	defer mu.Unlock()
	e := entryFor(sp, rel)
	src := filepath.Join(sp.Dir, rel)
	dst := templatePath(sp, rel)
	b, err := os.ReadFile(src)
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
	return settle(sp, rel)
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
	mu.Lock()
	defer mu.Unlock()
	if isShared(sp, rel) {
		return linkShared(sp, rel) // re-point a detached credential at the template
	}
	e := entryFor(sp, rel)
	src := templatePath(sp, rel)
	dst := filepath.Join(sp.Dir, rel)
	b, err := os.ReadFile(src)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.Remove(dst); err != nil && !errors.Is(err, fs.ErrNotExist) {
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
		if err := writeFile(dst, b, mode); err != nil {
			return err
		}
	}
	return settle(sp, rel)
}

// settle records the copy's current content of rel as the baseline. Caller
// holds mu.
func settle(sp Spec, rel string) error {
	m, err := readManifest(sp)
	if err != nil {
		m = &manifest{Files: map[string]string{}}
	}
	if h, ok := hashFile(sp, filepath.Join(sp.Dir, rel), rel, false); ok {
		m.Files[rel] = h
	} else {
		delete(m.Files, rel)
	}
	return writeManifest(sp, m.Files)
}

// Forget drops the agent's manifest — for when the agent (and with it the copy)
// is deleted.
func Forget(sp Spec) { _ = os.Remove(manifestPath(sp)) }

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
	summaryMu sync.Mutex
	summaries = map[string]summaryEntry{}
)

// Summary is Scan for the daemon's poll path: cached per copy for summaryTTL,
// never erroring (a scan failure reads as no changes), and logging the moment an
// agent's pending edits first appear or change, so the daemon log records what
// the agent did to its configuration even if nobody is watching the rail.
func Summary(sp Spec) []Change {
	summaryMu.Lock()
	defer summaryMu.Unlock()
	key := sp.Dir
	if e, ok := summaries[key]; ok && time.Since(e.at) < summaryTTL {
		return e.changes
	}
	prev := summaries[key].changes
	changes, err := Scan(sp)
	if err != nil {
		changes = nil
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

// snapshot hashes every templated file under root (the copy, or — template=true
// — the template, honoring per-entry Src overrides), keyed by template-relative
// path. Unreadable files are skipped.
func snapshot(sp Spec, root string, template bool) map[string]string {
	out := map[string]string{}
	for _, e := range sp.Entries {
		base := filepath.Join(root, e.Rel)
		if template {
			base = e.src(sp)
		}
		fi, err := os.Stat(base)
		if err != nil {
			continue
		}
		if !fi.IsDir() {
			if h, ok := hashFile(sp, base, e.Rel, template); ok {
				out[e.Rel] = h
			}
			continue
		}
		_ = filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if skipDir(d) {
				return fs.SkipDir
			}
			if d.IsDir() || !d.Type().IsRegular() {
				return nil
			}
			sub, _ := filepath.Rel(base, p)
			rel := filepath.Join(e.Rel, sub)
			if h, ok := hashFile(sp, p, rel, template); ok {
				out[rel] = h
			}
			return nil
		})
	}
	return out
}

// hashFile is the sha256 of path's content as compared: a template-side file is
// first put through the entry's Seed transform (so it hashes as the copy a
// fresh seed would make of it), then both sides through Normalize. ok=false
// when unreadable or a symlink.
//
// Results are memoized by (path, size, mtime, side): the daemon rescans every
// live agent's copy — plugin trees included — every summaryTTL, and a rescan
// must cost a stat per file, not a read and a hash.
func hashFile(sp Spec, path, rel string, template bool) (string, bool) {
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return "", false
	}
	// A file written in the last moment may be rewritten again inside the same
	// mtime tick with the same size; don't trust the memo for it.
	settled := time.Since(fi.ModTime()) > hashSettle
	key := hashKey{path: path, template: template, dir: sp.Dir}
	hashMu.Lock()
	if c, ok := hashCache[key]; ok && settled && c.size == fi.Size() && c.mtime.Equal(fi.ModTime()) {
		hashMu.Unlock()
		return c.sum, true
	}
	hashMu.Unlock()
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	if e := entryFor(sp, rel); e.Rel == rel {
		if template && e.Seed != nil {
			b = e.Seed(sp, b)
		}
		if e.Normalize != nil {
			b = e.Normalize(sp, b)
		}
	}
	sum := sha256.Sum256(b)
	hexSum := hex.EncodeToString(sum[:])
	hashMu.Lock()
	if len(hashCache) > hashCacheMax {
		hashCache = map[hashKey]hashEntry{} // crude bound; refills on the next scan
	}
	hashCache[key] = hashEntry{size: fi.Size(), mtime: fi.ModTime(), sum: hexSum}
	hashMu.Unlock()
	return hexSum, true
}

// hashKey identifies a memoized file hash. The side (template or copy) and the
// copy dir are part of the key because the transforms applied depend on both
// (a template file's Seed rewrite embeds the copy's path).
type hashKey struct {
	path     string
	template bool
	dir      string
}

type hashEntry struct {
	size  int64
	mtime time.Time
	sum   string
}

// hashCacheMax bounds the memo so a machine with many agents and large plugin
// trees can't grow it without limit.
const hashCacheMax = 50000

// hashSettle is how old a file's mtime must be before its memoized hash is
// trusted (see hashFile).
const hashSettle = 2 * time.Second

var (
	hashMu    sync.Mutex
	hashCache = map[hashKey]hashEntry{}
)

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
