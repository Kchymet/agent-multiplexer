package git

import (
	"fmt"
	"os"
	"path/filepath"
)

func layoutRecord(path string) string {
	return layoutVersion + filepath.Clean(path) + "\n"
}

func writeLayoutRecord(recordPath, checkoutPath string) error {
	if err := os.MkdirAll(filepath.Dir(recordPath), 0o700); err != nil {
		return err
	}
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
	if _, err := tmp.WriteString(layoutRecord(checkoutPath)); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, recordPath)
}

func hasLayoutRecord(recordPath, checkoutPath string) (bool, error) {
	info, err := os.Lstat(recordPath)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect checkout layout record: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("checkout layout record is not a regular file: %s", recordPath)
	}
	data, err := os.ReadFile(recordPath)
	if err != nil {
		return false, fmt.Errorf("read checkout layout record: %w", err)
	}
	if string(data) != layoutRecord(checkoutPath) {
		return false, fmt.Errorf("checkout layout record does not match %s", checkoutPath)
	}
	return true, nil
}

// RemoveManagedTree deletes exactly path beneath managedRoot without following
// session-controlled ancestors. It is used for the remainder of an agent tree
// after each repository's Git metadata has been reconciled.
func RemoveManagedTree(path, managedRoot, trashRoot string) error {
	return removeTreeAnchored(path, managedRoot, trashRoot)
}
