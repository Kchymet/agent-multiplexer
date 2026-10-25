package centralauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// mockToken is a synthetic value shaped like a setup-token; it is never a
// real credential.
const mockToken = "mock-oauth-token-0123456789abcdef"

func testStore(t *testing.T) Store {
	t.Helper()
	return Store{Dir: filepath.Join(t.TempDir(), "central", "claude"), Harness: Claude, Kind: KindClaudeOAuthToken}
}

func TestSecretNeverFormatsItsValue(t *testing.T) {
	s, err := NewSecret(mockToken)
	if err != nil {
		t.Fatal(err)
	}
	c := Credential{Harness: Claude, Kind: KindClaudeOAuthToken, Token: s, Generation: 3}
	j, _ := json.Marshal(c)
	txt, _ := s.MarshalText()
	for _, out := range []string{
		fmt.Sprint(s), fmt.Sprintf("%v %+v %#v %s %q %x", s, s, s, s, s, s),
		fmt.Sprintf("%v %+v %#v", c, c, c), string(j), string(txt), s.String(), s.GoString(),
	} {
		if strings.Contains(out, mockToken) || strings.Contains(out, mockToken[5:20]) {
			t.Fatalf("secret leaked through formatting: %s", out)
		}
		if !strings.Contains(out, s.Fingerprint()) {
			t.Fatalf("redaction lacks fingerprint: %s", out)
		}
	}
	if s.Reveal() != mockToken {
		t.Fatal("Reveal must return the value")
	}
}

func TestNewSecretValidation(t *testing.T) {
	for _, bad := range []string{"", "has space", "tab\tin", "new\nline", "nul\x00", "é-accent", strings.Repeat("a", maxSecretBytes+1)} {
		if _, err := NewSecret(bad); !errors.Is(err, ErrInvalidSecret) {
			t.Errorf("NewSecret(%q) = %v", bad, err)
		}
	}
	if _, err := NewSecret(strings.Repeat("a", maxSecretBytes)); err != nil {
		t.Fatal(err)
	}
}

func TestStoreSaveLoadAndGenerations(t *testing.T) {
	s := testStore(t)
	if _, err := s.Load(); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("empty Load = %v", err)
	}
	if st, err := s.Status(); err != nil || st.Configured {
		t.Fatalf("empty Status = %+v, %v", st, err)
	}
	first, _ := NewSecret(mockToken)
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	c, err := s.Save(first, now)
	if err != nil {
		t.Fatal(err)
	}
	if c.Generation != 1 || !c.StoredAt.Equal(now) {
		t.Fatalf("first save = %+v", c)
	}
	second, _ := NewSecret(mockToken + "-rotated")
	if c, err = s.Save(second, now.Add(time.Hour)); err != nil || c.Generation != 2 {
		t.Fatalf("second save = %+v, %v", c, err)
	}
	got, err := s.Load()
	if err != nil || got.Token.Reveal() != second.Reveal() || got.Generation != 2 {
		t.Fatalf("Load = %+v, %v", got, err)
	}
	st, err := s.Status()
	if err != nil || !st.Configured || st.Fingerprint != second.Fingerprint() || st.Generation != 2 {
		t.Fatalf("Status = %+v, %v", st, err)
	}
	for _, p := range []string{s.Dir, s.path()} {
		fi, err := os.Stat(p)
		if err != nil || fi.Mode().Perm()&0o077 != 0 {
			t.Fatalf("%s mode = %v, %v", p, fi.Mode(), err)
		}
	}
	entries, _ := os.ReadDir(s.Dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "."+credentialFile) {
			t.Fatalf("temporary file left behind: %s", e.Name())
		}
	}
}

func TestStoreRefusesExposedOrRedirectedRecords(t *testing.T) {
	s := testStore(t)
	tok, _ := NewSecret(mockToken)
	if _, err := s.Save(tok, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(s.path(), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(); !errors.Is(err, ErrInsecureStore) {
		t.Fatalf("world-readable record Load = %v", err)
	}
	if _, err := s.Status(); !errors.Is(err, ErrInsecureStore) {
		t.Fatalf("world-readable record Status = %v", err)
	}
	if err := os.Chmod(s.path(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(s.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(); !errors.Is(err, ErrInsecureStore) {
		t.Fatalf("exposed directory Load = %v", err)
	}
	if err := os.Chmod(s.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A symlinked record could redirect the daemon to another file.
	elsewhere := filepath.Join(t.TempDir(), "other.json")
	if err := os.Rename(s.path(), elsewhere); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, s.path()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(); !errors.Is(err, ErrInsecureStore) {
		t.Fatalf("symlinked record Load = %v", err)
	}
}

func TestStoreRejectsCorruptRecordsWithoutEchoingThem(t *testing.T) {
	s := testStore(t)
	if err := s.ensureDir(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.path(), []byte(`{"token":"`+mockToken+`",`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := s.Load()
	if err == nil || strings.Contains(err.Error(), mockToken) {
		t.Fatalf("corrupt Load = %v", err)
	}
	// A corrupt record never blocks a new login, and generations still advance.
	tok, _ := NewSecret(mockToken)
	c, err := s.Save(tok, time.Unix(1_800_000_000, 0))
	if err != nil || c.Generation <= 1 {
		t.Fatalf("Save over corrupt record = %+v, %v", c, err)
	}
	other := Store{Dir: s.Dir, Harness: Codex, Kind: "other"}
	if _, err := other.Load(); err == nil {
		t.Fatal("record of another harness/kind accepted")
	}
}

func TestConcurrentSavesNeverTearOrRepeatGenerations(t *testing.T) {
	s := testStore(t)
	const writers = 16
	var wg sync.WaitGroup
	gens := make(chan uint64, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tok, _ := NewSecret(fmt.Sprintf("%s-%02d", mockToken, i))
			c, err := s.Save(tok, time.Now())
			if err != nil {
				t.Error(err)
				return
			}
			gens <- c.Generation
		}(i)
	}
	stop := make(chan struct{})
	readErr := make(chan error, 1)
	go func() {
		for {
			select {
			case <-stop:
				readErr <- nil
				return
			default:
			}
			if c, err := s.Load(); err == nil && !strings.HasPrefix(c.Token.Reveal(), mockToken+"-") {
				readErr <- fmt.Errorf("torn read: %v", c)
				return
			} else if err != nil && !errors.Is(err, ErrNotConfigured) {
				readErr <- err
				return
			}
		}
	}()
	wg.Wait()
	close(stop)
	if err := <-readErr; err != nil {
		t.Fatal(err)
	}
	close(gens)
	seen := map[uint64]bool{}
	for g := range gens {
		if seen[g] {
			t.Fatalf("generation %d issued twice", g)
		}
		seen[g] = true
	}
	if c, _ := s.Load(); c.Generation != writers {
		t.Fatalf("final generation = %d", c.Generation)
	}
}

func TestLoginLockIsExclusiveAndReleased(t *testing.T) {
	s := testStore(t)
	release, err := s.LockLogin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.LockLogin(); err == nil || !strings.Contains(err.Error(), "another amux claude login") {
		t.Fatalf("second LockLogin = %v", err)
	}
	release()
	again, err := s.LockLogin()
	if err != nil {
		t.Fatalf("lock not released: %v", err)
	}
	again()
}
