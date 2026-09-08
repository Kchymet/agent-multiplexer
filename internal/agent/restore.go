package agent

import (
	"errors"
	"io/fs"
	"os"

	"amux/internal/core"
	"amux/internal/hostprep"
)

// restoreCapturedRooted applies core's append-only transcript freshness rule,
// but publishes into the private harness home through the pinned session root.
// The trusted captured backup streams into an unpredictable temporary file, so
// large intentional histories are neither memory-bounded nor exposed to a
// predictable destination temp alias.
func restoreCapturedRooted(root *hostprep.Root, sessionID, dst string) (bool, error) {
	src, srcSize, ok := core.CapturedTranscript(sessionID)
	if !ok || dst == "" {
		return false, nil
	}
	rel, err := root.Rel(dst)
	if err != nil {
		return false, err
	}
	dstInfo, err := root.StatFile(rel)
	switch {
	case err == nil:
		if srcSize < dstInfo.Size() {
			return false, nil
		}
		if srcSize == dstInfo.Size() {
			srcInfo, statErr := os.Stat(src)
			if statErr != nil || !srcInfo.ModTime().After(dstInfo.ModTime()) {
				return false, statErr
			}
		}
	case hostprep.IsUnsafe(err):
		return false, err
	case !errors.Is(err, fs.ErrNotExist):
		return false, err
	}
	f, err := os.Open(src)
	if err != nil {
		return false, err
	}
	defer f.Close()
	if err := root.AtomicWriteFrom(rel, f, 0o644); err != nil {
		return false, err
	}
	return true, nil
}
