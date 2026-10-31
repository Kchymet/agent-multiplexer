package ghpr

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestSlug(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
		ok   bool
	}{
		{"kchymet/agent-multiplexer", "kchymet/agent-multiplexer", true},
		{"https://github.com/kchymet/agent-multiplexer.git", "kchymet/agent-multiplexer", true},
		{"https://github.com/kchymet/agent-multiplexer", "kchymet/agent-multiplexer", true},
		{"git@github.com:kchymet/agent-multiplexer.git", "kchymet/agent-multiplexer", true},
		{"ssh://git@github.com/kchymet/agent-multiplexer.git", "kchymet/agent-multiplexer", true},
		{"https://ghe.example.com/org/repo.git", "ghe.example.com/org/repo", true},
		{"/srv/git/repo.git", "", false},
		{"file:///srv/git/repo.git", "", false},
		{"~/src/repo", "", false},
		{"", "", false},
		{"https://github.com/only-owner", "", false},
	} {
		got, ok := Slug(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("Slug(%q) = %q,%v want %q,%v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestMatchesBranch(t *testing.T) {
	const b = "amux/wg1-abc123"
	for head, want := range map[string]bool{
		b:                        true,
		b + "-b":                 true,
		b + "/followup":          true,
		"amux/wg1-abc1234":       false,
		"amux/wg1-abc12":         false,
		"amux/wg1":               false,
		"main":                   false,
		"other/" + b:             false,
		"amux/wg1-abc123x-extra": false,
	} {
		if got := MatchesBranch(head, b); got != want {
			t.Errorf("MatchesBranch(%q) = %v want %v", head, got, want)
		}
	}
	if MatchesBranch("amux/x", "") {
		t.Error("empty branch matched")
	}
}

// fakeGH puts a gh script on PATH that prints out (or fails) and records its args.
func fakeGH(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	args := filepath.Join(dir, "args")
	body := "#!/bin/sh\necho \"$@\" > " + args + "\n" + script + "\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return args
}

func TestGHListsAndFilters(t *testing.T) {
	argsFile := fakeGH(t, `cat <<'JSON'
[{"number":1,"url":"https://github.com/o/r/pull/1","headRefName":"amux/wg-m","isDraft":false},
 {"number":2,"url":"https://github.com/o/r/pull/2","headRefName":"amux/wg-m-b","isDraft":true},
 {"number":3,"url":"https://github.com/o/r/pull/3","headRefName":"amux/wg-n","isDraft":false}]
JSON`)
	prs, err := GH{}.OpenPRs(context.Background(), "o/r", "amux/wg-m")
	if err != nil {
		t.Fatal(err)
	}
	want := []PR{
		{Number: 1, URL: "https://github.com/o/r/pull/1", HeadRefName: "amux/wg-m"},
		{Number: 2, URL: "https://github.com/o/r/pull/2", HeadRefName: "amux/wg-m-b", IsDraft: true},
	}
	if !reflect.DeepEqual(prs, want) {
		t.Fatalf("prs = %+v", prs)
	}
	args, _ := os.ReadFile(argsFile)
	if got := string(args); got != "pr list -R o/r --state open --limit 200 --json number,url,headRefName,isDraft\n" {
		t.Fatalf("gh args = %q", got)
	}
}

func TestGHErrors(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		if _, err := (GH{}).OpenPRs(context.Background(), "o/r", "amux/x"); err == nil {
			t.Fatal("missing gh did not error")
		}
	})
	t.Run("unauthenticated", func(t *testing.T) {
		fakeGH(t, "echo 'gh auth login' >&2; exit 4")
		if _, err := (GH{}).OpenPRs(context.Background(), "o/r", "amux/x"); err == nil {
			t.Fatal("failing gh did not error")
		}
	})
	t.Run("timeout", func(t *testing.T) {
		fakeGH(t, "exec /bin/sleep 5")
		start := time.Now()
		if _, err := (GH{Timeout: 50 * time.Millisecond}).OpenPRs(context.Background(), "o/r", "amux/x"); err == nil {
			t.Fatal("hung gh did not error")
		}
		if time.Since(start) > 3*time.Second {
			t.Fatal("timeout not enforced")
		}
	})
}
