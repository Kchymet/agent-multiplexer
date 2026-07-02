// Package codexcfg makes minimal, safe reads of and edits to OpenAI Codex CLI's
// on-disk state on amux's behalf: discovering a session's rollout file (so amux
// can resume or gap-fill it), listing conversations across the machine, and
// pre-trusting directories amux creates (so Codex doesn't prompt to trust the
// folder). It mirrors internal/claudecfg's shape and conventions for the Codex
// world; where claudecfg reads Claude Code's <projects>/<munge(cwd)>/<uuid>.jsonl
// transcripts, codexcfg reads Codex's $CODEX_HOME/sessions/YYYY/MM/DD rollout
// files. Everything is best-effort: a format drift degrades to "no match" rather
// than a crash, so callers proceed.
package codexcfg

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var mu sync.Mutex // serialize our own read-modify-write of config.toml

// Home is Codex's config/data home: $CODEX_HOME, or ~/.codex when unset.
func Home() string {
	if d := os.Getenv("CODEX_HOME"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex")
}

// sessionsRoot is where Codex stores per-day rollout files
// ($CODEX_HOME/sessions/YYYY/MM/DD/rollout-<timestamp>-<uuid>.jsonl).
func sessionsRoot() string { return filepath.Join(Home(), "sessions") }

// ConfigPath is $CODEX_HOME/config.toml — Codex's user config.
func ConfigPath() string { return filepath.Join(Home(), "config.toml") }

// rolloutFile is one rollout jsonl discovered under the sessions tree.
type rolloutFile struct {
	uuid string
	path string
	info os.FileInfo
}

// eachRollout walks the sessions tree and returns every
// rollout-<timestamp>-<uuid>.jsonl file it finds, with the uuid parsed from the
// name. Best-effort: unreadable subtrees are skipped (siblings still walked) and
// a missing tree yields nil, so discovery degrades gracefully rather than
// failing. Callers filter/sort the results.
func eachRollout() []rolloutFile {
	var out []rolloutFile
	_ = filepath.WalkDir(sessionsRoot(), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip an unreadable dir, keep walking siblings
		}
		if d.IsDir() {
			return nil
		}
		uuid, ok := rolloutUUID(d.Name())
		if !ok {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		out = append(out, rolloutFile{uuid: uuid, path: path, info: info})
		return nil
	})
	return out
}

// rolloutUUID extracts the session uuid from a rollout filename of the form
// rollout-<timestamp>-<uuid>.jsonl. The timestamp itself contains hyphens, so we
// can't split on '-'; instead we take the trailing canonical 36-char uuid and
// validate its shape. Anything that isn't a rollout with a uuid tail yields
// ok=false, so a stray file never masquerades as a session.
func rolloutUUID(name string) (string, bool) {
	if !strings.HasPrefix(name, "rollout-") || !strings.HasSuffix(name, ".jsonl") {
		return "", false
	}
	core := name[len("rollout-") : len(name)-len(".jsonl")]
	if len(core) < 37 || core[len(core)-37] != '-' { // need "<ts>-<uuid>"
		return "", false
	}
	uuid := core[len(core)-36:]
	if !looksLikeUUID(uuid) {
		return "", false
	}
	return uuid, true
}

// looksLikeUUID reports whether s is a canonical 8-4-4-4-12 hex uuid.
func looksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
				return false
			}
		}
	}
	return true
}

// RolloutPath returns the path to uuid's rollout file if one exists under the
// sessions tree, mirroring claudecfg's transcript-locating role. ok is false for
// a blank uuid or when no rollout is found.
//
// This sits on the daemon's poll path (codexHarness.Activity calls it per live
// codex agent every tick), so unlike eachRollout it matches on filenames alone —
// no per-file stat — and stops at the first hit. The uuid is embedded in the
// rollout filename, which is all discovery needs.
func RolloutPath(uuid string) (string, bool) {
	if uuid == "" {
		return "", false
	}
	suffix := "-" + uuid + ".jsonl"
	var found string
	_ = filepath.WalkDir(sessionsRoot(), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil // skip an unreadable dir, keep walking siblings
		}
		if strings.HasPrefix(d.Name(), "rollout-") && strings.HasSuffix(d.Name(), suffix) {
			found = path
			return fs.SkipAll
		}
		return nil
	})
	return found, found != ""
}

