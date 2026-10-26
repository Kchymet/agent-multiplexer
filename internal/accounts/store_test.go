package accounts

import (
	"amux/internal/centralauth"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func isolated(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
}
func TestCorruptMetadataFailsClosed(t *testing.T) {
	isolated(t)
	os.MkdirAll(filepath.Dir(Path()), 0700)
	os.WriteFile(Path(), []byte(`{"version":99}`), 0600)
	if e := Bind("w", "codex"); e == nil {
		t.Fatal("corrupt store accepted")
	}
}
func TestClaudeObservationProvenanceAndEpoch(t *testing.T) {
	isolated(t)
	Bind("w", "claude")
	b, p, _ := Delivery("w", "claude")
	l, _ := BeginLaunch("w", "claude")
	Accept("w", b.Revision, p.DeliveryEpoch, "", 1, l)
	raw := []byte(`{"five_hour":{"used_percentage":42,"resets_at":1892900800}}`)
	if e := ObserveClaude("w", "runtime-1", raw); e != nil {
		t.Fatal(e)
	}
	s, _ := Read()
	q := s.Profiles[p.ID].Limits
	if q.State != "known" || q.SessionID != "w" || q.RuntimeGeneration != "runtime-1" || q.DeliveryEpoch != p.DeliveryEpoch {
		t.Fatalf("provenance missing: %+v", q)
	}
	Update(func(s *State) error {
		p := s.Profiles[b.ProfileID]
		p.DeliveryEpoch++
		s.Profiles[p.ID] = p
		return nil
	})
	if e := ObserveClaude("w", "runtime-1", raw); !errors.Is(e, ErrStale) {
		t.Fatal("stale runtime quota accepted", e)
	}
}

func TestMigrationCapturesPreexistingClaudeCredential(t *testing.T) {
	isolated(t)
	// A central credential stored before machine profiles existed counts as a
	// configured central login, and a later replacement serves the next launch.
	token, _ := centralauth.NewSecret("LEGACY-OPAQUE-TOKEN")
	old, err := centralauth.ClaudeStore().Save(token, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err = Bind("legacy", "claude"); err != nil {
		t.Fatal(err)
	}
	s, _ := Read()
	if s.CredentialVersion["claude:central"] != old.Generation || !s.centralConfigured("claude") {
		t.Fatal("preexisting credential not captured")
	}
	b, p, _ := Delivery("legacy", "claude")
	running, _ := BeginLaunch("legacy", "claude")
	if err = Accept("legacy", b.Revision, p.DeliveryEpoch, "", old.Generation, running); err != nil {
		t.Fatal(err)
	}
	token, _ = centralauth.NewSecret("REPLACEMENT-OPAQUE-TOKEN")
	next, err := centralauth.ClaudeStore().Save(token, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// The login completion commits the replacement's version with it.
	if err = Update(func(s *State) error { s.CredentialVersion["claude:central"] = next.Generation; return nil }); err != nil {
		t.Fatal(err)
	}
	if err = Accept("legacy", b.Revision, p.DeliveryEpoch, "", next.Generation, running); !errors.Is(err, ErrSuperseded) {
		t.Fatal("running Claude process received the replacement", err)
	}
	relaunch, _ := BeginLaunch("legacy", "claude")
	if err = Accept("legacy", b.Revision, p.DeliveryEpoch, "", next.Generation, relaunch); err != nil {
		t.Fatal("next Claude launch refused the replacement", err)
	}
}

func TestUnreadableClaudeCredentialDoesNotBlockBinding(t *testing.T) {
	isolated(t)
	dir := centralauth.ClaudeStore().Dir
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	evidence := filepath.Join(dir, "credential.json")
	if err := os.WriteFile(evidence, []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Bind("w", "claude"); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(evidence); err != nil || string(b) != "invalid" {
		t.Fatal("binding erased the unreadable credential evidence")
	}
	if Mode("w", "claude") != "central" {
		t.Fatal("unreadable credential changed routing")
	}
}

func TestInsecureClaudeStoreFailsClosedWithoutRewrite(t *testing.T) {
	isolated(t)
	dir := centralauth.ClaudeStore().Dir
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "credential.json")
	if err := os.WriteFile(path, []byte("evidence"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if err := Bind("w", "claude"); !errors.Is(err, centralauth.ErrInsecureStore) {
		t.Fatalf("insecure store accepted: %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "evidence" {
		t.Fatal("insecure credential file changed")
	}
}
