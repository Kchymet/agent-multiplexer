// Package termoutput serializes UI rendering and explicit clipboard writes to
// the same terminal, preserving the file interface used for terminal sizing.
package termoutput

import (
	"io"
	"sync"

	"github.com/charmbracelet/x/ansi"
)

// File is the terminal file interface recognized by Bubble Tea. Retaining only
// io.Writer and Fd would lose TTY detection: it also requires Read and Close.
type File interface {
	io.ReadWriteCloser
	Fd() uintptr
}

// Output wraps the file supplied to the UI renderer. All rendering and clipboard
// writes must use this same instance, never a separate handle to stdout.
type Output struct {
	mu   sync.Mutex
	file File
}

func New(file File) *Output { return &Output{file: file} }

func (o *Output) Fd() uintptr                { return o.file.Fd() }
func (o *Output) Read(p []byte) (int, error) { return o.file.Read(p) }

// Write holds the output lock across the entire write so a clipboard escape
// sequence cannot be inserted into a renderer's escape sequence (or vice versa).
func (o *Output) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	n, err := o.file.Write(p)
	if n < len(p) && err == nil {
		err = io.ErrShortWrite
	}
	return n, err
}

func (o *Output) Close() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.file.Close()
}

// Copy sends a write-only OSC52 request for explicitly selected text. Call this
// only on the user's completed selection/copy action, never during rendering or
// in response to child output. Empty text does nothing, preserving the clipboard.
// Success means the request was written; the outer terminal may deny it or lack
// OSC52 support. No clipboard contents are requested or read.
func (o *Output) Copy(text string) error {
	if text == "" {
		return nil
	}
	_, err := o.Write([]byte(ansi.SetSystemClipboard(text)))
	return err
}
