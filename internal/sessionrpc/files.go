package sessionrpc

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const regularFileMode = 0o600

// openAbsoluteDirNoFollow walks an absolute path one component at a time. A
// concurrent ancestor replacement cannot redirect any already-open descriptor.
func openAbsoluteDirNoFollow(path string) (*os.File, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, fmt.Errorf("%w: directory path is not clean and absolute", ErrInvalidRecord)
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	current := os.NewFile(uintptr(fd), "/")
	for _, component := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if component == "" {
			continue
		}
		next, err := openDirAt(current, component)
		if err != nil {
			_ = current.Close()
			return nil, err
		}
		_ = current.Close()
		current = next
	}
	return current, nil
}

func openDirAt(parent *os.File, name string) (*os.File, error) {
	if !validComponent(name) {
		return nil, ErrInvalidRecord
	}
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

func ensureDirAt(parent *os.File, name string, mode uint32) (*os.File, error) {
	if !validComponent(name) {
		return nil, ErrInvalidRecord
	}
	if err := unix.Mkdirat(int(parent.Fd()), name, mode); err != nil && !errors.Is(err, unix.EEXIST) {
		return nil, err
	}
	dir, err := openDirAt(parent, name)
	if err != nil {
		return nil, err
	}
	if err := validatePrivateDir(dir); err != nil {
		_ = dir.Close()
		return nil, err
	}
	return dir, nil
}

func validatePrivateDir(dir *os.File) error {
	var stat unix.Stat_t
	if err := unix.Fstat(int(dir.Fd()), &stat); err != nil {
		return err
	}
	mode := uint32(stat.Mode)
	if stat.Uid != uint32(os.Geteuid()) || mode&unix.S_IFMT != unix.S_IFDIR || mode&0o7777 != 0o700 {
		return fmt.Errorf("%w: unsafe directory ownership or mode", ErrInvalidRecord)
	}
	return nil
}

func readRegularAt(dir *os.File, name string, max int) ([]byte, error) {
	return readRegularAtMode(dir, name, max, regularFileMode)
}

func readRegularAtMode(dir *os.File, name string, max int, mode uint32) ([]byte, error) {
	if !validComponent(name) || max <= 0 {
		return nil, ErrInvalidRecord
	}
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, err
	}
	statMode := uint32(stat.Mode)
	if statMode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 || stat.Uid != uint32(os.Geteuid()) || statMode&0o7777 != mode || stat.Size < 0 || stat.Size > int64(max) {
		return nil, fmt.Errorf("%w: unsafe regular file %q", ErrInvalidRecord, name)
	}
	data, err := io.ReadAll(io.LimitReader(file, int64(max)+1))
	if err != nil {
		return nil, err
	}
	var after unix.Stat_t
	if err := unix.Fstat(fd, &after); err != nil {
		return nil, err
	}
	if len(data) > max || int64(len(data)) != stat.Size || stat.Dev != after.Dev || stat.Ino != after.Ino ||
		stat.Size != after.Size || statModTime(stat) != statModTime(after) || statChangeTime(stat) != statChangeTime(after) {
		return nil, ErrInvalidRecord
	}
	return data, nil
}

func atomicWriteAt(dir *os.File, final string, data []byte, random io.Reader) error {
	if !validComponent(final) || len(data) == 0 || random == nil {
		return ErrInvalidRecord
	}
	var nonce [16]byte
	if _, err := io.ReadFull(random, nonce[:]); err != nil {
		return err
	}
	temporary := fmt.Sprintf(".tmp-%x", nonce[:])
	fd, err := unix.Openat(int(dir.Fd()), temporary, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, regularFileMode)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), temporary)
	cleanup := true
	defer func() {
		_ = file.Close()
		if cleanup {
			_ = unix.Unlinkat(int(dir.Fd()), temporary, 0)
		}
	}()
	if err := file.Chmod(regularFileMode); err != nil {
		return err
	}
	if err := writeFull(file, data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := renameNoReplace(int(dir.Fd()), temporary, int(dir.Fd()), final); err != nil {
		return err
	}
	cleanup = false
	return unix.Fsync(int(dir.Fd()))
}

func replaceFileAt(dir *os.File, final string, data []byte, random io.Reader) error {
	if !validComponent(final) || len(data) == 0 || random == nil {
		return ErrInvalidRecord
	}
	var nonce [16]byte
	if _, err := io.ReadFull(random, nonce[:]); err != nil {
		return err
	}
	temporary := fmt.Sprintf(".tmp-%x", nonce[:])
	fd, err := unix.Openat(int(dir.Fd()), temporary, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, regularFileMode)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), temporary)
	cleanup := true
	defer func() {
		_ = file.Close()
		if cleanup {
			_ = unix.Unlinkat(int(dir.Fd()), temporary, 0)
		}
	}()
	if err := file.Chmod(regularFileMode); err != nil {
		return err
	}
	if err := writeFull(file, data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := unix.Renameat(int(dir.Fd()), temporary, int(dir.Fd()), final); err != nil {
		return err
	}
	cleanup = false
	return unix.Fsync(int(dir.Fd()))
}

func writeFull(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if err != nil {
			return err
		}
		if written <= 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}

func listNames(dir *os.File, limit int) ([]string, bool, error) {
	if limit <= 0 {
		return nil, false, ErrInvalidRecord
	}
	// Use os.File.Seek rather than a raw lseek so os.File also discards its
	// buffered directory state before the next Readdirnames call.
	if _, err := dir.Seek(0, io.SeekStart); err != nil {
		return nil, false, err
	}
	names, err := dir.Readdirnames(limit + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false, err
	}
	overflow := len(names) > limit
	if overflow {
		names = names[:limit]
	}
	return names, overflow, nil
}

func unlinkAt(dir *os.File, name string) error {
	if !validComponent(name) {
		return ErrInvalidRecord
	}
	err := unix.Unlinkat(int(dir.Fd()), name, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err == nil {
		return unix.Fsync(int(dir.Fd()))
	}
	return err
}

func validComponent(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsRune(name, '/') && !strings.ContainsRune(name, 0)
}

func requestFileName(requestID string) string  { return requestID + ".req" }
func responseFileName(requestID string) string { return requestID + ".res" }

func requestIDFromFile(name string) (string, bool) {
	if !strings.HasSuffix(name, ".req") {
		return "", false
	}
	id := strings.TrimSuffix(name, ".req")
	return id, validRequestID(id)
}
