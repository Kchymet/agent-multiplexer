//go:build !linux && !darwin

package git

import (
	"fmt"
	"io"
	"os"
)

func publishCheckout(_, _, _ string) error {
	return fmt.Errorf("independent checkout publication is unsupported on this operating system")
}

func removeTreeAnchored(_, _, _ string) error {
	return fmt.Errorf("anchored checkout removal is unsupported on this operating system")
}

func mkdirAllAnchored(_, _ string, _ os.FileMode) error {
	return fmt.Errorf("anchored checkout directory creation is unsupported on this operating system")
}

func readRegularFileAnchored(_, path string, maxBytes int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > maxBytes {
		return nil, fmt.Errorf("path is not a bounded regular file: %s", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, maxBytes+1))
}
