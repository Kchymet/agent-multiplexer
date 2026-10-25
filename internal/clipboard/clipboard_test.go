package clipboard

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
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
		{"wsl prefers clip.exe over WSLg", "linux", map[string]string{"DISPLAY": ":0", "WAYLAND_DISPLAY": "wayland-0"}, all, true, "/bin/clip.exe"},
		{"wsl falls back to powershell", "linux", nil, only("powershell.exe"), true, "/bin/powershell.exe"},
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
	ts := Tools("linux", env(nil), all, true)
	if !ts[0].UTF16 || ts[len(ts)-1].UTF16 {
		t.Errorf("clip.exe needs UTF-16 input and powershell does not: %+v", ts)
	}
}

func TestUTF16LE(t *testing.T) {
	if got, want := utf16LE([]byte("a✓")), []byte{0xff, 0xfe, 'a', 0, 0x13, 0x27}; !bytes.Equal(got, want) {
		t.Errorf("utf16LE = % x, want % x", got, want)
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
	clip := []Tool{{Argv: []string{"clip.exe"}, UTF16: true}, {Argv: []string{"powershell.exe"}}}

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
