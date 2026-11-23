// Package panefixture loads the captured and reconstructed agent pane screens
// in internal/composer/testdata/panes (see the README there) for tests of the
// composer classifier and of typed delivery. It is test support only.
package panefixture

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"amux/internal/vterm"
)

// Fixture is one pane screen with its expected classification.
type Fixture struct {
	Name       string
	Runtime    string
	Cols, Rows int
	Real       bool // provenance REAL (bytes a genuine runtime wrote)
	WantState  string
	WantBusy   bool
	WantText   string
	Stream     []byte
}

// Snapshot replays the fixture through the production emulator.
func (f Fixture) Snapshot() vterm.Snapshot { return vterm.Capture(f.Stream, f.Cols, f.Rows) }

// Dir is the fixtures directory.
func Dir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "testdata", "panes")
}

// All loads every fixture, sorted by name.
func All() ([]Fixture, error) {
	paths, err := filepath.Glob(filepath.Join(Dir(), "*.pane"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	var out []Fixture
	for _, p := range paths {
		f, err := load(p)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}

// Load loads one fixture by name (without the .pane suffix).
func Load(name string) (Fixture, error) {
	return load(filepath.Join(Dir(), name+".pane"))
}

const separator = "---8<---\n"

func load(path string) (Fixture, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Fixture{}, err
	}
	at := bytes.Index(raw, []byte(separator))
	if at < 0 {
		return Fixture{}, fmt.Errorf("%s: no %q line", path, strings.TrimSpace(separator))
	}
	f := Fixture{Name: strings.TrimSuffix(filepath.Base(path), ".pane")}
	header, body := string(raw[:at]), raw[at+len(separator):]
	encoding := ""
	for _, line := range strings.Split(header, "\n") {
		key, value, ok := strings.Cut(strings.TrimPrefix(line, "# "), ": ")
		if !ok || !strings.HasPrefix(line, "# ") {
			continue
		}
		switch key {
		case "runtime":
			f.Runtime = value
		case "size":
			if _, err := fmt.Sscanf(value, "%dx%d", &f.Cols, &f.Rows); err != nil {
				return Fixture{}, fmt.Errorf("%s: size %q: %v", path, value, err)
			}
		case "provenance":
			f.Real = strings.HasPrefix(value, "REAL")
		case "want-state":
			f.WantState = value
		case "want-busy":
			f.WantBusy = value == "true"
		case "want-text":
			if f.WantText, err = strconv.Unquote(value); err != nil {
				return Fixture{}, fmt.Errorf("%s: want-text: %v", path, err)
			}
		case "encoding":
			encoding = value
		}
	}
	switch encoding {
	case "raw":
		f.Stream = body
	case "escaped":
		rule := strings.Repeat("─", f.Cols)
		lines := strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
		var b strings.Builder
		for i, line := range lines {
			s, err := strconv.Unquote(`"` + strings.ReplaceAll(line, "{{RULE}}", rule) + `"`)
			if err != nil {
				return Fixture{}, fmt.Errorf("%s: body line %d: %v", path, i+1, err)
			}
			if i > 0 {
				b.WriteString("\r\n")
			}
			b.WriteString(s)
		}
		f.Stream = []byte(b.String())
	default:
		return Fixture{}, fmt.Errorf("%s: encoding %q", path, encoding)
	}
	if f.Runtime == "" || f.Cols == 0 || f.Rows == 0 || f.WantState == "" {
		return Fixture{}, fmt.Errorf("%s: incomplete header", path)
	}
	return f, nil
}
