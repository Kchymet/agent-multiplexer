package clipboard

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"path"
	"reflect"
	"strings"
	"sync"
	"testing"
	"unicode/utf16"
)

func TestParseOSC52(t *testing.T) {
	for _, tc := range []struct {
		data string
		want string // "" means not a write amux acts on
	}{
		{"52;c;aGVsbG8=", "hello"},
		{"52;;aGVsbG8=", "hello"},   // empty target: xterm's default
		{"52;pc;aGVsbG8=", "hello"}, // clipboard among several targets
		{"52;s;aGVsbG8=", "hello"},  // the "selection" target
		{"52;c;aGVsbG8", "hello"},   // unpadded base64
		{"52;c;4pyTIMOp", "✓ é"},    // UTF-8 survives
		{"52;c;?", ""},              // query: never answered
		{"52;p;aGVsbG8=", ""},       // X primary only
		{"52;c;", ""},               // clear
		{"52;c;!!!", ""},            // malformed
		{"52", ""},                  // truncated
		{"520;c;aGVsbG8=", ""},      // different OSC
		{"2;52;c;aGVsbG8=", ""},     // title text that merely contains 52
	} {
		got, ok := ParseOSC52([]byte(tc.data))
		if ok != (tc.want != "") || string(got) != tc.want {
			t.Errorf("ParseOSC52(%q) = %q, %v; want %q", tc.data, got, ok, tc.want)
		}
	}
}

func TestSequence(t *testing.T) {
	if got := string(Sequence([]byte("hi"), false)); got != "\x1b]52;c;aGk=\a" {
		t.Errorf("plain = %q", got)
	}
	if got := string(Sequence([]byte("hi"), true)); got != "\x1bPtmux;\x1b\x1b]52;c;aGk=\a\x1b\\" {
		t.Errorf("tmux = %q", got)
	}
}

