package core

import (
	"os"
	"testing"
)

// TestPermissionJournalLifecycle walks one prompt through the journal the way the
// hooks do — opened by PermissionRequest, closed by the hook that proves it went
// away — and pins the property the `permission` verb depends on: a resolved
// request is no longer pending, so its id can never be answered twice.
func TestPermissionJournalLifecycle(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if got := PendingPermissions("s1"); len(got) != 0 {
		t.Fatalf("a session with no journal has nothing pending, got %+v", got)
	}
	// A record with no id, or no session, writes nothing rather than a junk line.
	if err := AppendPermission("s1", PermissionRecord{Tool: "Bash"}); err != nil {
		t.Fatalf("id-less record: %v", err)
	}
	if err := AppendPermission("", PermissionRecord{RequestID: "perm-1"}); err != nil {
		t.Fatalf("session-less record: %v", err)
	}
	if got := PendingPermissions("s1"); len(got) != 0 {
		t.Fatalf("nothing should have been journaled, got %+v", got)
	}

	req := PermissionRecord{
		RequestID: "perm-1", Tool: "Bash", Action: "rm -rf /tmp/x",
		Options: []string{PermissionAllow, PermissionDeny},
	}
	if err := AppendPermission("s1", req); err != nil {
		t.Fatal(err)
	}
	open := PendingPermissions("s1")
	if len(open) != 1 || open[0].RequestID != "perm-1" || open[0].Action != "rm -rf /tmp/x" {
		t.Fatalf("pending = %+v, want the open perm-1", open)
	}
	if open[0].At == 0 {
		t.Error("AppendPermission must stamp a time so a stale journal is legible")
	}

	rec, ok := ResolvePermission("s1", "Bash", PermissionAllow)
	if !ok || rec.RequestID != "perm-1" || rec.Decision != PermissionAllow {
		t.Fatalf("resolve = %+v ok=%v, want perm-1 allowed", rec, ok)
	}
	if got := PendingPermissions("s1"); len(got) != 0 {
		t.Fatalf("a resolved request must not stay pending, got %+v", got)
	}
	// The resolving hooks fire on every tool call, prompted or not: with nothing
	// open, resolving is a no-op rather than an error or an invented record.
	if _, ok := ResolvePermission("s1", "Bash", PermissionAllow); ok {
		t.Error("resolving with nothing open should report false")
	}
	if n := len(ReadPermissions("s1")); n != 2 {
		t.Errorf("journal has %d lines, want the request and its one resolution", n)
	}
}

// TestPermissionJournalManyOpen covers parallel tool calls: several prompts can
// be open at once, a resolution names the one for its tool, and a turn boundary
// clears whatever is left.
func TestPermissionJournalManyOpen(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	for _, r := range []PermissionRecord{
		{RequestID: "perm-a", Tool: "Bash"},
		{RequestID: "perm-b", Tool: "Write"},
		{RequestID: "perm-c", Tool: "WebFetch"},
	} {
		if err := AppendPermission("s1", r); err != nil {
			t.Fatal(err)
		}
	}
	if got := PendingPermissions("s1"); len(got) != 3 {
		t.Fatalf("pending = %+v, want three open requests", got)
	}

	// The resolving hook names its tool, not the request, so the matching one is
	// picked rather than simply the oldest.
	rec, ok := ResolvePermission("s1", "Write", PermissionDeny)
	if !ok || rec.RequestID != "perm-b" {
		t.Fatalf("resolve(Write) = %+v ok=%v, want perm-b", rec, ok)
	}
	// An unknown tool falls back to the oldest still open.
	rec, ok = ResolvePermission("s1", "NoSuchTool", PermissionAllow)
	if !ok || rec.RequestID != "perm-a" {
		t.Fatalf("resolve(unknown tool) = %+v ok=%v, want the oldest, perm-a", rec, ok)
	}

	if n := ClearPermissions("s1"); n != 1 {
		t.Fatalf("clear resolved %d requests, want the 1 still open", n)
	}
	if got := PendingPermissions("s1"); len(got) != 0 {
		t.Fatalf("clear must leave nothing open, got %+v", got)
	}
	last := ReadPermissions("s1")
	if l := last[len(last)-1]; l.RequestID != "perm-c" || l.Decision != PermissionCleared {
		t.Errorf("last line = %+v, want perm-c cleared", l)
	}
}

// TestPermissionJournalTolerance: a torn or foreign line must not hide the
// records around it — the journal is appended to by hook processes that can be
// killed mid-write.
func TestPermissionJournalTolerance(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if err := AppendPermission("s1", PermissionRecord{RequestID: "perm-1", Tool: "Bash"}); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(PermissionJournalPath("s1"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{\"request_id\": tru\n\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := AppendPermission("s1", PermissionRecord{RequestID: "perm-2", Tool: "Write"}); err != nil {
		t.Fatal(err)
	}

	open := PendingPermissions("s1")
	if len(open) != 2 || open[0].RequestID != "perm-1" || open[1].RequestID != "perm-2" {
		t.Fatalf("pending = %+v, want both requests despite the torn line between them", open)
	}
}

// TestNewPermissionID: ids must be distinct, or two prompts in one session would
// share one answer.
func TestNewPermissionID(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := NewPermissionID()
		if id == "" || seen[id] {
			t.Fatalf("NewPermissionID returned %q (empty or repeated)", id)
		}
		seen[id] = true
	}
}
