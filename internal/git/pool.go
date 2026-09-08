package git

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	poolManifestVersion = 1
	poolRefreshInterval = 30 * time.Second
)

type poolManifest struct {
	Version      int    `json:"version"`
	RepoKey      string `json:"repo_key"`
	Generation   string `json:"generation"`
	Source       string `json:"source"`
	DefaultRef   string `json:"default_ref"`
	BaseOID      string `json:"base_oid,omitempty"`
	Predecessor  string `json:"predecessor,omitempty"`
	SourcePolicy string `json:"source_policy"`
}

type poolResult struct {
	SourcePolicy string
	DefaultRef   string
	BaseOID      string
	Mounts       []GitObjectMount
}

// PrepareObjectPool builds or refreshes the trusted authorized backing store.
// It is used while tracking a new repository so the legacy compatibility
// inventory need not duplicate the remote's object database.
func PrepareObjectPool(ctx context.Context, poolRoot, repoKey, source string, allowLocal bool) error {
	_, err := ensureObjectPool(ctx, poolRoot, repoKey, source, allowLocal)
	return err
}

func cachedObjectPool(poolRoot, repoKey, source string, allowLocal bool, maxAge time.Duration) (poolResult, bool, error) {
	if err := validateRepoKey(repoKey); err != nil {
		return poolResult{}, false, err
	}
	policy, err := classifyPoolSource(source, allowLocal)
	if err != nil {
		return poolResult{}, false, err
	}
	poolRoot, err = filepath.Abs(filepath.Clean(poolRoot))
	if err != nil {
		return poolResult{}, false, err
	}
	canonical, err := filepath.EvalSymlinks(poolRoot)
	if os.IsNotExist(err) {
		return poolResult{}, false, nil
	}
	if err != nil {
		return poolResult{}, false, err
	}
	repoRoot := filepath.Join(canonical, repoKey)
	currentPath := filepath.Join(repoRoot, "current.json")
	info, err := os.Lstat(currentPath)
	if os.IsNotExist(err) {
		return poolResult{}, false, nil
	}
	if err != nil {
		return poolResult{}, false, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return poolResult{}, false, fmt.Errorf("Git pool current record is not a regular file")
	}
	if maxAge >= 0 && time.Since(info.ModTime()) > maxAge {
		return poolResult{}, false, nil
	}
	current, err := readPoolManifest(currentPath)
	if err != nil {
		return poolResult{}, false, err
	}
	if current.RepoKey != repoKey || current.Source != source || current.SourcePolicy != policy {
		return poolResult{}, false, nil
	}
	mounts, err := mountClosure(repoRoot, current)
	if err != nil {
		return poolResult{}, false, err
	}
	return poolResult{SourcePolicy: policy, DefaultRef: current.DefaultRef, BaseOID: current.BaseOID, Mounts: mounts}, true, nil
}

func objectPoolForCheckout(ctx context.Context, poolRoot, repoKey, source string, allowLocal bool) (poolResult, error) {
	if cached, ok, err := cachedObjectPool(poolRoot, repoKey, source, allowLocal, poolRefreshInterval); err != nil || ok {
		return cached, err
	}
	return ensureObjectPool(ctx, poolRoot, repoKey, source, allowLocal)
}

var poolMutexes sync.Map

func poolMutex(path string) *sync.Mutex {
	v, _ := poolMutexes.LoadOrStore(path, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// SourceKey returns an opaque stable key for a canonical checkout source.
// Paths derived from repository display names or caller-controlled IDs are not
// used as pool path components.
func SourceKey(source string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(source)))
	return hex.EncodeToString(sum[:])
}

func validateRepoKey(key string) error {
	if len(key) != sha256.Size*2 {
		return fmt.Errorf("invalid Git pool repository key")
	}
	if _, err := hex.DecodeString(key); err != nil || strings.ToLower(key) != key {
		return fmt.Errorf("invalid Git pool repository key")
	}
	return nil
}

