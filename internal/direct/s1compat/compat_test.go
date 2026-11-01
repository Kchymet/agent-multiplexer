package direct

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	cur "amux/internal/direct"
)

const (
	frozenCommit = "15fcf0e"
	frozenSHA256 = "8ec57093fc37613739120a3817338a36642d621599a1e308b43a8f40a7f8e0ee"
	verifier     = "dBjftJeZ4CVP-mJ0kWbaCPaFfvAZ8tZ_xwvoGbZT4eNC-JqHT8JN2WtW"
)

func TestFrozenReaderIsVerbatim(t *testing.T) {
	got, err := os.ReadFile("store.go")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(got)
	if hex.EncodeToString(sum[:]) != frozenSHA256 {
		t.Fatal("s1compat/store.go is no longer the frozen 15fcf0e reader")
	}
	// Cross-check against history when the checkout has it (CI may be shallow).
	out, err := exec.Command("git", "show", frozenCommit+":internal/direct/store.go").Output()
	if err != nil {
		t.Logf("git history unavailable (%v); relying on the pinned hash", err)
		return
	}
	if !bytes.Equal(out, got) {
		t.Fatal("s1compat/store.go differs from 15fcf0e internal/direct/store.go")
	}
}

func s1Load(t *testing.T, root string) (*storeState, error) {
	t.Helper()
	st, err := openStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer st.close()
	return st.loadState()
}

