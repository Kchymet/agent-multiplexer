package provider

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"amux/internal/core"
	"amux/internal/store"
	"amux/internal/wsops"
	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// TestBrowserEmptyWorkgroupContract runs the exact lifecycle shape emitted by
// Harness's browser through the provider adapter and the real temporary store.
// It is deliberately not an ApplyAction spy: the regression was a valid-looking
// wire sequence whose persisted empty grant ceiling rejected every later member.
func TestBrowserEmptyWorkgroupContract(t *testing.T) {
	for _, tc := range []struct {
		name, runtime, model string
		// stored is the persisted model when it differs from the one sent: a
		// legacy short Claude alias is stored as its detailed ID.
		stored string
		grants []string
	}{
		{name: "empty codex", runtime: "codex", model: "gpt-5.6-sol"},
		{name: "one claude", runtime: "claude", model: "claude-opus-5-5", grants: []string{"api"}},
		{name: "legacy claude alias", runtime: "claude", model: "opus", stored: "claude-opus-5-5", grants: []string{"api"}},
		{name: "multiple codex", runtime: "codex", model: "gpt-6-astra", grants: []string{"api", "web"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantModel := tc.model
			if tc.stored != "" {
				wantModel = tc.stored
			}
			browserCreationStore(t, "api", "web", "secret")
			prompted := map[string]int{}
			p := &Provider{cfg: Config{ApplyAction: func(ctx context.Context, action core.Action) (string, error) {
				if action.Action == core.ActionSteer {
					prompted[action.ID]++
					return "", nil
				}
				return wsops.ApplyResult(ctx, action)
			}}}
			wire := &session{
				rtCtx: context.Background(), published: map[string]core.Session{},
				provisional: map[string]struct{}{}, actions: map[uint64]*sessionActionCall{},
			}
			fields := map[string]string{
				"name": "browser-created", "repos": strings.Join(tc.grants, ","),
				"prompt": "", "coordinator": tc.runtime, "coordinator_model": tc.model,
				"agent": tc.runtime, "model": tc.model,
				"identity_mode": harnessproto.IdentityMachine,
			}
			rootID, err := p.applyAuthorizedSessionAction(wire, harnessproto.MuxMsg{
				Action: harnessproto.VerbNewWorkgroupEmpty, Fields: fields,
			})
			if err != nil {
				t.Fatal(err)
			}

			db, err := store.Open()
			if err != nil {
				t.Fatal(err)
			}
			root, ok, err := db.GetSession(rootID)
			if err != nil || !ok {
				t.Fatalf("created root: %+v ok=%v err=%v", root, ok, err)
			}
			children, err := db.Children(rootID)
			if err != nil || len(children) != 0 {
				t.Fatalf("new-workgroup-empty precreated members: %+v err=%v", children, err)
			}
			grants, initialized, err := db.CoordinatorRepoGrants(rootID)
			if err != nil || !initialized || !reflect.DeepEqual(grants, tc.grants) {
				t.Fatalf("grants = %v initialized=%v err=%v, want %v/true", grants, initialized, err, tc.grants)
			}
			if root.Agent != tc.runtime || root.Model != wantModel || root.Prompt != "" {
				t.Fatalf("coordinator selection drifted: %+v", root)
			}
			db.Close()

			var memberIDs []string
			for _, repo := range tc.grants {
				memberID, err := p.applyAuthorizedSessionAction(wire, harnessproto.MuxMsg{
					Action: harnessproto.VerbAddAgent, ID: rootID,
					Fields: map[string]string{"repos": repo, "prompt": "", "agent": tc.runtime, "model": tc.model, "identity_mode": harnessproto.IdentityMachine},
				})
				if err != nil {
					t.Fatalf("add %s: %v", repo, err)
				}
				memberIDs = append(memberIDs, memberID)
				if _, err := p.applyAuthorizedSessionAction(wire, harnessproto.MuxMsg{
					Action: harnessproto.VerbPrompt, ID: memberID,
					Fields: map[string]string{harnessproto.FieldText: "browser task"},
				}); err != nil {
					t.Fatalf("prompt %s: %v", memberID, err)
				}
			}
			for _, memberID := range memberIDs {
				if prompted[memberID] != 1 {
					t.Fatalf("member %s prompted %d times, want exactly once", memberID, prompted[memberID])
				}
			}

			db, err = store.Open()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			children, err = db.Children(rootID)
			if err != nil || len(children) != len(tc.grants) {
				t.Fatalf("members = %+v err=%v, want %d", children, err, len(tc.grants))
			}
			for i, child := range children {
				if child.Agent != tc.runtime || child.Model != wantModel || child.Prompt != "" || child.Repo != tc.grants[i] {
					t.Fatalf("member %d selection drifted: %+v", i, child)
				}
			}
			if len(tc.grants) == 1 {
				if _, err := p.applyAuthorizedSessionAction(wire, harnessproto.MuxMsg{
					Action: harnessproto.VerbAddAgent, ID: rootID,
					Fields: map[string]string{"repos": "secret", "agent": tc.runtime, "model": tc.model},
				}); err == nil || !strings.Contains(err.Error(), "grant") {
					t.Fatalf("out-of-grant add = %v, want grant refusal", err)
				}
				after, _ := db.Children(rootID)
				if len(after) != 1 {
					t.Fatalf("denied add changed members: %+v", after)
				}
			}
			// A successful authoritative snapshot that omits provisional IDs
			// revokes them; they are not an enduring bypass of inventory policy.
			wire.revokeMissingPublished(map[string]core.Session{})
			for _, memberID := range memberIDs {
				if _, err := p.applyAuthorizedSessionAction(wire, harnessproto.MuxMsg{
					Action: harnessproto.VerbPrompt, ID: memberID,
					Fields: map[string]string{harnessproto.FieldText: "must be denied"},
				}); err != errSessionNotPublished {
					t.Fatalf("revoked provisional member %s = %v", memberID, err)
				}
			}
		})
	}
}

