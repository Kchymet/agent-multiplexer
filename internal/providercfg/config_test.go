package providercfg

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// full is a config with every field set, so a round trip that drops or mangles
// one fails rather than passing on the fields that happen to be exercised.
func full() Config {
	return Config{
		Harnesses:        []string{"claude", "codex"},
		IdentityMode:     "machine",
		Orchestrator:     "orch.example.com:7443",
		TokenFile:        "/home/u/.config/amux/provider.token",
		Name:             "laptop",
		CAFile:           "/etc/ssl/private-ca.pem",
		ServerName:       "orch.internal",
		MaxPanes:         8,
		PublishSessions:  true,
		ReadOnlySessions: true,
		RuntimeEvents:    true,
		Labels:           map[string]string{"zone": "home", "gpu": "none"},
		Features:         []string{"bigdisk", "cuda"},
	}
}

// TestRoundTrip is the contract that keeps Marshal and Parse in sync: a field
// added to Config but forgotten in one half fails here instead of silently
// dropping a setting the user typed.
func TestRoundTrip(t *testing.T) {
	got, err := Parse(full().Marshal())
	if err != nil {
		t.Fatalf("Parse: %v\n%s", err, full().Marshal())
	}
	if !reflect.DeepEqual(got, full()) {
		t.Errorf("round trip changed the config:\n got %+v\nwant %+v", got, full())
	}
}

// TestMarshalIsDeterministic pins that rewriting an unchanged config leaves the
// file byte-identical, so a diff shows only what the user actually changed.
func TestMarshalIsDeterministic(t *testing.T) {
	first := string(full().Marshal())
	for i := 0; i < 5; i++ {
		if got := string(full().Marshal()); got != first {
			t.Fatalf("Marshal is not deterministic:\n%s\n---\n%s", first, got)
		}
	}
	if !strings.Contains(first, "features = [\"bigdisk\", \"cuda\"]") {
		t.Errorf("features are not sorted:\n%s", first)
	}
	if strings.Index(first, "gpu =") > strings.Index(first, "zone =") {
		t.Errorf("labels are not sorted:\n%s", first)
	}
}

// TestMarshalCarriesNoToken is the security property of the whole design: the
// config file names the token file, never the credential in it.
func TestMarshalCarriesNoToken(t *testing.T) {
	out := string(full().Marshal())
	if !strings.Contains(out, `token-file = "/home/u/.config/amux/provider.token"`) {
		t.Errorf("config does not name the token file:\n%s", out)
	}
	if strings.Contains(out, "token =") {
		t.Errorf("config file holds a bare token key:\n%s", out)
	}
}

// TestMarshalOmitsEmpty keeps an unconfigured field out of the file entirely,
// rather than writing `name = ""` and making "unset" indistinguishable from
// "set to empty".
func TestMarshalOmitsEmpty(t *testing.T) {
	out := string(Config{Orchestrator: "h:1"}.Marshal())
	var keys []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue // the header prose is not a setting
		}
		keys = append(keys, line)
	}
	if !reflect.DeepEqual(keys, []string{"orchestrator = \"h:1\""}) {
		t.Errorf("empty config wrote %q, want only the orchestrator:\n%s", keys, out)
	}
	got, err := Parse([]byte(out))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !reflect.DeepEqual(got, Config{Orchestrator: "h:1"}) {
		t.Errorf("parsed %+v, want only the orchestrator", got)
	}
}

func TestParseHandComposed(t *testing.T) {
	in := `
# a hand-edited file
orchestrator = "orch:7443"   # the home box
token-file = "/tmp/tok"
max-panes = 4
publish-sessions = true

[labels]
tag = "has # hash"
`
	got, err := Parse([]byte(in))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := Config{
		Orchestrator:    "orch:7443",
		TokenFile:       "/tmp/tok",
		MaxPanes:        4,
		PublishSessions: true,
		Labels:          map[string]string{"tag": "has # hash"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Parse = %+v, want %+v", got, want)
	}
}

// TestParseRejects: a config amux cannot read is an error the user sees, never a
// setting silently dropped.
func TestParseRejects(t *testing.T) {
	for name, in := range map[string]string{
		"unknown key":     `orchestraitor = "h:1"`,
		"unknown table":   "[server]\nname = \"x\"",
		"missing value":   "name =",
		"no equals":       "orchestrator",
		"wrong type":      "max-panes = true",
		"string for bool": `publish-sessions = "yes"`,
		"unquoted string": "name = laptop",
		"bad escape":      `name = "a\qb"`,
		"open table":      "[labels",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(in)); err == nil {
				t.Errorf("Parse(%q) = nil error, want a complaint", in)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	for name, tc := range map[string]struct {
		cfg     Config
		wantErr string
	}{
		"ok":              {full(), ""},
		"no orchestrator": {Config{TokenFile: "/tmp/t"}, "orchestrator"},
		"no token file":   {Config{Orchestrator: "h:1"}, "token file"},
		"events need publish": {
			Config{Orchestrator: "h:1", TokenFile: "/tmp/t", RuntimeEvents: true},
			"--runtime-events requires",
		},
		"read-only needs publish": {
			Config{Orchestrator: "h:1", TokenFile: "/tmp/t", ReadOnlySessions: true},
			"--read-only-sessions requires",
		},
		"unknown harness": {
			Config{Orchestrator: "h:1", TokenFile: "/tmp/t", Harnesses: []string{"missing"}},
			"unknown harness",
		},
		"cloud identity": {
			Config{Orchestrator: "h:1", TokenFile: "/tmp/t", IdentityMode: "api-key"},
			"identity-mode=machine",
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := tc.cfg.Validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("Validate = %v, want nil", err)
			case tc.wantErr != "" && err == nil:
				t.Errorf("Validate = nil, want an error mentioning %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("Validate = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestNormalizeHarnesses(t *testing.T) {
	if got, want := NormalizeHarnesses([]string{"claude", "auto", " codex "}), []string{"codex"}; !reflect.DeepEqual(got, want) {
		t.Errorf("NormalizeHarnesses = %v, want %v", got, want)
	}
	if got := NormalizeHarnesses([]string{"claude", "auto"}); len(got) != 0 {
		t.Errorf("NormalizeHarnesses ending in auto = %v, want automatic discovery", got)
	}
}

func TestSaveLoad(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if _, err := Load(); !os.IsNotExist(err) {
		t.Fatalf("Load with no file = %v, want a not-exist error (provider mode is opt-in)", err)
	}
	if err := Save(full()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if got := filepath.Base(Path()); got != "provider.toml" {
		t.Errorf("Path() basename = %q, want provider.toml", got)
	}
	got, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(got, full()) {
		t.Errorf("Load = %+v, want %+v", got, full())
	}
}