// Every split of the stream must strip the same way: the engine sees PTY reads
// cut at arbitrary bytes.
func TestStripperAcrossChunks(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"a\x1b]52;c;aGk=\ab", "ab"},                         // BEL-terminated
		{"a\x1b]52;c;aGk=\x1b\\b", "ab"},                     // ST-terminated
		{"a\x1b]52;c;aGk=\x1b[1mb", "a\x1b[1mb"},             // ESC ends it, starts a CSI
		{"a\x1b]52;c;aG\x18b", "ab"},                         // CAN cancels
		{"\x1b]0;t52;x\a\x1b]52;c;?\a", "\x1b]0;t52;x\a"},    // other OSCs survive
		{"\x1b]521;x\a\x1b]5;x\a", "\x1b]521;x\a\x1b]5;x\a"}, // near-miss numbers
		{"\x1b\x1b]52;c;aGk=\ax", "\x1bx"},                   // ESC ESC
		{"\x1b[31mred\x1b[0m", "\x1b[31mred\x1b[0m"},
		{"x\x1b]52;c;aGk=\x1b]52;c;aGk=\ay", "xy"}, // back to back, ESC-terminated
	} {
		for split := 0; split <= len(tc.in); split++ {
			var s Stripper
			got := s.Strip(nil, []byte(tc.in[:split]))
			got = s.Strip(got, []byte(tc.in[split:]))
			if string(got) != tc.want {
				t.Errorf("Strip(%q) split at %d = %q, want %q", tc.in, split, got, tc.want)
			}
		}
		// And one byte at a time.
		var s Stripper
		var got []byte
		for i := range len(tc.in) {
			got = s.Strip(got, []byte{tc.in[i]})
		}
		if string(got) != tc.want {
			t.Errorf("Strip(%q) bytewise = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestToolsSelection(t *testing.T) {
	env := func(vars map[string]string) func(string) string {
		return func(k string) string { return vars[k] }
	}
	all := func(name string) (string, error) {
		return "/bin/" + strings.TrimPrefix(name, "/mnt/c/Windows/System32/"), nil
	}
	only := func(names ...string) func(string) (string, error) {
		return func(name string) (string, error) {
			for _, n := range names {
				if n == name {
					return "/bin/" + n, nil
				}
			}
			return "", errors.New("not found")
		}
	}
	first := func(ts []Tool) string {
		if len(ts) == 0 {
			return ""
		}
		return ts[0].Argv[0]
	}
	for _, tc := range []struct {
		name string
		goos string
		env  map[string]string
		look func(string) (string, error)
		wsl  bool
		want string
	}{
		{"wsl prefers powershell over WSLg", "linux", map[string]string{"DISPLAY": ":0", "WAYLAND_DISPLAY": "wayland-0"}, all, true, "/bin/powershell.exe"},
		{"wsl falls back to clip.exe", "linux", nil, only("clip.exe"), true, "/bin/clip.exe"},
		{"wayland", "linux", map[string]string{"WAYLAND_DISPLAY": "wayland-0", "DISPLAY": ":0"}, all, false, "/bin/wl-copy"},
		{"x11 xclip", "linux", map[string]string{"DISPLAY": ":0"}, all, false, "/bin/xclip"},
		{"x11 xsel", "linux", map[string]string{"DISPLAY": ":0"}, only("xsel"), false, "/bin/xsel"},
		{"headless", "linux", nil, all, false, ""},
		{"display without tools", "linux", map[string]string{"DISPLAY": ":0"}, only(), false, ""},
		{"macos", "darwin", map[string]string{"DISPLAY": ":0"}, all, false, "/bin/pbcopy"},
	} {
		if got := first(Tools(tc.goos, env(tc.env), tc.look, tc.wsl)); got != tc.want {
			t.Errorf("%s: first tool = %q, want %q", tc.name, got, tc.want)
		}
	}
	for _, tl := range Tools("linux", env(nil), all, true) {
		name := path.Base(tl.Argv[0])
		switch name {
		case "powershell.exe":
			if tl.Encoding != Base64 {
				t.Errorf("powershell must read base64 (console-encoding proof): %+v", tl)
			}
			script := tl.Argv[len(tl.Argv)-1]
			// The old script set [Console]::InputEncoding to a BOM-carrying
			// UTF8 object; the base64 route must not depend on it.
			if strings.Contains(script, "InputEncoding") || !strings.Contains(script, "FromBase64String") || !strings.Contains(script, "Set-Clipboard") {
				t.Errorf("powershell script = %q", script)
			}
		case "clip.exe":
			if tl.Encoding != UTF16LE {
				t.Errorf("clip.exe needs UTF-16LE input: %+v", tl)
			}
		default:
			t.Errorf("unexpected WSL tool %q", name)
		}
	}
}

// Regression: clip.exe pastes a leading BOM into Windows as U+FEFF, so no
// encoding may emit one, and non-ASCII text must still round-trip.
func TestEncodingsCarryNoBOM(t *testing.T) {
	text := "a✓ é 日本 🙂"
	u16 := UTF16LE.encode([]byte(text))
	if bytes.HasPrefix(u16, []byte{0xff, 0xfe}) || bytes.HasPrefix(u16, []byte{0xfe, 0xff}) {
		t.Errorf("UTF16LE output starts with a BOM: % x", u16[:2])
	}
	if got, want := UTF16LE.encode([]byte("a✓")), []byte{'a', 0, 0x13, 0x27}; !bytes.Equal(got, want) {
		t.Errorf("UTF16LE(a✓) = % x, want % x", got, want)
	}
	units := make([]uint16, len(u16)/2)
	for i := range units {
		units[i] = uint16(u16[2*i]) | uint16(u16[2*i+1])<<8
	}
	if got := string(utf16.Decode(units)); got != text {
		t.Errorf("UTF16LE round trip = %q, want %q", got, text)
	}

	b64 := Base64.encode([]byte(text))
	for _, c := range b64 {
		if c >= 0x80 {
			t.Fatalf("Base64 output is not ASCII: %q", b64)
		}
	}
	dec, err := base64.StdEncoding.DecodeString(string(b64))
	if err != nil || string(dec) != text || bytes.HasPrefix(dec, []byte("\uFEFF")) {
		t.Errorf("Base64 round trip = %q, %v; want %q", dec, err, text)
	}
	if got := UTF8.encode([]byte(text)); string(got) != text {
		t.Errorf("UTF8 = %q", got)
	}
}

// A BOM in the copied text itself (an agent that read a BOM-prefixed file) is
// stripped before either route sees it.
func TestDeliverStripsLeadingBOM(t *testing.T) {
	tools := []Tool{{Argv: []string{"clip.exe"}, Encoding: UTF16LE}}
	c, out, runs := fakeCopier(nil, tools, nil)
	c.deliver([]byte("\uFEFFhé"))
	if out.String() != string(Sequence([]byte("hé"), false)) {
		t.Errorf("terminal got %q", out)
	}
	if len(*runs) != 1 || !bytes.Equal((*runs)[0].stdin, UTF16LE.encode([]byte("hé"))) {
		t.Errorf("runs = %+v", *runs)
	}

	c, out, runs = fakeCopier(nil, tools, nil)
	c.deliver([]byte("\uFEFF"))
	if out.Len() != 0 || len(*runs) != 0 {
		t.Errorf("a bare BOM should copy nothing: out %q, runs %+v", out, *runs)
	}
}

type runRecord struct {
	argv  []string
	stdin []byte
}

func fakeCopier(env map[string]string, tools []Tool, fail map[string]bool) (*Copier, *bytes.Buffer, *[]runRecord) {
	var out bytes.Buffer
	var runs []runRecord
	c := &Copier{
		out: &out,
		env: func(k string) string { return env[k] },
		run: func(_ context.Context, argv []string, stdin []byte) error {
			runs = append(runs, runRecord{argv, stdin})
			if fail[argv[0]] {
				return errors.New("failed")
			}
			return nil
		},
		tools: func() []Tool { return tools },
	}
	return c, &out, &runs
}

func TestDeliverRoutes(t *testing.T) {
	clip := []Tool{{Argv: []string{"clip.exe"}, Encoding: UTF16LE}, {Argv: []string{"powershell.exe"}}}

	t.Run("plain terminal emits OSC 52 and runs the first tool", func(t *testing.T) {
		c, out, runs := fakeCopier(nil, clip, nil)
		c.deliver([]byte("hi"))
		if out.String() != "\x1b]52;c;aGk=\a" {
			t.Errorf("terminal got %q", out)
		}
		if len(*runs) != 1 || (*runs)[0].argv[0] != "clip.exe" || !bytes.Equal((*runs)[0].stdin, utf16LE([]byte("hi"))) {
			t.Errorf("runs = %+v", *runs)
		}
	})
	t.Run("tool failure falls through to the next", func(t *testing.T) {
		c, _, runs := fakeCopier(nil, clip, map[string]bool{"clip.exe": true})
		c.deliver([]byte("hi"))
		if len(*runs) != 2 || string((*runs)[1].stdin) != "hi" {
			t.Errorf("runs = %+v", *runs)
		}
	})
	t.Run("under tmux load-buffer -w replaces the raw sequence", func(t *testing.T) {
		c, out, runs := fakeCopier(map[string]string{"TMUX": "/tmp/tmux-1/default,1,0"}, nil, nil)
		c.deliver([]byte("hi"))
		if out.Len() != 0 {
			t.Errorf("terminal got %q", out)
		}
		want := []runRecord{{[]string{"tmux", "load-buffer", "-w", "-"}, []byte("hi")}}
		if !reflect.DeepEqual(*runs, want) {
			t.Errorf("runs = %+v", *runs)
		}
	})
	t.Run("old tmux falls back to passthrough", func(t *testing.T) {
		c, out, _ := fakeCopier(map[string]string{"TMUX": "x"}, nil, map[string]bool{"tmux": true})
		c.deliver([]byte("hi"))
		if out.String() != "\x1bPtmux;\x1b\x1b]52;c;aGk=\a\x1b\\" {
			t.Errorf("terminal got %q", out)
		}
	})
}

// Copy hands off without blocking, and the worker delivers what was queued.
func TestCopyIsAsync(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	got := make(chan string, 4)
	var once sync.Once
	c := newCopier(nil, func(string) string { return "" },
		func(_ context.Context, _ []string, stdin []byte) error {
			once.Do(func() { close(started); <-release }) // the first delivery stalls
			got <- string(stdin)
			return nil
		},
		func() []Tool { return []Tool{{Argv: []string{"tool"}}} })
	c.Copy([]byte("one"))
	<-started
	buf := []byte("two")
	c.Copy(buf)  // returns although the worker is stuck
	buf[0] = 'X' // Copy must have taken its own copy
	c.Copy([]byte("three"))
	close(release)
	if first := <-got; first != "one" {
		t.Fatalf("first delivery = %q", first)
	}
	if last := <-got; last != "three" {
		t.Fatalf("queued burst should settle on the latest copy, got %q", last)
	}
}
