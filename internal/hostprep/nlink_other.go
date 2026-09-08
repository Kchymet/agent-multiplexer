//go:build !unix

package hostprep

import "io/fs"

// Platforms without a portable link count receive os.Root containment but do
// not claim the Unix hard-link rejection property.
func linkCount(fs.FileInfo) uint64 { return 0 }