func cloudStore(t *testing.T) (root, code string) {
	t.Helper()
	root = t.TempDir()
	a, err := cur.OpenAuthority(root, cur.Lifetimes{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	wg := cur.ResourceScope{Kind: cur.ResourceWorkgroups, Workgroups: []string{"wg1"}}
	if _, err := a.EnableCloud(cur.CloudPolicyRequest{Permissions: []string{cur.PermRead}, Resource: wg, OwnerPin: "u1", ProviderPin: "p1"}); err != nil {
		t.Fatal(err)
	}
	code, _, err = a.IssueCloudCode(cur.CloudIssueRequest{IssueKey: "k1", Requester: "u1", ProviderID: "p1",
		CodeChallenge: cur.S256Challenge(verifier), CodeChallengeMethod: cur.CodeChallengeMethodS256,
		Permissions: []string{cur.PermRead}, Resource: wg})
	if err != nil {
		t.Fatal(err)
	}
	return root, code
}

// The 15fcf0e reader must refuse a registry holding an outstanding cloud code:
// it would otherwise redeem the code with no verifier and no policy check.
func TestS1ReaderRefusesV2WithOutstandingCloudCode(t *testing.T) {
	root, _ := cloudStore(t)
	before, _ := os.ReadFile(filepath.Join(root, grantsFile))
	_, err := s1Load(t, root)
	if err == nil || !strings.Contains(err.Error(), "unsupported version 2") {
		t.Fatalf("15fcf0e reader on a v2 registry: %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(root, grantsFile))
	if !bytes.Equal(before, after) {
		t.Fatal("refused read changed the registry")
	}
}

// Why the schema version moved: had cloud fields been added under v1, the
// 15fcf0e reader would load an outstanding cloud code as an ordinary,
// unbound enrollment code. The current reader refuses such a file.
func TestV1WithCloudFieldsWouldDowngradeToUnboundCode(t *testing.T) {
	root, code := cloudStore(t)
	path := filepath.Join(root, grantsFile)
	raw, _ := os.ReadFile(path)
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc["v"] = 1
	v1, _ := json.Marshal(doc)
	if err := os.WriteFile(path, v1, 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := s1Load(t, root)
	if err != nil {
		t.Fatalf("15fcf0e reader: %v", err)
	}
	sum := sha256.Sum256([]byte(code))
	rec := st.Codes[hex.EncodeToString(sum[:])]
	if rec == nil || rec.ConsumedAt != 0 || time.Now().Unix() >= rec.ExpiresAt {
		t.Fatalf("expected the 15fcf0e reader to see a live code: %+v", rec)
	}
	// It has no notion of a challenge: this is the hazard v2 prevents.
	if _, err := cur.OpenAuthority(root, cur.Lifetimes{}, nil); err == nil || !strings.Contains(err.Error(), "version 2 cloud data") {
		t.Fatalf("current reader accepted v1 with cloud data: %v", err)
	}
}

// A real S1 registry (accepted by the 15fcf0e reader) migrates to v2 keeping
// every grant, revocation, used-refresh history and idempotency record.
func TestV1MigrationPreservesState(t *testing.T) {
	root := t.TempDir()
	a, err := cur.OpenAuthority(root, cur.Lifetimes{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	all := cur.ResourceScope{Kind: cur.ResourceAllSessions}
	redeem := func() cur.TokenResponse {
		code, _, err := a.NewCode("x", []string{cur.PermRead, cur.PermSteer}, all, 0)
		if err != nil {
			t.Fatal(err)
		}
		tok, err := a.RedeemCode(code, "phone", cur.ClientAndroid)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	live := redeem()
	old := live.RefreshToken
	if live, err = a.Refresh(old); err != nil {
		t.Fatal(err)
	}
	revoked := redeem()
	if err := a.Revoke(revoked.ClientID, "test"); err != nil {
		t.Fatal(err)
	}
	l, err := a.Admit(context.Background(), live.AccessToken, cur.PermSteer)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.BeginAction(l, "idem-1", "body-hash", false); err != nil {
		t.Fatal(err)
	}
	a.FinishAction(l, "idem-1", true, "", "")
	l.Done()
	pending, _, err := a.NewCode("pending", []string{cur.PermRead}, all, 0)
	if err != nil {
		t.Fatal(err)
	}
	a.Close()

	// Rewrite as schema v1, exactly what 15fcf0e wrote (no cloud data exists).
	path := filepath.Join(root, grantsFile)
	raw, _ := os.ReadFile(path)
	var doc map[string]any
	_ = json.Unmarshal(raw, &doc)
	if _, ok := doc["cloud"]; ok {
		t.Fatal("manual-only registry carries cloud data")
	}
	delete(doc, "cloudIssues")
	doc["v"] = 1
	v1, _ := json.Marshal(doc)
	if err := os.WriteFile(path, v1, 0o600); err != nil {
		t.Fatal(err)
	}
	s1Before, err := s1Load(t, root)
	if err != nil {
		t.Fatalf("not a valid S1 registry: %v", err)
	}

	b, err := cur.OpenAuthority(root, cur.Lifetimes{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	migrated, _ := os.ReadFile(path)
	var head struct{ V int }
	_ = json.Unmarshal(migrated, &head)
	if head.V != 2 {
		t.Fatalf("migrated version %d", head.V)
	}
	// Same records, read field by field with the frozen types.
	var s1After storeState
	if err := json.Unmarshal(migrated, &s1After); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s1Before.Grants, s1After.Grants) || !reflect.DeepEqual(s1Before.Codes, s1After.Codes) ||
		s1Before.IdentityGeneration != s1After.IdentityGeneration {
		t.Fatal("migration changed S1 records")
	}
	// And they still behave.
	if _, err := b.RedeemCode(pending, "c", cur.ClientWeb); err != nil {
		t.Fatalf("pending manual code lost: %v", err)
	}
	if _, err := b.Refresh(revoked.RefreshToken); err == nil {
		t.Fatal("revocation lost in migration")
	}
	if _, err := b.Refresh(old); err == nil {
		t.Fatal("refresh reuse detection lost in migration")
	}
	if _, err := b.Refresh(live.RefreshToken); err == nil {
		t.Fatal("reuse did not revoke the whole grant after migration")
	}
	b.Close()
	if _, err := s1Load(t, root); err == nil || !strings.Contains(err.Error(), "unsupported version 2") {
		t.Fatalf("15fcf0e reader on the migrated registry: %v", err)
	}
}
