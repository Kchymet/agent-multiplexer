//go:build linux || darwin

package git

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// openParentAnchored opens target's parent beneath managedRoot without
// following any session-controlled symlink component. managedRoot itself may be
// an intentional XDG symlink; it is resolved before its directory FD is opened.
func openParentAnchored(managedRoot, target string) (int, string, error) {
	rootAbs, err := filepath.Abs(filepath.Clean(managedRoot))
	if err != nil {
		return -1, "", err
	}
	targetAbs, err := filepath.Abs(filepath.Clean(target))
	if err != nil {
		return -1, "", err
	}
	rel, err := filepath.Rel(rootAbs, targetAbs)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return -1, "", fmt.Errorf("path %q is outside managed root %q", target, managedRoot)
	}
	base := filepath.Base(rel)
	parent := filepath.Dir(rel)
	resolvedRoot, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return -1, "", fmt.Errorf("resolve managed root: %w", err)
	}
	fd, err := unix.Open(resolvedRoot, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, "", fmt.Errorf("open managed root: %w", err)
	}
	for _, component := range strings.Split(parent, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		next, openErr := unix.Openat(fd, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		unix.Close(fd)
		if openErr != nil {
			return -1, "", fmt.Errorf("open managed path component %q: %w", component, openErr)
		}
		fd = next
	}
	return fd, base, nil
}

// mkdirAllAnchored creates target beneath managedRoot without following a
// symlink in any session-controlled component. Existing real directories are
// accepted. managedRoot itself may be an intentional canonicalized XDG link.
func mkdirAllAnchored(managedRoot, target string, perm os.FileMode) error {
	rootAbs, err := filepath.Abs(filepath.Clean(managedRoot))
	if err != nil {
		return err
	}
	targetAbs, err := filepath.Abs(filepath.Clean(target))
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(rootAbs, targetAbs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path %q is outside managed root %q", target, managedRoot)
	}
	resolvedRoot, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return fmt.Errorf("resolve managed root: %w", err)
	}
	fd, err := unix.Open(resolvedRoot, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open managed root: %w", err)
	}
	defer func() {
		if fd >= 0 {
			_ = unix.Close(fd)
		}
	}()
	for _, component := range strings.Split(rel, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		next, openErr := unix.Openat(fd, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if openErr != nil && errors.Is(openErr, unix.ENOENT) {
			if mkdirErr := unix.Mkdirat(fd, component, uint32(perm.Perm())); mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
				return fmt.Errorf("create managed path component %q: %w", component, mkdirErr)
			}
			next, openErr = unix.Openat(fd, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		}
		if openErr != nil {
			return fmt.Errorf("open managed path component %q: %w", component, openErr)
		}
		_ = unix.Close(fd)
		fd = next
	}
	return nil
}

// readRegularFileAnchored opens path relative to an already trusted managed
// root, never follows a symlink, never blocks on a FIFO/device, verifies the
// opened descriptor (not a preceding pathname stat), and bounds allocation.
func readRegularFileAnchored(managedRoot, path string, maxBytes int64) ([]byte, error) {
	parentFD, name, err := openParentAnchored(managedRoot, path)
	if err != nil {
		return nil, err
	}
	defer unix.Close(parentFD)
	fd, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		unix.Close(fd)
		return nil, fmt.Errorf("open anchored regular file: %s", path)
	}
	defer file.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, fmt.Errorf("path is not a regular file: %s", path)
	}
	if stat.Size < 0 || stat.Size > maxBytes {
		return nil, fmt.Errorf("regular file exceeds %d-byte limit: %s", maxBytes, path)
	}
	b, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > maxBytes {
		return nil, fmt.Errorf("regular file exceeds %d-byte limit: %s", maxBytes, path)
	}
	return b, nil
}

func publishCheckout(staged, destination, managedRoot string) error {
	dstFD, dstName, err := openParentAnchored(managedRoot, destination)
	if err != nil {
		return err
	}
	defer unix.Close(dstFD)
	src, err := os.Open(filepath.Dir(staged))
	if err != nil {
		return err
	}
	defer src.Close()
	// renameat is anchored to already-open directory FDs: replacing any path
	// ancestor after validation cannot redirect publication. The platform's
	// no-replace flag makes a racing final-component entry fail closed too.
	if err := renameNoReplace(int(src.Fd()), filepath.Base(staged), dstFD, dstName); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("destination already exists: %s", destination)
		}
		return err
	}
	return nil
}

func removeTreeAnchored(path, managedRoot, trashRoot string) error {
	srcFD, srcName, err := openParentAnchored(managedRoot, path)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return err
	}
	defer unix.Close(srcFD)
	if err := os.MkdirAll(trashRoot, 0o700); err != nil {
		return fmt.Errorf("create daemon-private removal root: %w", err)
	}
	trash, err := os.MkdirTemp(trashRoot, "remove-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(trash)
	dst, err := os.Open(trash)
	if err != nil {
		return err
	}
	defer dst.Close()
	if err := unix.Renameat(srcFD, srcName, int(dst.Fd()), "tree"); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return fmt.Errorf("detach managed path: %w", err)
	}
	if err := os.RemoveAll(filepath.Join(trash, "tree")); err != nil {
		return fmt.Errorf("remove detached managed path: %w", err)
	}
	return nil
}