// NewRolloutPath builds a fresh, plausible rollout path for uuid under today's
// sessions/YYYY/MM/DD directory, following Codex's rollout-<timestamp>-<uuid>
// naming. amux uses it to place a gap-filled transcript where `codex resume
// <id>` will discover it when no rollout for uuid exists on disk yet. Discovery
// keys on the uuid in the filename, so the timestamp portion is only cosmetic
// and need not match Codex's to the letter.
func NewRolloutPath(uuid string) string {
	now := time.Now()
	name := "rollout-" + now.Format("2006-01-02T15-04-05") + "-" + uuid + ".jsonl"
	return filepath.Join(sessionsRoot(),
		now.Format("2006"), now.Format("01"), now.Format("02"), name)
}

// LatestSession returns the uuid of the newest (by mtime) rollout recorded under
// cwd, so amux can resume the most recent conversation for a directory. ok is
// false when cwd has no rollout.
func LatestSession(cwd string) (uuid string, ok bool) {
	var best rolloutFile
	for _, r := range eachRollout() {
		if !sameDir(rolloutCwd(r.path), cwd) {
			continue
		}
		if !ok || r.info.ModTime().After(best.info.ModTime()) {
			best, ok = r, true
		}
	}
	return best.uuid, ok
}

// SessionInfo describes one saved Codex rollout discovered under the sessions
// tree: its session uuid, the working directory it ran in, the day-grouping it
// is stored under, the rollout path, and file metadata. It mirrors
// claudecfg.SessionInfo so `amux agent sessions` can list Codex conversations
// alongside Claude's with a single row shape.
type SessionInfo struct {
	ID       string    `json:"id"`       // Codex session uuid (parsed from the rollout filename)
	Cwd      string    `json:"cwd"`      // originating working dir, read from the rollout meta (best-effort)
	Project  string    `json:"project"`  // day-grouping dir (YYYY/MM/DD) the rollout is stored under
	Path     string    `json:"path"`     // absolute path to the .jsonl rollout
	Size     int64     `json:"size"`     // rollout size in bytes
	Modified time.Time `json:"modified"` // rollout mtime (proxy for last activity)
}

