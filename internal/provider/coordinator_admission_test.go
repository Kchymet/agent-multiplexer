package provider

import (
	"context"
	"strings"
	"testing"

	"amux/internal/core"
	"amux/internal/wsops"
	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// A creation verb must be admitted against the harnesses the daemon will
// actually launch. A workgroup always launches a coordinator — the default goal
// runtime unless the request names another — and only launches a member when it
// asks for repositories. Validating the member field alone (the old behaviour)
// both refused a valid default request on a Codex-only pool, because an absent
// `agent` reads as Claude, and admitted one on a Claude-only pool whose Codex
// coordinator launch would then fail on the machine.
func TestCreationAdmissionChecksTheHarnessesItWillLaunch(t *testing.T) {
	pool := func(kinds ...string) *harnessproto.ExecutionCapabilities {
		caps := &harnessproto.ExecutionCapabilities{IdentityModes: []string{harnessproto.IdentityMachine}}
		for _, k := range kinds {
			caps.Harnesses = append(caps.Harnesses, harnessproto.HarnessCapability{Name: k})
		}
		return caps
	}
	const codex, claude = "codex", "claude"
	for _, tc := range []struct {
		name   string
		caps   *harnessproto.ExecutionCapabilities
		action string
		fields map[string]string
		wantOK bool
		reject string // the harness the error must name when refused
	}{
		{
			name: "default coordinator on a codex-only pool", caps: pool(codex),
			action: harnessproto.VerbNewWorkgroup, fields: map[string]string{"prompt": "ship it"},
			wantOK: true,
		},
		{
			name: "default coordinator on a claude-only pool", caps: pool(claude),
			action: harnessproto.VerbNewWorkgroup, fields: map[string]string{"prompt": "ship it"},
			wantOK: false, reject: codex,
		},
		{
			name: "explicit claude coordinator on a claude-only pool", caps: pool(claude),
			action: harnessproto.VerbNewWorkgroup,
			fields: map[string]string{wsops.FieldCoordinator: claude, "prompt": "supervise"},
			wantOK: true,
		},
		{
			name: "explicit claude coordinator on a codex-only pool", caps: pool(codex),
			action: harnessproto.VerbNewWorkgroup,
			fields: map[string]string{wsops.FieldCoordinator: claude},
			wantOK: false, reject: claude,
		},
		{
			// The member harness matters only when repositories request one.
			name: "claude member requested on a codex-only pool", caps: pool(codex),
			action: harnessproto.VerbNewWorkgroup,
			fields: map[string]string{"repos": "api", "agent": claude},
			wantOK: false, reject: claude,
		},
		{
			name: "claude member named but no repos", caps: pool(codex),
			action: harnessproto.VerbNewWorkgroup, fields: map[string]string{"agent": claude},
			wantOK: true,
		},
		{
			name: "both harnesses available", caps: pool(codex, claude),
			action: harnessproto.VerbNewWorkgroup,
			fields: map[string]string{"repos": "api", "agent": claude},
			wantOK: true,
		},
		{
			// add-agent creates exactly one member and no coordinator.
			name: "add-agent default member on a claude-only pool", caps: pool(claude),
			action: harnessproto.VerbAddAgent, fields: map[string]string{},
			wantOK: true,
		},
		{
			name: "add-agent codex member on a claude-only pool", caps: pool(claude),
			action: harnessproto.VerbAddAgent, fields: map[string]string{"agent": codex},
			wantOK: false, reject: codex,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var applied core.Action
			p := &Provider{cfg: Config{
				Execution: tc.caps,
				ApplyAction: func(_ context.Context, a core.Action) (string, error) {
					applied = a
					return "new", nil
				},
			}}
			id, err := p.applySessionAction(context.Background(),
				harnessproto.MuxMsg{Action: tc.action, ID: "wg", Fields: tc.fields})
			if tc.wantOK {
				if err != nil {
					t.Fatalf("admission refused a launchable request: %v", err)
				}
				if id != "new" || applied.Action == "" {
					t.Fatalf("verb did not reach the daemon: id=%q action=%+v", id, applied)
				}
				return
			}
			if err == nil {
				t.Fatalf("admission accepted a request this pool cannot launch (applied %+v)", applied)
			}
			if !strings.Contains(err.Error(), tc.reject) {
				t.Fatalf("error %q does not name the unsupported harness %q", err, tc.reject)
			}
			if applied.Action != "" {
				t.Fatalf("refused verb still reached the daemon: %+v", applied)
			}
		})
	}
}

// A pool that advertises no execution block at all is a legacy provider: it is
// not evidence that any harness is available, so nothing is inferred from it.
func TestCreationAdmissionWithoutCapabilitiesIsUnchanged(t *testing.T) {
	var applied core.Action
	p := &Provider{cfg: Config{ApplyAction: func(_ context.Context, a core.Action) (string, error) {
		applied = a
		return "new", nil
	}}}
	if _, err := p.applySessionAction(context.Background(),
		harnessproto.MuxMsg{Action: harnessproto.VerbNewWorkgroup, Fields: map[string]string{"prompt": "x"}}); err != nil {
		t.Fatalf("legacy pool refused a creation: %v", err)
	}
	if applied.Action != core.ActionNewWorkgroup {
		t.Fatalf("verb did not reach the daemon: %+v", applied)
	}
}

// An identity mode this daemon cannot serve is refused whatever the harnesses.
func TestCreationAdmissionRejectsForeignIdentity(t *testing.T) {
	p := &Provider{cfg: Config{ApplyAction: func(context.Context, core.Action) (string, error) {
		t.Fatal("an unsupported identity reached the daemon")
		return "", nil
	}}}
	_, err := p.applySessionAction(context.Background(), harnessproto.MuxMsg{
		Action: harnessproto.VerbNewWorkgroup,
		Fields: map[string]string{"identity_mode": harnessproto.IdentityAPIKey},
	})
	if err == nil || !strings.Contains(err.Error(), harnessproto.IdentityAPIKey) {
		t.Fatalf("identity refusal = %v", err)
	}
}