func validGenerationName(generation string) bool {
	if len(generation) != sha256.Size {
		return false
	}
	_, err := hex.DecodeString(generation)
	return err == nil && generation == strings.ToLower(generation)
}

func classifyPoolSource(source string, allowLocal bool) (string, error) {
	source = strings.TrimSpace(source)
	if source == "" || strings.HasPrefix(source, "-") {
		return "", fmt.Errorf("invalid empty or option-like Git source")
	}
	if LooksLocal(source) {
		if !allowLocal {
			return "", fmt.Errorf("local Git source %q requires explicit host trust; set AMUX_GIT_TRUST_LOCAL_SOURCE=1 only when no session can write that source", source)
		}
		return "trusted-local", nil
	}
	if strings.HasPrefix(source, "file:") {
		if !allowLocal {
			return "", fmt.Errorf("file Git source %q requires explicit host trust; set AMUX_GIT_TRUST_LOCAL_SOURCE=1 only when no session can write that source", source)
		}
		return "trusted-local", nil
	}
	if strings.Contains(source, "::") {
		return "", fmt.Errorf("Git remote helpers and ext transports are not allowed for object pools")
	}
	if strings.Contains(source, ":") && !strings.Contains(source, "://") {
		if strings.Contains(source, "@") {
			return "network", nil // scp-style SSH URL
		}
		return "", fmt.Errorf("unsupported Git source syntax %q", source)
	}
	u, err := url.Parse(source)
	if err != nil {
		return "", fmt.Errorf("parse Git source: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "https", "http", "ssh", "git":
		if u.Host == "" {
			return "", fmt.Errorf("Git source has no host: %q", source)
		}
		if u.User != nil && strings.ToLower(u.Scheme) != "ssh" {
			return "", fmt.Errorf("credentials embedded in Git source URLs are not supported")
		}
		return "network", nil
	default:
		return "", fmt.Errorf("unsupported Git source scheme %q", u.Scheme)
	}
}

func poolGitArgs(source string, args ...string) []string {
	filePolicy := "never"
	if LooksLocal(source) || strings.HasPrefix(source, "file:") {
		filePolicy = "always"
	}
	safe := []string{
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.fsmonitor=false",
		"-c", "credential.helper=",
		"-c", "protocol.ext.allow=never",
		"-c", "protocol.file.allow=" + filePolicy,
		"-c", "fetch.fsckObjects=true",
		"-c", "transfer.fsckObjects=true",
		"-c", "core.pager=cat",
	}
	// GitHub authentication remains an explicit external authority boundary.
	// Use only the known gh credential protocol, never a mutable repository or
	// global credential-helper setting.
	if u, err := url.Parse(source); err == nil && strings.EqualFold(u.Hostname(), "github.com") {
		if gh, lookErr := exec.LookPath("gh"); lookErr == nil {
			safe = append(safe, "-c", "credential.helper=!"+gh+" auth git-credential")
		}
	}
	return append(safe, args...)
}

func poolGitEnv() []string {
	var env []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "GIT_") || key == "SSH_ASKPASS" || key == "PAGER" {
			continue
		}
		env = append(env, entry)
	}
	return append(env,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_COUNT=0",
		"GIT_CONFIG_PARAMETERS=",
		"GIT_ATTR_NOSYSTEM=1",
		"GIT_EXTERNAL_DIFF=",
		"GIT_PAGER=cat",
		"GIT_PROTOCOL_FROM_USER=0",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_SSH_COMMAND=ssh -F /dev/null -oBatchMode=yes",
		"GIT_ASKPASS=/bin/false",
		"SSH_ASKPASS=/bin/false",
		"GIT_REPLACE_REF_BASE=refs/replace/",
		"PAGER=cat",
	)
}

func runPoolGit(ctx context.Context, dir, source string, args ...string) (string, error) {
	return runExactEnvInput(ctx, dir, poolGitEnv(), nil, poolGitArgs(source, args...)...)
}

func runPoolGitInput(ctx context.Context, dir, source string, extraEnv []string, input []byte, args ...string) (string, error) {
	env := append(poolGitEnv(), extraEnv...)
	return runExactEnvInput(ctx, dir, env, input, poolGitArgs(source, args...)...)
}

