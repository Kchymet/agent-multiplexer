package selfupdate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ResolveTarget is the file an update replaces: the running executable with
// symlinks resolved, so a ~/.local/bin/amux symlink keeps pointing at the
// updated file rather than being replaced by a regular file.
func ResolveTarget() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(self)
}

// ReplaceFile atomically installs the contents of src at target with mode,
// re-verifying wantSHA over the exact bytes written. The new file is written
// and fsynced beside target (same directory, so same filesystem) and then
// renamed over it: a concurrent reader or exec sees either the old complete
// file or the new complete file, never a partial one, and processes already
// running the old binary keep their open inode. On any failure target is left
// untouched and the temporary file is removed.
func ReplaceFile(target, src string, mode os.FileMode, wantSHA string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	return ReplaceFromReader(target, in, mode, wantSHA)
}

// ReplaceFromReader is ReplaceFile for an in-memory or streamed source.
func ReplaceFromReader(target string, r io.Reader, mode os.FileMode, wantSHA string) (err error) {
	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(target)+".update-*")
	if err != nil {
		return fmt.Errorf("replace %s: %w", target, err)
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()
	h := sha256.New()
	if _, err = io.Copy(io.MultiWriter(tmp, h), r); err != nil {
		return fmt.Errorf("replace %s: write: %w", target, err)
	}
	if err = VerifySHA256(filepath.Base(target), hex.EncodeToString(h.Sum(nil)), wantSHA); err != nil {
		return err
	}
	if err = tmp.Chmod(mode); err != nil {
		return fmt.Errorf("replace %s: chmod: %w", target, err)
	}
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("replace %s: sync: %w", target, err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("replace %s: close: %w", target, err)
	}
	if err = os.Rename(tmpName, target); err != nil {
		return fmt.Errorf("replace %s: %w", target, err)
	}
	// Persist the directory entry too; best effort (not every platform
	// supports fsync on a directory handle).
	if d, derr := os.Open(dir); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// WriteFileAtomic replaces path with data (used for the shell shim).
func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	return ReplaceFromReader(path, bytes.NewReader(data), mode, hex.EncodeToString(sum[:]))
}

// Writable reports whether an update could replace target: its directory must
// accept new files (the rename happens there).
func Writable(target string) error {
	dir := filepath.Dir(target)
	f, err := os.CreateTemp(dir, ".amux-write-probe-*")
	if err != nil {
		return fmt.Errorf("cannot write to %s: %w", dir, err)
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}

// ProbeVersion runs `<binary> version --short`, which prints only the binary's
// own product version without contacting a daemon. It validates that a
// downloaded binary actually executes on this host, and identifies what is
// installed at the target path (which may differ from the running process).
func ProbeVersion(ctx context.Context, binary string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "version", "--short")
	cmd.Env = append(os.Environ(), "AMUX_SKIP=1")
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return "", fmt.Errorf("run %s version --short: %w: %s", binary, err, msg)
		}
		return "", fmt.Errorf("run %s version --short: %w", binary, err)
	}
	v := strings.TrimSpace(out.String())
	if v == "" || strings.ContainsAny(v, " \n\t") {
		return "", errors.New("unexpected version --short output: " + v)
	}
	return v, nil
}
