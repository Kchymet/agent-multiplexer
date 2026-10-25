//go:build linux

// Package codexsocket maps Codex's protected Unix listener into an exact
// amux-session mount without sharing Codex's host-wide daemon socket directory.
package codexsocket

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"amux/internal/core"
)

const (
	rendezvousName  = "cx.sock"
	sessionKeyBytes = 8
)

// PrepareNamespaceMount creates and validates the host directory that one
// session's Codex process may use for its physical listener. The caller mounts
// source at target inside that session's private /tmp. No host
// /tmp/codex-daemon-* directory is used or exposed.
func PrepareNamespaceMount(rendezvous string) (source, target string, err error) {
	key, owned, err := layoutKey(rendezvous)
	if err != nil {
		return "", "", err
	}
	if !owned {
		return "", "", fmt.Errorf("codex socket rendezvous %q is outside the amux socket tree", rendezvous)
	}
	root, err := ensurePrivateDir(hostPhysicalRoot())
	if err != nil {
		return "", "", fmt.Errorf("prepare Codex physical socket root: %w", err)
	}
	source, err = ensurePrivateDir(filepath.Join(root, physicalSessionKey(key)))
	if err != nil {
		return "", "", fmt.Errorf("prepare Codex session physical socket directory: %w", err)
	}
	return source, sharedDaemonDir(), nil
}

// ResolveDialPath preserves direct sockets used by Codex <=0.154. When a
// generated amux rendezvous is a symlink (Codex >=0.157), it accepts only the
// exact deterministic alias Codex derives for that rendezvous and maps it into
// the corresponding session's private host directory.
func ResolveDialPath(rendezvous string) (string, error) {
	info, err := os.Lstat(rendezvous)
	if errors.Is(err, os.ErrNotExist) {
		return rendezvous, nil
	}
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return rendezvous, nil
	}

	key, owned, err := layoutKey(rendezvous)
	if err != nil {
		return "", err
	}
	if !owned {
		// Custom unix:// endpoints historically followed ordinary filesystem
		// symlinks. Only generated amux rendezvous paths use this translation.
		return rendezvous, nil
	}
	expected, err := ExpectedNamespacePath(rendezvous)
	if err != nil {
		return "", err
	}
	target, err := os.Readlink(rendezvous)
	if err != nil {
		return "", err
	}
	if target != expected {
		return "", fmt.Errorf("Codex socket rendezvous %q targets %q, want exact session endpoint %q", rendezvous, target, expected)
	}

	root, err := existingPrivateDir(hostPhysicalRoot())
	if err != nil {
		return "", fmt.Errorf("validate Codex physical socket root: %w", err)
	}
	source, err := existingPrivateDir(filepath.Join(root, physicalSessionKey(key)))
	if err != nil {
		return "", fmt.Errorf("validate Codex session physical socket directory: %w", err)
	}
	physical := filepath.Join(source, filepath.Base(expected))
	physicalInfo, err := os.Lstat(physical)
	if err != nil {
		return "", fmt.Errorf("validate Codex physical socket: %w", err)
	}
	physicalStat, ok := physicalInfo.Sys().(*syscall.Stat_t)
	if physicalInfo.Mode()&os.ModeSocket == 0 || physicalInfo.Mode()&os.ModeSymlink != 0 ||
		physicalInfo.Mode().Perm() != 0o600 || !ok || physicalStat.Uid != uint32(os.Geteuid()) {
		return "", fmt.Errorf("Codex physical endpoint %q must be a real, user-owned mode-0600 Unix socket", physical)
	}
	return physical, nil
}

// ExpectedNamespacePath implements Codex 0.157's Linux protected_socket_path:
// SHA256(canonical requested parent + requested basename), under the fixed
// per-user shared-daemon directory. In amux's namespace, the clean rendezvous
// parent is a bind-mount destination and therefore already canonical there.
func ExpectedNamespacePath(rendezvous string) (string, error) {
	_, owned, err := layoutKey(rendezvous)
	if err != nil {
		return "", err
	}
	if !owned {
		return "", fmt.Errorf("codex socket rendezvous %q is outside the amux socket tree", rendezvous)
	}
	hash := sha256.Sum256([]byte(rendezvous))
	return filepath.Join(sharedDaemonDir(), fmt.Sprintf("%x", hash)), nil
}

func sharedDaemonDir() string {
	return filepath.Join("/tmp", fmt.Sprintf("codex-daemon-%d", os.Geteuid()))
}

// Keep the host path short enough that appending Codex's 64-byte socket name
// stays below Linux's sockaddr_un limit. This is an amux-private root, distinct
// from Codex's host daemon directory; panes receive only one keyed child.
func hostPhysicalRoot() string {
	return filepath.Join("/tmp", fmt.Sprintf("amux-cx-%d", os.Geteuid()))
}

// Include the amux data instance as well as the session key so isolated daemon
// instances with the same session id cannot share a physical directory.
func physicalSessionKey(sessionKey string) string {
	hash := sha256.Sum256([]byte(filepath.Clean(core.DataDir()) + "\x00" + sessionKey))
	return fmt.Sprintf("%x", hash[:sessionKeyBytes])
}

// layoutKey returns owned=true for paths lexically below amux's rendezvous root.
// Once a path claims that tree, malformed aliases fail closed rather than
// falling back to normal symlink following.
func layoutKey(rendezvous string) (key string, owned bool, err error) {
	root := filepath.Join(core.DataDir(), "cx")
	clean := filepath.Clean(rendezvous)
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return "", false, fmt.Errorf("Codex socket root %q is not absolute and clean", root)
	}
	rel, relErr := filepath.Rel(root, clean)
	if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false, nil
	}
	owned = true
	if !filepath.IsAbs(rendezvous) || clean != rendezvous {
		return "", true, fmt.Errorf("Codex socket rendezvous %q is not absolute and clean", rendezvous)
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 2 || parts[1] != rendezvousName || !validSessionKey(parts[0]) {
		return "", true, fmt.Errorf("Codex socket rendezvous %q does not have the exact session layout", rendezvous)
	}
	return parts[0], true, nil
}

func validSessionKey(key string) bool {
	if len(key) != sessionKeyBytes*2 {
		return false
	}
	for _, c := range key {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func ensurePrivateDir(path string) (string, error) {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return "", err
	}
	return existingPrivateDir(path)
}

func existingPrivateDir(path string) (string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", fmt.Errorf("path %q is not absolute and clean", path)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		return "", fmt.Errorf("path %q must be a real mode-0700 directory", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return "", fmt.Errorf("path %q must be owned by uid %d", path, os.Geteuid())
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	canonicalInfo, err := os.Lstat(canonical)
	if err != nil {
		return "", err
	}
	if !canonicalInfo.IsDir() || canonicalInfo.Mode()&os.ModeSymlink != 0 || canonicalInfo.Mode().Perm() != 0o700 {
		return "", fmt.Errorf("canonical path %q must be a real mode-0700 directory", canonical)
	}
	canonicalStat, ok := canonicalInfo.Sys().(*syscall.Stat_t)
	if !ok || canonicalStat.Uid != uint32(os.Geteuid()) {
		return "", fmt.Errorf("canonical path %q must be owned by uid %d", canonical, os.Geteuid())
	}
	return canonical, nil
}