func runPoolGitObjects(ctx context.Context, dir, source string, objectDirs []string, args ...string) (string, error) {
	env := poolGitEnv()
	if len(objectDirs) > 0 {
		parts := make([]string, 0, len(objectDirs))
		for _, path := range objectDirs {
			if strings.ContainsRune(path, os.PathListSeparator) {
				path = strconv.Quote(path)
			}
			parts = append(parts, path)
		}
		env = append(env, "GIT_ALTERNATE_OBJECT_DIRECTORIES="+strings.Join(parts, string(os.PathListSeparator)))
	}
	return runExactEnvInput(ctx, dir, env, nil, poolGitArgs(source, args...)...)
}

func remoteHead(ctx context.Context, source string) (ref, oid string, err error) {
	out, err := runPoolGit(ctx, "", source, "ls-remote", "--symref", "--", source, "HEAD")
	if err != nil {
		return "", "", err
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == "ref:" && fields[2] == "HEAD" {
			ref = fields[1]
		}
		if len(fields) == 2 && fields[1] == "HEAD" && fields[0] != "ref:" {
			oid = fields[0]
		}
	}
	if ref == "" && oid == "" {
		// Empty repositories do not advertise HEAD. Git's modern default is main;
		// the first push remains an assigned branch and does not depend on this
		// placeholder having existed upstream.
		return "refs/heads/main", "", nil
	}
	if !strings.HasPrefix(ref, "refs/heads/") || strings.Contains(ref, "..") {
		return "", "", fmt.Errorf("remote HEAD did not resolve to a branch: %q", ref)
	}
	if len(oid) != 40 && len(oid) != 64 {
		return "", "", fmt.Errorf("remote HEAD returned invalid object ID %q", oid)
	}
	return ref, oid, nil
}

func generationID(repoKey, source, ref, oid string) string {
	sum := sha256.Sum256([]byte(repoKey + "\x00" + source + "\x00" + ref + "\x00" + oid))
	return hex.EncodeToString(sum[:16])
}

