//go:build linux

// Package codexsocket maps Codex's protected Unix listener into an exact
// amux-session mount without sharing Codex's host-wide daemon socket directory.
package codexsocket

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"amux/internal/core"

	"golang.org/x/sys/unix"
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

// DialContext pins the socket inode for generated amux rendezvous paths before
// connecting. A session may replace entries in its writable socket directory;
// dialing a validated pathname would therefore leave a resolve/connect race.
// Custom endpoints outside the generated tree retain normal Unix-dial behavior.
func DialContext(ctx context.Context, rendezvous string) (net.Conn, error) {
	key, owned, err := layoutKey(rendezvous)
	if err != nil {
		return nil, err
	}
	if !owned {
		return (&net.Dialer{}).DialContext(ctx, "unix", rendezvous)
	}
	return dialManagedContext(ctx, rendezvous, key, nil)
}

// dialManagedContext exposes a deterministic post-pin barrier to package tests.
// The production path always supplies nil.
func dialManagedContext(ctx context.Context, rendezvous, key string, afterPin func()) (net.Conn, error) {
	info, err := os.Lstat(rendezvous)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		parent, err := existingPrivateDir(filepath.Dir(rendezvous))
		if err != nil {
			return nil, fmt.Errorf("validate Codex rendezvous directory: %w", err)
		}
		return dialPinnedContext(ctx, filepath.Join(parent, filepath.Base(rendezvous)), filepath.Join(filepath.Dir(parent), ".dial"), false, afterPin)
	}

	expected, err := ExpectedNamespacePath(rendezvous)
	if err != nil {
		return nil, err
	}
	target, err := os.Readlink(rendezvous)
	if err != nil {
		return nil, err
	}
	if target != expected {
		return nil, fmt.Errorf("Codex socket rendezvous %q targets %q, want exact session endpoint %q", rendezvous, target, expected)
	}

	root, err := existingPrivateDir(hostPhysicalRoot())
	if err != nil {
		return nil, fmt.Errorf("validate Codex physical socket root: %w", err)
	}
	source, err := existingPrivateDir(filepath.Join(root, physicalSessionKey(key)))
	if err != nil {
		return nil, fmt.Errorf("validate Codex session physical socket directory: %w", err)
	}
	physical := filepath.Join(source, filepath.Base(expected))
	return dialPinnedContext(ctx, physical, filepath.Join(root, ".dial"), true, afterPin)
}

// dialPinnedContext uses linkat without AT_SYMLINK_FOLLOW to select the current
// socket inode atomically. The hard link lives in a host-only directory on the
// same filesystem; after it is created, replacing either the source leaf or its
// directory cannot redirect connect(2). The established connection survives
// removal of the temporary link.
func dialPinnedContext(ctx context.Context, source, pinRoot string, require0600 bool, afterPin func()) (net.Conn, error) {
	pinRoot, err := ensurePrivateDir(pinRoot)
	if err != nil {
		return nil, fmt.Errorf("prepare Codex host dial directory: %w", err)
	}
	sourceFD, err := openPrivateDir(filepath.Dir(source))
	if err != nil {
		return nil, fmt.Errorf("open Codex socket directory: %w", err)
	}
	defer unix.Close(sourceFD)
	pinDir, err := os.MkdirTemp(pinRoot, "dial-")
	if err != nil {
		return nil, err
	}
	pinFD, err := openPrivateDir(pinDir)
	if err != nil {
		_ = os.Remove(pinDir)
		return nil, fmt.Errorf("open Codex host dial directory: %w", err)
	}
	defer func() {
		_ = unix.Unlinkat(pinFD, "socket", 0)
		_ = unix.Close(pinFD)
		_ = os.Remove(pinDir)
	}()
	if err := unix.Linkat(sourceFD, filepath.Base(source), pinFD, "socket", 0); err != nil {
		return nil, fmt.Errorf("pin Codex socket %q: %w", source, err)
	}
	var stat unix.Stat_t
	if err := unix.Fstatat(pinFD, "socket", &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, fmt.Errorf("validate pinned Codex socket: %w", err)
	}
	modeOK := !require0600 || stat.Mode&0o777 == 0o600
	if stat.Mode&unix.S_IFMT != unix.S_IFSOCK || !modeOK || stat.Uid != uint32(os.Geteuid()) {
		wantMode := ""
		if require0600 {
			wantMode = " mode-0600"
		}
		return nil, fmt.Errorf("Codex endpoint %q must pin a real, user-owned%s Unix socket", source, wantMode)
	}
	if afterPin != nil {
		afterPin()
	}
	return (&net.Dialer{}).DialContext(ctx, "unix", fmt.Sprintf("/proc/self/fd/%d/socket", pinFD))
}

func openPrivateDir(path string) (int, error) {
	fd, err := unix.Open(path, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = unix.Close(fd)
		return -1, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Mode&0o777 != 0o700 || stat.Uid != uint32(os.Geteuid()) {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("path %q must open as a user-owned mode-0700 directory", path)
	}
	return fd, nil
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
