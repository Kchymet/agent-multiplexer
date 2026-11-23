package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSanitizeBrokerID(t *testing.T) {
	for in, want := range map[string]string{
		"devbox":                 "devbox",
		"  Kevin's MacBook Pro ": "kevin-s-macbook-pro",
		"build_01.example.com":   "build-01-example-com",
		"--x--":                  "x",
		"héllo/wörld":            "h-llo-w-rld",
		"":                       "",
		"...":                    "",
		// Would look like a legacy amux/<root>[-<agent>] branch: prefixed.
		"abc123":                       "b-abc123",
		"ABC123-mini":                  "b-abc123-mini",
		"deadbeef":                     "deadbeef", // 8 hex digits is not a session id
		"abc12":                        "abc12",
		"abc123x":                      "abc123x",
		strings.Repeat("a", 40):        strings.Repeat("a", maxBrokerIDLen),
		strings.Repeat("a", 31) + "-b": strings.Repeat("a", 31),
	} {
		if got := SanitizeBrokerID(in); got != want {
			t.Errorf("SanitizeBrokerID(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestResolveBrokerIDPersists pins the resolution order: the environment wins
// without being persisted; otherwise the persisted file; otherwise a new id is
// derived once and persisted, so later resolutions agree even if the hostname
// changes.
func TestResolveBrokerIDPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "broker-id")

	t.Setenv(BrokerIDEnv, "From Env")
	if got := resolveBrokerID(path); got != "from-env" {
		t.Fatalf("env override = %q, want from-env", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("env override was persisted (stat err %v)", err)
	}

	t.Setenv(BrokerIDEnv, "")
	first := resolveBrokerID(path)
	if first == "" || SanitizeBrokerID(first) != first {
		t.Fatalf("derived id %q is not a sanitized id", first)
	}
	b, err := os.ReadFile(path)
	if err != nil || strings.TrimSpace(string(b)) != first {
		t.Fatalf("persisted %q (err %v), want %q", b, err, first)
	}
	if again := resolveBrokerID(path); again != first {
		t.Fatalf("second resolution = %q, want persisted %q", again, first)
	}

	if err := os.WriteFile(path, []byte("Renamed Box\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := resolveBrokerID(path); got != "renamed-box" {
		t.Fatalf("edited file = %q, want renamed-box", got)
	}
}

func TestBrokerIDUnderTest(t *testing.T) {
	if os.Getenv(BrokerIDEnv) == "" && BrokerID() != TestBrokerID {
		t.Fatalf("BrokerID() in a test binary = %q, want %q", BrokerID(), TestBrokerID)
	}
}
