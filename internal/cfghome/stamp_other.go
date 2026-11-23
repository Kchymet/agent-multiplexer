//go:build !darwin && !linux

package cfghome

import "io/fs"

// stampOf reports no stamp where ctime and inode are not portable, so cached
// hashes are never reused there: every scan reads and hashes every file.
func stampOf(fs.FileInfo) (fileStamp, bool) { return fileStamp{}, false }
