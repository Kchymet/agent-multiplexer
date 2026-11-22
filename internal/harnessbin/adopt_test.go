package harnessbin

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestAdoptDecisionRules(t *testing.T) {
	m, _ := newTestManager(t)
	used := AdoptInput{OnPath: "/usr/local/bin/claude"}
	cases := []struct {
		name string
		in   AdoptInput
		want bool
		why  string
	}{
		{"binary on PATH", used, true, "found"},
		{"logged in only", AdoptInput{LoggedIn: "~/.claude/.credentials.json"}, true, "logged in"},
		{"not used", AdoptInput{}, false, "not installed or logged in"},
		{"env override", AdoptInput{OnPath: "/x/claude", EnvBin: "/opt/claude"}, false, "AMUX_CLAUDE_BIN"},
		{"config override", AdoptInput{OnPath: "/x/claude", ConfigBin: "/opt/claude"}, false, "harnesses.claude.bin"},
	}
	for _, c := range cases {
		got, why := m.AdoptDecision("claude", c.in)
		if got != c.want || !strings.Contains(why, c.why) {
			t.Errorf("%s: AdoptDecision = %v %q; want %v containing %q", c.name, got, why, c.want, c.why)
		}
	}
	if ok, _ := m.AdoptDecision("hermes", used); ok {
		t.Fatal("adopted an unsupported harness")
	}

	// An existing managed install (pinned or not) is never re-adopted.
	mustInstall(t, m, "1.0.0")
	if ok, why := m.AdoptDecision("claude", used); ok || !strings.Contains(why, "already managed") {
		t.Fatalf("managed kind: %v %q", ok, why)
	}

	// Uninstall is remembered: adoption never reinstalls what the user removed.
	if _, _, err := m.Uninstall("claude"); err != nil {
		t.Fatal(err)
	}
	if ok, why := m.AdoptDecision("claude", used); ok || !strings.Contains(why, "uninstalled") {
		t.Fatalf("declined kind: %v %q", ok, why)
	}
	// An explicit install opts back in.
	mustInstall(t, m, "1.0.0")
	if st := m.LoadState("claude"); st.Declined {
		t.Fatal("explicit install left the kind declined")
	}
}

func TestAdoptInstallsOnlyUsedKindsAndNeverDowngrades(t *testing.T) {
	m, claude := newTestManager(t)
	claude.latest = "2.0.0"
	codex := m.Sources["codex"].(*fakeSource)
	ctx := context.Background()
	inputs := map[string]AdoptInput{
		// The user's own claude is newer than the stable channel.
		"claude": {OnPath: "/home/u/.local/bin/claude", PathVersion: "2.1.0"},
		"codex":  {}, // not installed, not logged in
	}
	res := m.Adopt(ctx, Settings{}, func(k string) AdoptInput { return inputs[k] })
	if len(res) != 2 || !res[0].Adopted || res[0].Version != "2.1.0" || res[1].Adopted {
		t.Fatalf("Adopt = %+v", res)
	}
	if m.Current("claude") != "2.1.0" || m.Current("codex") != "" || len(codex.fetched) != 0 {
		t.Fatalf("current claude %q codex %q, codex fetched %v", m.Current("claude"), m.Current("codex"), codex.fetched)
	}
	// Running again is a no-op: the kind is managed now.
	res = m.Adopt(ctx, Settings{}, func(k string) AdoptInput { return inputs[k] })
	if res[0].Adopted || !reflect.DeepEqual(claude.fetched, []string{"2.1.0"}) {
		t.Fatalf("second Adopt = %+v fetched %v", res, claude.fetched)
	}
}

func TestAdoptFailureIsRateLimited(t *testing.T) {
	m, src := newTestManager(t)
	now := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)
	m.Now = func() time.Time { return now }
	src.fail = errors.New("offline")
	in := func(k string) AdoptInput {
		if k == "claude" {
			return AdoptInput{LoggedIn: "creds"}
		}
		return AdoptInput{}
	}
	res := m.Adopt(context.Background(), Settings{}, in)
	if res[0].Err == nil || res[0].Adopted {
		t.Fatalf("Adopt = %+v", res)
	}
	if ok, why := m.AdoptDecision("claude", in("claude")); ok || !strings.Contains(why, "offline") {
		t.Fatalf("retry not rate-limited: %v %q", ok, why)
	}
	now = now.Add(AdoptRetry)
	src.fail = nil
	if res := m.Adopt(context.Background(), Settings{}, in); !res[0].Adopted || m.Current("claude") != "1.0.0" {
		t.Fatalf("retry after %v = %+v", AdoptRetry, res)
	}
	if st := m.LoadState("claude"); st.AdoptError != "" {
		t.Fatalf("success left error %q", st.AdoptError)
	}
}