func writeJSONAtomic(path string, value any, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".record-")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func readPoolManifest(path string) (poolManifest, error) {
	var m poolManifest
	info, err := os.Lstat(path)
	if err != nil {
		return m, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return m, fmt.Errorf("Git pool manifest is not a regular file: %s", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, err
	}
	if m.Version != poolManifestVersion {
		return m, fmt.Errorf("unsupported Git pool manifest version %d", m.Version)
	}
	if err := validateRepoKey(m.RepoKey); err != nil {
		return m, err
	}
	if !validGenerationName(m.Generation) || (m.Predecessor != "" && !validGenerationName(m.Predecessor)) {
		return m, fmt.Errorf("Git pool manifest contains an invalid generation name")
	}
	if !strings.HasPrefix(m.DefaultRef, "refs/heads/") || strings.Contains(m.DefaultRef, "..") {
		return m, fmt.Errorf("Git pool manifest contains an invalid default ref")
	}
	if m.SourcePolicy != "network" && m.SourcePolicy != "trusted-local" {
		return m, fmt.Errorf("Git pool manifest contains an invalid source policy")
	}
	return m, nil
}

func isAncestor(ctx context.Context, gitDir, source string, objectDirs []string, oldOID, newOID string) (bool, error) {
	_, err := runPoolGitObjects(ctx, "", source, objectDirs, "--git-dir", gitDir, "merge-base", "--is-ancestor", oldOID, newOID)
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}

func writeAlternates(objectsDir string, objectDirs []string) error {
	info := filepath.Join(objectsDir, "info")
	if err := os.MkdirAll(info, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(info, "alternates"), []byte(strings.Join(objectDirs, "\n")+"\n"), 0o600)
}

func fetchGeneration(ctx context.Context, source, ref, oid, repoGit string, predecessorObjects []string) error {
	if _, err := runPoolGit(ctx, "", source, "init", "--bare", "--initial-branch=amux-pool-base", repoGit); err != nil {
		return err
	}
	if oid == "" {
		return nil
	}
	refspec := "+" + ref + ":refs/heads/amux-pool-base"
	if _, err := runPoolGitObjects(ctx, "", source, predecessorObjects, "--git-dir", repoGit, "fetch", "--no-tags", "--force", "--", source, refspec); err != nil {
		return err
	}
	got, err := runPoolGitObjects(ctx, "", source, predecessorObjects, "--git-dir", repoGit, "rev-parse", "refs/heads/amux-pool-base^{commit}")
	if err != nil {
		return err
	}
	if got != oid {
		return fmt.Errorf("Git pool fetched %q, want authoritative HEAD %q", got, oid)
	}
	return nil
}

func validateGeneration(ctx context.Context, repoGit string, m poolManifest, predecessorObjects []string) error {
	refs, err := runPoolGitObjects(ctx, "", m.Source, predecessorObjects, "--git-dir", repoGit, "for-each-ref", "--format=%(refname)")
	if err != nil {
		return err
	}
	wantRefs := ""
	if m.BaseOID != "" {
		wantRefs = "refs/heads/amux-pool-base"
	}
	if strings.TrimSpace(refs) != wantRefs {
		return fmt.Errorf("Git pool contains unexpected refs: %q", refs)
	}
	altPath := filepath.Join(repoGit, "objects", "info", "alternates")
	if _, altErr := os.Lstat(altPath); altErr == nil || !os.IsNotExist(altErr) {
		return fmt.Errorf("immutable Git pool generation contains an alternate file")
	}
	if _, err := os.Lstat(filepath.Join(repoGit, "objects", "info", "http-alternates")); err == nil || !os.IsNotExist(err) {
		return fmt.Errorf("Git pool contains forbidden http-alternates")
	}
	if m.BaseOID != "" {
		if _, err := runPoolGitObjects(ctx, "", m.Source, predecessorObjects, "--git-dir", repoGit, "fsck", "--connectivity-only", m.BaseOID); err != nil {
			return err
		}
	}
	return nil
}

func sealGeneration(path string) error {
	var dirs []string
	err := filepath.Walk(path, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("Git pool generation contains symlink: %s", p)
		}
		if info.IsDir() {
			dirs = append(dirs, p)
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("Git pool generation contains non-regular file: %s", p)
		}
		return os.Chmod(p, 0o444)
	})
	if err != nil {
		return err
	}
	for _, dir := range dirs {
		// The host-owned generation parent remains traversable/removable for a
		// future reference-aware collector. Immutability is enforced by never
		// reopening a published generation for Git writes and by the namespace's
		// read-only bind; regular files themselves are not writable.
		if err := os.Chmod(dir, 0o755); err != nil {
			return err
		}
	}
	return nil
}

func generationObjectDirs(repoRoot string, tip poolManifest) ([]string, error) {
	mounts, err := mountClosure(repoRoot, tip)
	if err != nil {
		return nil, err
	}
	dirs := make([]string, 0, len(mounts))
	for _, mount := range mounts {
		dirs = append(dirs, mount.ObjectsHostDir)
	}
	return dirs, nil
}

func matchingGeneration(repoRoot, generation, repoKey, source, policy, ref, oid string) (poolManifest, bool, error) {
	m, err := readPoolManifest(filepath.Join(repoRoot, generation, "manifest.json"))
	if os.IsNotExist(err) {
		return poolManifest{}, false, nil
	}
	if err != nil {
		return poolManifest{}, false, err
	}
	if m.RepoKey != repoKey || m.Generation != generation || m.Source != source ||
		m.SourcePolicy != policy || m.DefaultRef != ref || m.BaseOID != oid {
		return poolManifest{}, false, fmt.Errorf("existing Git pool generation does not match requested identity")
	}
	return m, true, nil
}

