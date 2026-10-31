package source

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"amux/internal/core"
	"amux/internal/store"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// A member row carries its PRs (the `prs` field of `amux status --json`) and a
// short PR suffix; a goal coordinator's status names its goal status.
func TestPollSurfacesPRsAndGoalStatus(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "data"))
	t.Setenv("HOME", t.TempDir())
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []store.Session{
		{ID: "wg1", Agent: "claude", Mode: store.ModeTask, Scope: store.ScopeWork, Created: 1},
		{ID: "m1", RootID: "wg1", Agent: "claude", Mode: store.ModeTask, Branch: "amux/wg1-m1", Created: 2},
	} {
		if err := db.PutSession(s); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	w := NewWorkspace()
	pr := harnessproto.SessionPR{Repo: "o/r", Number: 247, URL: "https://github.com/o/r/pull/247", Branch: "amux/wg1-m1",
		State: "open", Checks: "fail", FailedChecks: []string{"build"}, Mergeable: "mergeable"}
	w.SetPRs(func(id string) []harnessproto.SessionPR {
		if id == "m1" {
			return []harnessproto.SessionPR{pr}
		}
		return nil
	})
	w.SetGoalStatus(func(s store.Session) string { return "waiting" })
	rows, err := w.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]core.Session{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	m := byID["m1"]
	if m.PRs == nil || len(*m.PRs) != 1 || (*m.PRs)[0].Number != 247 {
		t.Fatalf("member PRs = %+v", m.PRs)
	}
	if !strings.Contains(m.Status, "PR #247 CI✗") {
		t.Errorf("member status %q lacks PR suffix", m.Status)
	}
	if root := byID["wg1"]; !strings.Contains(root.Status, "goal waiting") {
		t.Errorf("coordinator status %q lacks goal status", root.Status)
	}
	if byID["wg1"].PRs != nil {
		t.Errorf("coordinator row carries PRs: %+v", byID["wg1"].PRs)
	}

	b, err := json.Marshal(core.Snapshot{Type: "snapshot", Sessions: []core.Session{m}})
	if err != nil {
		t.Fatal(err)
	}
	var shape struct {
		Sessions []struct {
			PRs []map[string]any `json:"prs"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(b, &shape); err != nil {
		t.Fatal(err)
	}
	got := shape.Sessions[0].PRs
	if len(got) != 1 {
		t.Fatalf("status JSON prs = %s", b)
	}
	for _, key := range []string{"repo", "number", "url", "branch", "state", "checks", "failedChecks", "mergeable"} {
		if _, ok := got[0][key]; !ok {
			t.Errorf("status JSON pr lacks %q: %v", key, got[0])
		}
	}
}
