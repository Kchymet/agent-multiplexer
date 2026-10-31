package clipboard

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
)

// toolTimeout bounds one clipboard tool run; powershell.exe's cold start is the
// slow case.
const toolTimeout = 5 * time.Second

// Copier delivers copied text to the system clipboard on two independent
// routes, because neither alone survives every setup:
//
//   - The terminal route re-emits OSC 52 to the terminal amux is drawing on, so
//     the copy lands on the machine the user is sitting at (over SSH, too).
//     Under tmux it goes through `tmux load-buffer -w`, which sets tmux's own
//     buffer and forwards OSC 52 outward per its set-clipboard option; a raw
//     OSC 52 from a pane is ignored at tmux's default (set-clipboard external).
//     If that command fails, a DCS-passthrough-wrapped OSC 52 is the fallback.
//   - The tool route pipes the text into the platform's clipboard command
//     (clip.exe on WSL, pbcopy, wl-copy, xclip, xsel), covering terminals and
//     tmux configurations that drop OSC 52.
//
// Both writing the same text is harmless. Copy never blocks: a single worker
// delivers the most recent copy, so a burst of selections settles on the last.
type Copier struct {
	out io.Writer // amux's own terminal; nil skips the terminal route
	env func(string) string
	run func(ctx context.Context, argv []string, stdin []byte) error
	// tools resolves the clipboard command candidates, first usable wins.
	tools func() []Tool

	mu      sync.Mutex
	pending []byte
	wake    chan struct{}
}

// Tool is one clipboard command: argv receives the text on stdin, re-encoded
// per Encoding.
type Tool struct {
	Argv     []string
	Encoding Encoding
}

// Encoding is how a Tool wants the UTF-8 text on its stdin.
type Encoding byte

const (
	// UTF8 passes the text through unchanged.
	UTF8 Encoding = iota
	// UTF16LE re-encodes it as UTF-16LE with no byte-order mark. clip.exe
	// reads non-UTF-16 input in the console's OEM code page, mangling
	// non-ASCII text, but it copies a leading BOM onto the clipboard
	// verbatim, so Windows pastes start with U+FEFF; without the BOM it
	// recognizes UTF-16 by its byte pattern (IsTextUnicode).
	UTF16LE
	// Base64 sends the UTF-8 text base64-encoded, so the bytes on the pipe
	// are pure ASCII and no console code page or BOM handling on the Windows
	// side can alter them; the receiving script decodes them back to UTF-8.
	Base64
)

// psSetClipboard decodes base64 UTF-8 from stdin and sets it as the Windows
// clipboard text. Reading ASCII sidesteps [Console]::InputEncoding entirely
// (whose UTF8 default object carries a BOM preamble), and Set-Clipboard takes
// a .NET string, so no BOM can reach the clipboard.
const psSetClipboard = "$b=[Console]::In.ReadToEnd().Trim(); " +
	"Set-Clipboard -Value ([Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($b)))"

// New returns a Copier for this process's environment that writes the terminal
// route to out. out must be safe to write concurrently with whatever else draws
// on the terminal (see SerialFile).
func New(out io.Writer) *Copier {
	return newCopier(out, os.Getenv, runTool, func() []Tool {
		return Tools(runtime.GOOS, os.Getenv, exec.LookPath, isWSL(os.Getenv))
	})
}

func newCopier(out io.Writer, env func(string) string, run func(context.Context, []string, []byte) error, tools func() []Tool) *Copier {
	c := &Copier{out: out, env: env, run: run, tools: tools, wake: make(chan struct{}, 1)}
	go c.loop()
	return c
}

// Copy queues text for delivery and returns at once. It is called from inside
// the emulator's parser, so it must not block or retain text past this call
// without copying it.
func (c *Copier) Copy(text []byte) {
	c.mu.Lock()
	c.pending = append(c.pending[:0:0], text...)
	c.mu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *Copier) loop() {
	for range c.wake {
		c.mu.Lock()
		text := c.pending
		c.pending = nil
		c.mu.Unlock()
		if len(text) > 0 {
			c.deliver(text)
		}
	}
}