// createGeneration publishes only after a requested fast-forward relationship
// has been proven in staging. A rejected non-fast-forward candidate is removed;
// it can never become an alternate edge selected by a later rollback.
func createGeneration(ctx context.Context, repoRoot, repoKey, source, policy, ref, oid, discriminator string, predecessor *poolManifest, requireAncestor string) (poolManifest, bool, error) {
	gen := generationID(repoKey+discriminator, source, ref, oid)
	finalDir := filepath.Join(repoRoot, gen)
	if _, err := os.Lstat(finalDir); err == nil {
		return poolManifest{}, false, fmt.Errorf("Git pool generation already exists unexpectedly: %s", gen)
	} else if !os.IsNotExist(err) {
		return poolManifest{}, false, err
	}
	staged, err := os.MkdirTemp(repoRoot, ".generation-")
	if err != nil {
		return poolManifest{}, false, err
	}
	defer os.RemoveAll(staged)
	repoGit := filepath.Join(staged, "repo.git")
	var predObjects []string
	predID := ""
	if predecessor != nil {
		predID = predecessor.Generation
		predObjects, err = generationObjectDirs(repoRoot, *predecessor)
		if err != nil {
			return poolManifest{}, false, err
		}
	}
	if err := fetchGeneration(ctx, source, ref, oid, repoGit, predObjects); err != nil {
		return poolManifest{}, false, err
	}
	if requireAncestor != "" {
		ff, err := isAncestor(ctx, repoGit, source, predObjects, requireAncestor, oid)
		if err != nil {
			return poolManifest{}, false, err
		}
		if !ff {
			return poolManifest{}, false, nil
		}
	}
	m := poolManifest{Version: poolManifestVersion, RepoKey: repoKey, Generation: gen, Source: source, DefaultRef: ref, BaseOID: oid, Predecessor: predID, SourcePolicy: policy}
	if err := validateGeneration(ctx, repoGit, m, predObjects); err != nil {
		return poolManifest{}, false, err
	}
	if err := writeJSONAtomic(filepath.Join(staged, "manifest.json"), m, 0o600); err != nil {
		return poolManifest{}, false, err
	}
	if err := sealGeneration(staged); err != nil {
		return poolManifest{}, false, err
	}
	if err := os.Rename(staged, finalDir); err != nil {
		if os.IsExist(err) {
			existing, ok, readErr := matchingGeneration(repoRoot, gen, repoKey, source, policy, ref, oid)
			return existing, ok, readErr
		}
		return poolManifest{}, false, err
	}
	return m, true, nil
}

func mountClosure(repoRoot string, tip poolManifest) ([]GitObjectMount, error) {
	seen := map[string]bool{}
	var reverse []poolManifest
	cur := tip
	for {
		if seen[cur.Generation] || len(reverse) > 1024 {
			return nil, fmt.Errorf("Git pool generation chain is cyclic or too deep")
		}
		seen[cur.Generation] = true
		if cur.RepoKey != tip.RepoKey {
			return nil, fmt.Errorf("Git pool generation crossed repository boundary")
		}
		reverse = append(reverse, cur)
		if cur.Predecessor == "" {
			break
		}
		predecessor := cur.Predecessor
		var err error
		cur, err = readPoolManifest(filepath.Join(repoRoot, predecessor, "manifest.json"))
		if err != nil {
			return nil, fmt.Errorf("read predecessor generation %s: %w", predecessor, err)
		}
	}
	mounts := make([]GitObjectMount, 0, len(reverse))
	for i := len(reverse) - 1; i >= 0; i-- {
		m := reverse[i]
		objects := filepath.Join(repoRoot, m.Generation, "repo.git", "objects")
		canonical, err := filepath.EvalSymlinks(objects)
		if err != nil {
			return nil, err
		}
		mounts = append(mounts, GitObjectMount{RepoKey: m.RepoKey, Generation: m.Generation, ObjectsHostDir: canonical, ObjectsMountDir: canonical})
	}
	return mounts, nil
}