func TestBrowserEmptyWorkgroupFailsClosed(t *testing.T) {
	browserCreationStore(t, "api")
	called := false
	p := &Provider{cfg: Config{ApplyAction: func(ctx context.Context, action core.Action) (string, error) {
		called = true
		return wsops.ApplyResult(ctx, action)
	}}}
	for _, fields := range []map[string]string{
		{},
		{"repos": "", "defaultAgent": "1"},
	} {
		called = false
		if _, err := p.applySessionAction(context.Background(), harnessproto.MuxMsg{Action: harnessproto.VerbNewWorkgroupEmpty, Fields: fields}); err == nil {
			t.Fatalf("accepted malformed fields %#v", fields)
		}
		if called {
			t.Fatalf("malformed fields reached store: %#v", fields)
		}
	}
	if _, err := p.applySessionAction(context.Background(), harnessproto.MuxMsg{
		Action: harnessproto.VerbNewWorkgroupEmpty, Fields: map[string]string{"repos": "missing"},
	}); err == nil || !strings.Contains(err.Error(), `unknown repo "missing"`) {
		t.Fatalf("unknown repo = %v", err)
	}
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	roots, err := db.Roots()
	if err != nil || len(roots) != 0 {
		t.Fatalf("failed creates persisted roots: %+v err=%v", roots, err)
	}
}

func browserCreationStore(t *testing.T, repos ...string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("AMUX_JAIL", "off")
	t.Setenv("AMUX_GIT_TRUST_LOCAL_SOURCE", "1")
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, repo := range repos {
		gitDir := browserBareRepo(t)
		if err := db.PutRepo(store.Repo{Name: repo, Source: gitDir, GitDir: gitDir}); err != nil {
			t.Fatal(err)
		}
	}
}

func browserBareRepo(t *testing.T) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "src")
	gitDir := filepath.Join(t.TempDir(), "repo.git")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	browserGit(t, src, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(src, "README"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	browserGit(t, src, "add", "README")
	browserGit(t, src, "commit", "-q", "-m", "fixture")
	browserGit(t, "", "clone", "-q", "--bare", src, gitDir)
	return gitDir
}

func browserGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.invalid",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