// ListSessions enumerates every Codex rollout across the sessions tree,
// most-recently-modified first. Best-effort: unreadable dirs and files are
// skipped rather than failing the whole listing, mirroring claudecfg.ListSessions.
func ListSessions() []SessionInfo {
	var out []SessionInfo
	root := sessionsRoot()
	for _, r := range eachRollout() {
		proj, err := filepath.Rel(root, filepath.Dir(r.path))
		if err != nil {
			proj = ""
		}
		out = append(out, SessionInfo{
			ID:       r.uuid,
			Cwd:      rolloutCwd(r.path),
			Project:  filepath.ToSlash(proj),
			Path:     r.path,
			Size:     r.info.Size(),
			Modified: r.info.ModTime(),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Modified.After(out[j].Modified)
	})
	return out
}

// rolloutCwd reads the working directory a rollout was recorded under. Codex
// writes a session_meta record early in the file whose payload carries the cwd;
// to survive format drift we scan the first several JSONL lines and accept a cwd
// from either the top level or a nested "payload". Returns "" if none is found —
// so a drifted/corrupt rollout degrades to "no match" rather than a crash.
func rolloutCwd(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // rollout lines can be large
	for i := 0; sc.Scan() && i < 16; i++ {           // only the first few lines carry meta
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var rec struct {
			Cwd     string `json:"cwd"`
			Payload struct {
				Cwd string `json:"cwd"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		if rec.Cwd != "" {
			return rec.Cwd
		}
		if rec.Payload.Cwd != "" {
			return rec.Payload.Cwd
		}
	}
	return ""
}

// sameDir reports whether two directory paths refer to the same location, after
// making both absolute. A blank path never matches.
func sameDir(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	aa, err := filepath.Abs(a)
	if err != nil {
		aa = a
	}
	bb, err := filepath.Abs(b)
	if err != nil {
		bb = b
	}
	return aa == bb
}

// PreferredModel returns the user's configured Codex model (the top-level
// "model" key in config.toml), or "" if unset or unreadable. amux uses it as the
// rational default when interactively configuring a new codex agent. Best-effort
// — callers treat "" as "let amux pick the harness default". Top-level keys in
// TOML precede any table, so we scan until the first table header.
func PreferredModel() string {
	b, err := os.ReadFile(ConfigPath())
	if err != nil {
		return ""
	}
	for _, ln := range strings.Split(string(b), "\n") {
		s := strings.TrimSpace(ln)
		if strings.HasPrefix(s, "[") {
			break // entered a table; top-level keys come before tables
		}
		if v, ok := keyValue(s, "model"); ok {
			return v
		}
	}
	return ""
}

// TrustDir marks dir as trusted in Codex's config.toml by ensuring a
//
//	[projects."<abs-dir>"]
//	trust_level = "trusted"
//
// table, so Codex doesn't prompt to trust the folder. It hand-rolls the minimal
// TOML edit (no external dependency): it preserves all unrelated content, creates
// the file if absent, corrects a differing trust_level in place, and is
// idempotent — an already-trusted dir leaves the file byte-for-byte unchanged.
// Best-effort: on any error the caller should proceed (Codex will just prompt
// once). Written atomically so a concurrent Codex never sees a partial file.
func TrustDir(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	mu.Lock()
	defer mu.Unlock()

	path := ConfigPath()
	var lines []string
	if b, err := os.ReadFile(path); err == nil {
		lines = strings.Split(string(b), "\n")
	}

	header := "[projects." + tomlQuote(abs) + "]"
	const trustLine = `trust_level = "trusted"`

	// Locate this dir's project table, if it already exists.
	hdr := -1
	for i, ln := range lines {
		if isProjectsHeaderFor(ln, abs) {
			hdr = i
			break
		}
	}

	if hdr >= 0 {
		// Scan the table body (up to the next table header) for trust_level.
		for i := hdr + 1; i < len(lines); i++ {
			if strings.HasPrefix(strings.TrimSpace(lines[i]), "[") {
				break // next table starts here
			}
			if val, ok := keyValue(lines[i], "trust_level"); ok {
				if val == "trusted" {
					return nil // already trusted; leave the file untouched
				}
				lines[i] = trustLine // correct a differing trust level
				return writeLines(path, lines)
			}
		}
		// Table present but missing trust_level: insert right after the header.
		lines = insertAt(lines, hdr+1, trustLine)
		return writeLines(path, lines)
	}

	// No table for this dir: append one, separated from prior content by a blank.
	if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) != "" {
		lines = append(lines, "")
	}
	lines = append(lines, header, trustLine)
	return writeLines(path, lines)
}

// isProjectsHeaderFor reports whether line is the [projects."<abs>"] table
// header for abs, comparing the unquoted key so quoting/spacing differences
// don't cause a duplicate table to be appended.
func isProjectsHeaderFor(line, abs string) bool {
	s := strings.TrimSpace(line)
	if !strings.HasPrefix(s, "[projects.") || !strings.HasSuffix(s, "]") {
		return false
	}
	return tomlUnquote(s[len("[projects."):len(s)-1]) == abs
}

// keyValue parses a `key = value` TOML line, returning the unquoted value. The
// '=' check means a longer key with the same prefix (e.g. "model_provider" vs
// "model") is not mistaken for a match.
func keyValue(line, key string) (string, bool) {
	s := strings.TrimSpace(line)
	if !strings.HasPrefix(s, key) {
		return "", false
	}
	rest := strings.TrimSpace(s[len(key):])
	if !strings.HasPrefix(rest, "=") {
		return "", false
	}
	return tomlUnquote(strings.TrimSpace(rest[1:])), true
}

// tomlQuote renders s as a TOML basic string, escaping backslashes and quotes.
func tomlQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// tomlUnquote reverses tomlQuote for a basic string; a value that isn't
// double-quoted is returned trimmed and as-is.
func tomlUnquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return s
	}
	inner := s[1 : len(s)-1]
	var b strings.Builder
	for i := 0; i < len(inner); i++ {
		if inner[i] == '\\' && i+1 < len(inner) {
			i++
			switch inner[i] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			default:
				b.WriteByte(inner[i]) // \\ and \" fall through to the literal char
			}
			continue
		}
		b.WriteByte(inner[i])
	}
	return b.String()
}

// insertAt returns s with v inserted at index i.
func insertAt(s []string, i int, v string) []string {
	s = append(s, "")
	copy(s[i+1:], s[i:])
	s[i] = v
	return s
}

// writeLines joins lines and writes them to path atomically, ensuring a single
// trailing newline and creating the parent directory as needed.
func writeLines(path string, lines []string) error {
	out := strings.Join(lines, "\n")
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".amux.tmp"
	if err := os.WriteFile(tmp, []byte(out), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
