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
	for _, v := range []int{0, 3, 99} {
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
