package git

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	legacyIndependentLayoutVersion = "independent-v1\n"
	pooledWorktreeLayoutVersion    = 2
	gitPointerMaxBytes             = 4096
	gitAlternatesMaxBytes          = 1 << 20
)

// CheckoutLayout is the daemon-private authority for a pooled linked
// worktree. It is deliberately sufficient for validation and deletion without
// running Git against session-writable configuration.
type CheckoutLayout struct {
	Version        int              `json:"version"`
	RepoKey        string           `json:"repo_key"`
	CheckoutPath   string           `json:"checkout_path"`
	CommonDir      string           `json:"common_dir"`
	AdminDir       string           `json:"admin_dir"`
	Branch         string           `json:"branch"`
	Source         string           `json:"source"`
	DefaultRef     string           `json:"default_ref"`
	BaseOID        string           `json:"base_oid,omitempty"`
	SourceBoundary string           `json:"source_boundary"`
	ObjectMounts   []GitObjectMount `json:"object_mounts"`
}

func legacyLayoutRecord(path string) string {
	return legacyIndependentLayoutVersion + filepath.Clean(path) + "\n"
}

func writeCheckoutLayout(recordPath string, layout CheckoutLayout) error {
	if err := os.MkdirAll(filepath.Dir(recordPath), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(layout, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(recordPath), ".layout-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
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
	return os.Rename(tmpName, recordPath)
}

func readCheckoutLayout(recordPath, checkoutPath string) (*CheckoutLayout, bool, error) {
	info, err := os.Lstat(recordPath)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("inspect checkout layout record: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, false, fmt.Errorf("checkout layout record is not a regular file: %s", recordPath)
	}
	data, err := os.ReadFile(recordPath)
	if err != nil {
		return nil, false, fmt.Errorf("read checkout layout record: %w", err)
	}
	if bytes.HasPrefix(data, []byte(legacyIndependentLayoutVersion)) {
		if string(data) != legacyLayoutRecord(checkoutPath) {
			return nil, false, fmt.Errorf("checkout layout record does not match %s", checkoutPath)
		}
		return nil, true, nil
	}
	var layout CheckoutLayout
	if err := json.Unmarshal(data, &layout); err != nil {
		return nil, false, fmt.Errorf("decode checkout layout record: %w", err)
	}
	if layout.Version != pooledWorktreeLayoutVersion {
		return nil, false, fmt.Errorf("unsupported checkout layout version %d", layout.Version)
	}
	if filepath.Clean(layout.CheckoutPath) != filepath.Clean(checkoutPath) {
		return nil, false, fmt.Errorf("checkout layout record does not match %s", checkoutPath)
	}
	return &layout, false, nil
}

// ReadObjectMounts returns the immutable object generations recorded by the
// daemon for a checkout. It never consults the checkout's mutable .git file or
// alternates. The returned order is oldest generation to tip generation.
func ReadObjectMounts(recordPath, checkoutPath, managedRoot string) ([]GitObjectMount, error) {
	layout, legacyIndependent, err := readCheckoutLayout(recordPath, checkoutPath)
	if err != nil {
		return nil, err
	}
	if legacyIndependent {
		return nil, nil
	}
	if layout == nil {
		if err := validateRetainedPrivateClone(managedRoot, checkoutPath); err != nil {
			return nil, fmt.Errorf("missing trusted Git layout record for %s: %w", checkoutPath, err)
		}
		return nil, nil
	}
	return append([]GitObjectMount(nil), layout.ObjectMounts...), nil
}

// ValidateCheckoutLayout verifies a session's mutable linked-worktree pointers
// against the daemon-private record using filesystem reads only. It never runs
// Git and therefore cannot execute session config, hooks, helpers, or includes.
func ValidateCheckoutLayout(recordPath, checkoutPath, managedRoot string) error {
	layout, legacyIndependent, err := readCheckoutLayout(recordPath, checkoutPath)
	if err != nil {
		return err
	}
	if legacyIndependent {
		return validateRetainedPrivateClone(managedRoot, checkoutPath)
	}
	if layout == nil {
		// #133 briefly created full private clones before pooled worktrees
		// superseded that design. Retain those safe, already-created repositories
		// without creating any new ones. A linked .git file still fails closed.
		return validateRetainedPrivateClone(managedRoot, checkoutPath)
	}
	if err := validateRepoKey(layout.RepoKey); err != nil {
		return err
	}
	if layout.Branch == "" || layout.Source == "" || !strings.HasPrefix(layout.DefaultRef, "refs/heads/") {
		return fmt.Errorf("trusted pooled-worktree layout has invalid branch or source identity")
	}
	wantCommon := filepath.Join(filepath.Dir(filepath.Clean(checkoutPath)), ".amux", "git", layout.RepoKey+".git")
	if filepath.Clean(layout.CommonDir) != wantCommon {
		return fmt.Errorf("private Git common directory does not match trusted layout")
	}
	for label, path := range map[string]string{"checkout": checkoutPath, "private Git common directory": layout.CommonDir, "worktree administration": layout.AdminDir} {
		info, statErr := os.Lstat(path)
		if statErr != nil {
			return fmt.Errorf("inspect %s: %w", label, statErr)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is not a real directory: %s", label, path)
		}
	}
	admin, err := linkedAdminDir(managedRoot, checkoutPath)
	if err != nil {
		return fmt.Errorf("inspect linked worktree .git: %w", err)
	}
	if filepath.Clean(admin) != filepath.Clean(layout.AdminDir) {
		return fmt.Errorf("linked worktree .git does not match trusted administration path")
	}
	gitdir, err := readRegularFileAnchored(managedRoot, filepath.Join(layout.AdminDir, "gitdir"), gitPointerMaxBytes)
	if err != nil {
		return fmt.Errorf("read linked worktree administration: %w", err)
	}
	if filepath.Clean(strings.TrimSpace(string(gitdir))) != filepath.Join(filepath.Clean(checkoutPath), ".git") {
		return fmt.Errorf("linked worktree administration does not point to trusted checkout")
	}
	if len(layout.ObjectMounts) == 0 {
		return fmt.Errorf("trusted pooled-worktree layout has no object generations")
	}
	seen := map[string]bool{}
	for _, mount := range layout.ObjectMounts {
		if mount.RepoKey != layout.RepoKey || !validGenerationName(mount.Generation) || seen[mount.Generation] {
			return fmt.Errorf("invalid or duplicate Git object generation in trusted layout")
		}
		seen[mount.Generation] = true
		if mount.ObjectsHostDir != mount.ObjectsMountDir || !filepath.IsAbs(mount.ObjectsHostDir) {
			return fmt.Errorf("Git object generation source and destination must be the same absolute path")
		}
		info, statErr := os.Lstat(mount.ObjectsHostDir)
		if statErr != nil {
			return fmt.Errorf("inspect Git object generation %s: %w", mount.ObjectsHostDir, statErr)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("Git object generation is not a real directory: %s", mount.ObjectsHostDir)
		}
		canonical, evalErr := filepath.EvalSymlinks(mount.ObjectsHostDir)
		if evalErr != nil {
			return fmt.Errorf("canonicalize Git object generation %s: %w", mount.ObjectsHostDir, evalErr)
		}
		if canonical != mount.ObjectsHostDir {
			return fmt.Errorf("Git object generation is not canonical: %s", mount.ObjectsHostDir)
		}
	}
	alt, err := readRegularFileAnchored(managedRoot, filepath.Join(layout.CommonDir, "objects", "info", "alternates"), gitAlternatesMaxBytes)
	if err != nil {
		return fmt.Errorf("read private Git alternate: %w", err)
	}
	wantAlternates := make([]string, 0, len(layout.ObjectMounts))
	for _, mount := range layout.ObjectMounts {
		wantAlternates = append(wantAlternates, mount.ObjectsMountDir)
	}
	if strings.TrimSpace(string(alt)) != strings.Join(wantAlternates, "\n") {
		return fmt.Errorf("private Git alternate does not match trusted pool tip")
	}
	return nil
}

func validateRetainedPrivateClone(managedRoot, checkoutPath string) error {
	gitDir := filepath.Join(checkoutPath, ".git")
	info, err := os.Lstat(gitDir)
	if err != nil {
		return fmt.Errorf("missing trusted pooled-worktree layout record and private Git metadata: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("missing trusted pooled-worktree layout record for linked or unsafe Git metadata")
	}
	alt := filepath.Join(gitDir, "objects", "info", "alternates")
	if b, readErr := readRegularFileAnchored(managedRoot, alt, gitAlternatesMaxBytes); readErr == nil && strings.TrimSpace(string(b)) != "" {
		return fmt.Errorf("retained private clone has an untrusted object alternate")
	} else if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return fmt.Errorf("inspect retained private clone alternate: %w", readErr)
	}
	return nil
}

// RemoveManagedTree deletes exactly path beneath managedRoot without following
// session-controlled ancestors. It is used for the remainder of an agent tree
// after each repository's Git metadata has been reconciled.
func RemoveManagedTree(path, managedRoot, trashRoot string) error {
	return removeTreeAnchored(path, managedRoot, trashRoot)
}
