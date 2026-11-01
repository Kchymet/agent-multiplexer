//go:build !linux && !darwin

package accounts

import "os"

// changeTime is unavailable here; sameStoreFile then never trusts the cache.
func changeTime(os.FileInfo) (int64, bool) { return 0, false }
