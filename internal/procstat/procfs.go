package procstat

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// procStat is the part of /proc/<pid>/stat the sampler uses.
type procStat struct {
	pid      int
	ppid     int
	comm     string
	ticks    uint64 // utime+stime, in clock ticks, across all threads
	start    uint64 // starttime, clock ticks after boot (pid-reuse identity)
	threads  int
	rssPages uint64
}

// parseStat parses /proc/<pid>/stat. comm (field 2) is parenthesized and may
// itself contain spaces and parentheses, so fields are counted from the LAST
// ')'.
func parseStat(b []byte) (procStat, error) {
	var p procStat
	open := bytes.IndexByte(b, '(')
	end := bytes.LastIndexByte(b, ')')
	if open < 1 || end < open {
		return p, errors.New("malformed stat")
	}
	pid, err := strconv.Atoi(string(bytes.TrimSpace(b[:open])))
	if err != nil {
		return p, errors.New("malformed stat pid")
	}
	p.pid = pid
	p.comm = string(b[open+1 : end])
	// Fields after ')': [0]=state(3) [1]=ppid(4) ... [11]=utime(14)
	// [12]=stime(15) [17]=num_threads(20) [19]=starttime(22) [21]=rss(24).
	var f [22][]byte
	n := 0
	rest := b[end+1:]
	for n < len(f) {
		rest = bytes.TrimLeft(rest, " \n")
		if len(rest) == 0 {
			break
		}
		i := bytes.IndexAny(rest, " \n")
		if i < 0 {
			i = len(rest)
		}
		f[n] = rest[:i]
		rest = rest[i:]
		n++
	}
	if n < len(f) {
		return p, errors.New("short stat")
	}
	num := func(i int) uint64 {
		v, e := strconv.ParseUint(string(f[i]), 10, 64)
		if e != nil {
			err = e
		}
		return v
	}
	p.ppid = int(num(1))
	p.ticks = num(11) + num(12)
	p.threads = int(num(17))
	p.start = num(19)
	p.rssPages = num(21)
	if err != nil {
		return procStat{}, errors.New("malformed stat field")
	}
	return p, nil
}

// reader reads small procfs files into a reused buffer. Not safe for
// concurrent use; the sampler serializes samples.
type reader struct {
	root string
	buf  []byte
}

// read returns the first max bytes of root/rel. The slice is only valid until
// the next read. It uses raw syscalls rather than os.Open: an *os.File
// registers with the runtime poller and a finalizer, which is most of the cost
// of reading a tiny procfs file.
func (r *reader) read(rel string, max int) ([]byte, error) {
	fd, err := openRead(r.root + "/" + rel)
	if err != nil {
		return nil, err
	}
	defer syscall.Close(fd)
	if cap(r.buf) < max {
		r.buf = make([]byte, max)
	}
	buf := r.buf[:max]
	n := 0
	for n < max {
		m, err := syscall.Read(fd, buf[n:])
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return nil, err
		}
		if m == 0 {
			break
		}
		n += m
	}
	return buf[:n], nil
}

func openRead(path string) (int, error) {
	for {
		fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
		if err != syscall.EINTR {
			return fd, err
		}
	}
}

func (r *reader) stat(pid string) (procStat, error) {
	b, err := r.read(pid+"/stat", 1024)
	if err != nil {
		return procStat{}, err
	}
	return parseStat(b)
}

// cmdline returns the NUL-separated argv, split. Empty for kernel threads and
// zombies.
func (r *reader) cmdline(pid string) []string {
	b, err := r.read(pid+"/cmdline", 16<<10)
	if err != nil || len(b) == 0 {
		return nil
	}
	b = bytes.TrimRight(b, "\x00")
	parts := bytes.Split(b, []byte{0})
	argv := make([]string, len(parts))
	for i, p := range parts {
		argv[i] = string(p)
	}
	return argv
}

func (r *reader) cwd(pid string) string {
	s, _ := os.Readlink(filepath.Join(r.root, pid, "cwd"))
	return s
}

// pss reads Pss from smaps_rollup, in bytes; ok is false when unreadable.
func (r *reader) pss(pid int) (uint64, bool) {
	b, err := r.read(strconv.Itoa(pid)+"/smaps_rollup", 4096)
	if err != nil {
		return 0, false
	}
	return kbField(b, "Pss:")
}

// uptimeSeconds reads the first field of /proc/uptime.
func (r *reader) uptimeSeconds() (float64, bool) {
	b, err := r.read("uptime", 128)
	if err != nil {
		return 0, false
	}
	if i := bytes.IndexByte(b, ' '); i > 0 {
		b = b[:i]
	}
	v, err := strconv.ParseFloat(string(bytes.TrimSpace(b)), 64)
	return v, err == nil
}

func (r *reader) memTotal() uint64 {
	b, err := r.read("meminfo", 4096)
	if err != nil {
		return 0
	}
	v, _ := kbField(b, "MemTotal:")
	return v
}

// kbField finds a "Name:   123 kB" line and returns it in bytes.
func kbField(b []byte, name string) (uint64, bool) {
	for len(b) > 0 {
		line := b
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			line, b = b[:i], b[i+1:]
		} else {
			b = nil
		}
		if !bytes.HasPrefix(line, []byte(name)) {
			continue
		}
		fields := bytes.Fields(line[len(name):])
		if len(fields) == 0 {
			return 0, false
		}
		v, err := strconv.ParseUint(string(fields[0]), 10, 64)
		if err != nil {
			return 0, false
		}
		return v * 1024, true
	}
	return 0, false
}

// isPID reports whether a /proc entry name is a process directory.
func isPID(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		if name[i] < '0' || name[i] > '9' {
			return false
		}
	}
	return true
}
