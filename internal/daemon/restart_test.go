package daemon

import (
	"amux/internal/access"
	"amux/internal/core"
	"amux/internal/engine"
	"amux/internal/panespec"
	"context"
	"fmt"
	"testing"
)

func TestRestartOnlyNamedAgentPreservesConversationAndOtherPanes(t *testing.T) {
	for _, kind := range []string{"claude", "codex"} {
		t.Run(kind, func(t *testing.T) {
			d, e := steerDaemon(t)
			putSession(t, "a1", kind)
			old := e.running("a1")
			other := e.running("child")
			for _, tab := range []int{panespec.TabEditor, panespec.TabTerminal} {
				key := engine.Key{AgentID: "a1", Tab: tab}
				e.insts[key] = &fakeInstance{key: key}
			}
			generation, err := d.permissions.observe("a1", old)
			if err != nil {
				t.Fatal(err)
			}
			d.resolve = func(spec panespec.LaunchSpec, tab int) (string, []string, []string, error) {
				if spec.Session.ClaudeID != convID("a1") || tab != panespec.TabAgent {
					t.Fatal("restart changed conversation or tab")
				}
				return "", nil, nil, fmt.Errorf("invalid config")
			}
			if r := d.handle(context.Background(), core.Action{Action: core.ActionRestart, ID: "a1"}); r.OK {
				t.Fatal("invalid launch succeeded")
			}
			if !old.Alive() {
				t.Fatal("failed preflight stopped agent")
			}
			d.resolve = func(spec panespec.LaunchSpec, tab int) (string, []string, []string, error) {
				if spec.Session.ClaudeID != convID("a1") {
					t.Fatal("conversation changed")
				}
				return "", nil, []string{kind, "--resume", spec.Session.ClaudeID}, nil
			}
			r := d.handle(context.Background(), core.Action{Action: core.ActionRestart, ID: "a1"})
			if !r.OK || r.RestartedID != "a1" {
				t.Fatalf("restart: %+v", r)
			}
			current, ok := e.Lookup(old.Key())
			if !ok || current == old || old.Alive() {
				t.Fatal("agent was not replaced")
			}
			if !other.Alive() {
				t.Fatal("restart stopped another agent")
			}
			for _, tab := range []int{panespec.TabEditor, panespec.TabTerminal} {
				in, ok := e.Lookup(engine.Key{AgentID: "a1", Tab: tab})
				if !ok || !in.Alive() {
					t.Fatal("restart stopped another tab")
				}
			}
			next, ok := d.permissions.generation("a1")
			if !ok || next == generation {
				t.Fatal("restart reused permission generation")
			}
		})
	}
}

func TestRestartRejectsSessionRPCAndExtraArguments(t *testing.T) {
	if _, err := canonicalSessionOperation(access.Request{Route: access.RouteAction, Verb: core.ActionRestart, ID: "a1"}); err == nil {
		t.Fatal("restricted client can restart agents")
	}
	d, _ := steerDaemon(t)
	for _, a := range []core.Action{{Action: core.ActionRestart}, {Action: core.ActionRestart, ID: "a1", Target: "other"}, {Action: core.ActionRestart, ID: "a1", Fields: map[string]string{"force": "true"}}} {
		if r := d.handle(context.Background(), a); r.OK {
			t.Fatalf("accepted %+v", a)
		}
	}
}