// deliver runs both routes for one copy, synchronously.
func (c *Copier) deliver(text []byte) {
	// A byte-order mark is never meaningful copied text (a child that read a
	// BOM-prefixed file, or encoded one itself, would paste U+FEFF).
	text = bytes.TrimPrefix(text, []byte("\uFEFF"))
	if len(text) == 0 {
		return
	}
	if c.out != nil {
		tmux := c.env("TMUX") != ""
		if !tmux || c.runTimeout([]string{"tmux", "load-buffer", "-w", "-"}, text) != nil {
			_, _ = c.out.Write(Sequence(text, tmux))
		}
	}
	for _, t := range c.tools() {
		if c.runTimeout(t.Argv, t.Encoding.encode(text)) == nil {
			return
		}
	}
}

func (c *Copier) runTimeout(argv []string, stdin []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), toolTimeout)
	defer cancel()
	return c.run(ctx, argv, stdin)
}

// Tools lists the clipboard commands to try on this host, best first, keeping
// only those lookPath finds. WSL prefers the Windows clipboard directly over
// WSLg's X/Wayland bridge, whose sync to Windows is unreliable. PowerShell
// leads there because its base64 route is immune to Windows console
// encodings; clip.exe (faster to start, but heuristic about its input
// encoding) is the fallback.
func Tools(goos string, env func(string) string, lookPath func(string) (string, error), wsl bool) []Tool {
	var cand []Tool
	switch {
	case goos == "darwin":
		cand = append(cand, Tool{Argv: []string{"pbcopy"}})
	case wsl:
		ps := []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-Command", psSetClipboard}
		cand = append(cand,
			Tool{Argv: append([]string{"powershell.exe"}, ps...), Encoding: Base64},
			Tool{Argv: append([]string{"/mnt/c/Windows/System32/WindowsPowerShell/v1.0/powershell.exe"}, ps...), Encoding: Base64},
			Tool{Argv: []string{"clip.exe"}, Encoding: UTF16LE},
			Tool{Argv: []string{"/mnt/c/Windows/System32/clip.exe"}, Encoding: UTF16LE})
	}
	if goos != "darwin" {
		if env("WAYLAND_DISPLAY") != "" {
			cand = append(cand, Tool{Argv: []string{"wl-copy"}})
		}
		if env("DISPLAY") != "" {
			cand = append(cand,
				Tool{Argv: []string{"xclip", "-selection", "clipboard", "-in"}},
				Tool{Argv: []string{"xsel", "--clipboard", "--input"}})
		}
	}
	var out []Tool
	for _, t := range cand {
		path, err := lookPath(t.Argv[0])
		if err != nil {
			continue
		}
		t.Argv = append([]string{path}, t.Argv[1:]...)
		out = append(out, t)
	}
	return out
}

// isWSL reports whether this process runs under Windows Subsystem for Linux:
// the interop variables WSL sets, or failing those (a scrubbed environment)
// the kernel release string.
func isWSL(env func(string) string) bool {
	if env("WSL_DISTRO_NAME") != "" || env("WSL_INTEROP") != "" {
		return true
	}
	b, err := os.ReadFile("/proc/sys/kernel/osrelease")
	return err == nil && strings.Contains(strings.ToLower(string(b)), "microsoft")
}

// runTool runs argv with stdin and no other I/O. Stdout and stderr go to
// /dev/null rather than pipes: xclip and wl-copy fork a child that keeps
// serving the selection, and an inherited pipe would hold Wait open.
func runTool(ctx context.Context, argv []string, stdin []byte) error {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.WaitDelay = time.Second
	return cmd.Run()
}

func (e Encoding) encode(text []byte) []byte {
	switch e {
	case UTF16LE:
		return utf16LE(text)
	case Base64:
		return base64.StdEncoding.AppendEncode(nil, text)
	}
	return text
}

// utf16LE re-encodes UTF-8 text as UTF-16LE, without a byte-order mark.
func utf16LE(text []byte) []byte {
	units := utf16.Encode([]rune(string(text)))
	out := make([]byte, 0, 2*len(units))
	for _, u := range units {
		out = append(out, byte(u), byte(u>>8))
	}
	return out
}
