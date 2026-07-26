package wsops

import (
	"context"
	"strings"
	"testing"

	"amux/internal/core"
	"amux/internal/store"
)

// TestControlActionsAreDispatchable is the drift guard behind the "valid
// actions" an unknown-verb error teaches: every verb core.ControlActions()
// advertises must actually be dispatchable, or the error message sends callers
// after something the daemon then rejects. A verb may fail for its *own* reason
// (no such id, no such repo) — it may not fail as unknown.
//
// ActionStart is the one advertised verb wsops doesn't own: it starts a process
// in the daemon's engine and changes no store state, so daemon.handle intercepts
// it before ApplyResult ever sees it.
func TestControlActionsAreDispatchable(t *testing.T) {
	isolateStore(t)
	for _, verb := range core.ControlActions() {
		if verb == core.ActionStart {
			continue // engine-only; handled in daemon.handle, never reaches here
		}
		t.Run(verb, func(t *testing.T) {
			_, err := ApplyResult(context.Background(), core.Action{Action: verb, ID: "nope"})
			if err != nil && strings.Contains(err.Error(), "unknown action") {
				t.Errorf("ApplyResult(%q) reports the verb as unknown, but ControlActions() advertises it: %v", verb, err)
			}
		})
	}
}

// TestUnknownActionListsValidOnes pins the teaching half: a typo'd verb answers
// with the vocabulary rather than just refusing.
func TestUnknownActionListsValidOnes(t *testing.T) {
	isolateStore(t)
	_, err := ApplyResult(context.Background(), core.Action{Action: "archve"})
	if err == nil {
		t.Fatal("ApplyResult(bogus verb) = nil error, want an error")
	}
	for _, want := range []string{`"archve"`, core.ActionArchive, core.ActionNewWorkgroup} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestRemoveRepoUnknownListsTracked covers `amux repo rm <bad>`: the error names
// the repos you could have meant, so a typo doesn't cost a second command.
func TestRemoveRepoUnknownListsTracked(t *testing.T) {
	isolateStore(t)
	db, err := store.Open()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	for _, name := range []string{"api", "web"} {
		if err := db.PutRepo(store.Repo{Name: name, Source: "acme/" + name}); err != nil {
			t.Fatalf("put repo %s: %v", name, err)
		}
	}
	db.Close()

	err = RemoveRepo("wbe")
	if err == nil {
		t.Fatal("RemoveRepo(unknown) = nil error, want an error")
	}
	if !strings.Contains(err.Error(), "tracked repos: api, web") {
		t.Errorf("error %q does not list the tracked repos", err)
	}
}

// TestRemoveRepoUnknownWithNoneTracked covers the empty store: listing nothing
// would be useless, so the error says how to track the first repo instead.
func TestRemoveRepoUnknownWithNoneTracked(t *testing.T) {
	isolateStore(t)
	err := RemoveRepo("api")
	if err == nil {
		t.Fatal("RemoveRepo(unknown) = nil error, want an error")
	}
	if !strings.Contains(err.Error(), "amux repo add") {
		t.Errorf("error %q does not point at `amux repo add`", err)
	}
}
