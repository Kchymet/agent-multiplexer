package clipboard

import (
	"io"
	"sync"
)

// File is the terminal file interface recognized by Bubble Tea. Retaining only
// io.Writer and Fd would lose TTY detection: it also requires Read and Close.
type File interface {
	io.ReadWriteCloser
	Fd() uintptr
}

// SerialFile wraps the file supplied to the UI renderer. All rendering and clipboard
// writes must use this same instance, never a separate handle to stdout.
// File is named rather than embedded: promoted WriteString or ReadFrom would
// let io.WriteString/io.Copy bypass the write lock.
type SerialFile struct {
	File File
	mu   sync.Mutex
}

func (o *SerialFile) Fd() uintptr                { return o.File.Fd() }
func (o *SerialFile) Read(p []byte) (int, error) { return o.File.Read(p) }

// Write holds the output lock across the entire write so a clipboard escape
// sequence cannot be inserted into a renderer's escape sequence (or vice versa).
func (o *SerialFile) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	n, err := o.File.Write(p)
	if n < len(p) && err == nil {
		err = io.ErrShortWrite
	}
	return n, err
}

func (o *SerialFile) Close() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.File.Close()
}
