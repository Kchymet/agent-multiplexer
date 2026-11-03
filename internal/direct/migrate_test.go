package direct

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// v1Store builds a manual-only registry and rewrites it as schema v1.
func v1Store(t *testing.T) (root string, refresh string) {
	t.Helper()
	root = t.TempDir()
	a, err := OpenAuthority(root, Lifetimes{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tok := enroll(t, a, []string{PermRead}, allSessions)
	a.Close()
	path := filepath.Join(root, grantsFile)
	raw, _ := os.ReadFile(path)
	var doc map[string]any
	_ = json.Unmarshal(raw, &doc)
	delete(doc, "cloudIssues")
	doc["v"] = 1
	v1, _ := json.Marshal(doc)
	if err := os.WriteFile(path, v1, 0o600); err != nil {
		t.Fatal(err)
	}
	return root, tok.RefreshToken
}

func TestMigrationFailureLeavesV1Intact(t *testing.T) {
	for _, ambiguous := range []bool{false, true} {
		root, refresh := v1Store(t)
		path := filepath.Join(root, grantsFile)
		before, _ := os.ReadFile(path)
		st, err := openStore(root)
		if err != nil {
			t.Fatal(err)
		}
		st.write = func(dir, name string, data []byte) error {
			if ambiguous {
				return errAmbiguousCommit
			}
			return errNotCommitted
		}
		if _, err := loadAuthority(st, Lifetimes{}.mustResolve(t), time.Now); err == nil || !strings.Contains(err.Error(), "migrate") {
			t.Fatalf("failed migration opened: %v", err)
		}
		st.close()
		after, _ := os.ReadFile(path)
		if !bytes.Equal(before, after) {
			t.Fatal("failed migration rewrote the registry")
		}
		a := openTestAuthority(t, root, Lifetimes{}, nil)
		if _, err := a.Refresh(refresh); err != nil {
			t.Fatalf("grant lost after a failed migration and retry: %v", err)
		}
	}
}

func TestUnknownStateVersionRefused(t *testing.T) {
	for _, v := range []int{0, 4, 99} {
		root, _ := v1Store(t)
		path := filepath.Join(root, grantsFile)
		raw, _ := os.ReadFile(path)
		raw = bytes.Replace(raw, []byte(`"v":1`), []byte(fmt.Sprintf(`"v":%d`, v)), 1)
		_ = os.WriteFile(path, raw, 0o600)
		if _, err := OpenAuthority(root, Lifetimes{}, nil); err == nil {
			t.Fatalf("version %d opened", v)
		}
		if got, _ := os.ReadFile(path); !bytes.Equal(got, raw) {
			t.Fatalf("version %d rewritten", v)
		}
	}
}

func (l Lifetimes) mustResolve(t *testing.T) Lifetimes {
	t.Helper()
	r, err := l.resolve()
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// v2Store builds a current registry (with a pending code, a grant and a
// completed action) and rewrites it as schema v2, which has no tombstones.
func v2Store(t *testing.T) (root, refresh string) {
	t.Helper()
	root = t.TempDir()
	a, err := OpenAuthority(root, Lifetimes{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tok := enroll(t, a, []string{PermRead, PermSteer}, allSessions)
	l, err := a.Admit(t.Context(), tok.AccessToken, PermSteer)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.BeginAction(l, "k", "h", false); err != nil {
		t.Fatal(err)
	}
	a.FinishResult(l, "k", ActionResponse{OK: true})
	l.Done()
	a.Close()
	path := filepath.Join(root, grantsFile)
	raw, _ := os.ReadFile(path)
	raw = bytes.Replace(raw, []byte(`"v":3`), []byte(`"v":2`), 1)
	if bytes.Contains(raw, []byte(`"spent"`)) {
		t.Fatal("fresh store already has tombstones")
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return root, tok.RefreshToken
}

func TestV2MigratesToV3KeepingRecords(t *testing.T) {
	root, refresh := v2Store(t)
	path := filepath.Join(root, grantsFile)
	before, _ := os.ReadFile(path)
	a := openTestAuthority(t, root, Lifetimes{}, nil)
	after, _ := os.ReadFile(path)
	if !bytes.Contains(after, []byte(`"v":3`)) {
		t.Fatalf("not migrated: %s", after)
	}
	// Only the version changed.
	if !bytes.Equal(bytes.Replace(before, []byte(`"v":2`), []byte(`"v":3`), 1), after) {
		t.Fatal("migration changed records")
	}
	if _, err := a.Refresh(refresh); err != nil {
		t.Fatal(err)
	}
}

// Injected write failures during the v2 -> v3 migration. "before rename"
// fails without touching the file (as writeAtomic does when the temp write
// fails), so the v2 file stays byte-identical. "after rename" really renames
// the complete v3 file into place and then reports errAmbiguousCommit (as
// writeAtomic does when the directory sync fails): the open still fails, and
// the next open loads that complete v3 file with every grant. Neither leaves
// a partial or reset registry.
func TestV2MigrationWriteFailures(t *testing.T) {
	for _, afterRename := range []bool{false, true} {
		t.Run(fmt.Sprint("afterRename=", afterRename), func(t *testing.T) {
			root, refresh := v2Store(t)
			path := filepath.Join(root, grantsFile)
			before, _ := os.ReadFile(path)
			st, err := openStore(root)
			if err != nil {
				t.Fatal(err)
			}
			st.write = func(dir, name string, data []byte) error {
				if !afterRename {
					return errNotCommitted
				}
				if err := writeTemp(dir, name, data); err != nil {
					return err
				}
				return errAmbiguousCommit
			}
			if _, err := loadAuthority(st, Lifetimes{}.mustResolve(t), time.Now); err == nil || !strings.Contains(err.Error(), "migrate version 2 to 3") {
				t.Fatalf("failed migration opened: %v", err)
			}
			st.close()
			after, _ := os.ReadFile(path)
			want := before
			if afterRename {
				want = bytes.Replace(before, []byte(`"v":2`), []byte(`"v":3`), 1)
			}
			if !bytes.Equal(after, want) {
				t.Fatalf("registry after the failed write:\n%s\nwant:\n%s", after, want)
			}
			a := openTestAuthority(t, root, Lifetimes{}, nil)
			if _, err := a.Refresh(refresh); err != nil {
				t.Fatalf("grant lost after a failed migration and reopen: %v", err)
			}
			if got, _ := os.ReadFile(path); !bytes.Contains(got, []byte(`"v":3`)) {
				t.Fatal("not at version 3 after reopen")
			}
		})
	}
}

// A file claiming an older version while carrying tombstones is refused
// unchanged, never loaded and rewritten without them.
func TestOldVersionWithSpentKeysRefused(t *testing.T) {
	for _, v := range []int{1, 2} {
		root, _ := v2Store(t)
		path := filepath.Join(root, grantsFile)
		raw, _ := os.ReadFile(path)
		var doc map[string]any
		_ = json.Unmarshal(raw, &doc)
		doc["v"] = v
		delete(doc, "cloudIssues")
		for _, g := range doc["grants"].(map[string]any) {
			g.(map[string]any)["spent"] = map[string]string{"0011223344556677": "8899aabbccddeeff"}
		}
		raw, _ = json.Marshal(doc)
		_ = os.WriteFile(path, raw, 0o600)
		if _, err := OpenAuthority(root, Lifetimes{}, nil); err == nil || !strings.Contains(err.Error(), "spent keys") {
			t.Fatalf("version %d with tombstones: %v", v, err)
		}
		if got, _ := os.ReadFile(path); !bytes.Equal(got, raw) {
			t.Fatalf("version %d rewritten", v)
		}
	}
}