func ensureObjectPool(ctx context.Context, poolRoot, repoKey, source string, allowLocal bool) (poolResult, error) {
	if err := validateRepoKey(repoKey); err != nil {
		return poolResult{}, err
	}
	policy, err := classifyPoolSource(source, allowLocal)
	if err != nil {
		return poolResult{}, err
	}
	poolRoot, err = filepath.Abs(filepath.Clean(poolRoot))
	if err != nil {
		return poolResult{}, err
	}
	if err := os.MkdirAll(poolRoot, 0o700); err != nil {
		return poolResult{}, err
	}
	poolRoot, err = filepath.EvalSymlinks(poolRoot)
	if err != nil {
		return poolResult{}, fmt.Errorf("canonicalize Git pool root: %w", err)
	}
	repoRoot := filepath.Join(poolRoot, repoKey)
	lock := poolMutex(repoRoot)
	lock.Lock()
	defer lock.Unlock()
	if err := os.MkdirAll(repoRoot, 0o700); err != nil {
		return poolResult{}, err
	}
	ref, oid, err := remoteHead(ctx, source)
	if err != nil {
		return poolResult{}, err
	}
	currentPath := filepath.Join(repoRoot, "current.json")
	var current *poolManifest
	if m, readErr := readPoolManifest(currentPath); readErr == nil {
		current = &m
	} else if !os.IsNotExist(readErr) {
		return poolResult{}, readErr
	}
	if current != nil && current.Source == source && current.DefaultRef == ref && current.BaseOID == oid {
		mounts, err := mountClosure(repoRoot, *current)
		if err != nil {
			return poolResult{}, err
		}
		// A successful remote verification renews the bounded freshness window.
		// Without this rewrite, every later checkout after the first expiry would
		// pay another network round trip even though the authoritative tip matched.
		if err := writeJSONAtomic(currentPath, *current, 0o600); err != nil {
			return poolResult{}, err
		}
		return poolResult{SourcePolicy: policy, DefaultRef: ref, BaseOID: oid, Mounts: mounts}, nil
	}
	var next poolManifest
	standardID := generationID(repoKey, source, ref, oid)
	if candidate, ok, candidateErr := matchingGeneration(repoRoot, standardID, repoKey, source, policy, ref, oid); candidateErr != nil {
		return poolResult{}, candidateErr
	} else if ok {
		// A cached generation carries only its own previously verified reachable
		// lineage. Selecting it on A -> B -> A does not grant B to the new A.
		next = candidate
	} else if current != nil && current.Source == source && current.DefaultRef == ref && current.BaseOID != "" && oid != "" {
		var published bool
		next, published, err = createGeneration(ctx, repoRoot, repoKey, source, policy, ref, oid, "", current, current.BaseOID)
		if err != nil {
			return poolResult{}, err
		}
		if !published {
			// The staged delta saw both commits and proved a non-fast-forward.
			// Re-fetch the selected ref into a root generation with no predecessor.
			discriminator := "\x00root\x00" + current.Generation
			next, published, err = createGeneration(ctx, repoRoot, repoKey, source, policy, ref, oid, discriminator, nil, "")
			if err != nil {
				return poolResult{}, err
			}
			if !published {
				return poolResult{}, fmt.Errorf("failed to publish non-fast-forward Git pool root")
			}
		}
	} else {
		discriminator := ""
		if current != nil && current.Source == source && current.DefaultRef == ref && current.BaseOID != oid {
			discriminator = "\x00root\x00" + current.Generation
		}
		var published bool
		next, published, err = createGeneration(ctx, repoRoot, repoKey, source, policy, ref, oid, discriminator, nil, "")
		if err == nil && !published {
			err = fmt.Errorf("failed to publish Git pool root generation")
		}
	}
	if err != nil {
		return poolResult{}, err
	}
	if err := writeJSONAtomic(currentPath, next, 0o600); err != nil {
		return poolResult{}, err
	}
	mounts, err := mountClosure(repoRoot, next)
	if err != nil {
		return poolResult{}, err
	}
	return poolResult{SourcePolicy: policy, DefaultRef: ref, BaseOID: oid, Mounts: mounts}, nil
}
