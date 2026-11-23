package core

import "testing"

// TestBranchFor pins the single-source branch scheme:
// amux/<broker>/<root>-<agent>. The root and agent are joined by a hyphen, not
// a slash, so the branch can't collide with a legacy amux/<root> ref. Changing
// the scheme is a change to BranchFor alone.
func TestBranchFor(t *testing.T) {
	if got, want := BranchFor("r1", "a1"), "amux/"+TestBrokerID+"/r1-a1"; got != want {
		t.Fatalf("BranchFor(r1, a1) = %q, want %q", got, want)
	}
	defer SetBrokerIDForTest("Kevin's MacBook.local")()
	if got, want := BranchFor("3f9c61", "3581b6"), "amux/kevin-s-macbook-local/3f9c61-3581b6"; got != want {
		t.Fatalf("BranchFor with a sanitized broker = %q, want %q", got, want)
	}
	if got, want := FlatBranchFor("r1", "a1"), "amux/r1-a1"; got != want {
		t.Fatalf("FlatBranchFor(r1, a1) = %q, want %q", got, want)
	}
}

func TestParseBranch(t *testing.T) {
	for branch, want := range map[string]ParsedBranch{
		"amux/host1/3f9c61-3581b6":       {Broker: "host1", Root: "3f9c61", Agent: "3581b6"},
		"amux/host1/3f9c61-3581b6-b":     {Broker: "host1", Root: "3f9c61", Agent: "3581b6", Suffix: "-b"},
		"amux/host1/3f9c61-3581b6/wip":   {Broker: "host1", Root: "3f9c61", Agent: "3581b6", Suffix: "/wip"},
		"amux/b-abc123/3f9c61-3581b6":    {Broker: "b-abc123", Root: "3f9c61", Agent: "3581b6"},
		"amux/3f9c61-3581b6":             {Root: "3f9c61", Agent: "3581b6"},
		"amux/3f9c61-3581b6-cold-attach": {Root: "3f9c61", Agent: "3581b6", Suffix: "-cold-attach"},
		"amux/3f9c61-3581b6/wip":         {Root: "3f9c61", Agent: "3581b6", Suffix: "/wip"},
		"amux/3f9c61":                    {Root: "3f9c61"},
		"amux/3f9c61/x":                  {Root: "3f9c61", Suffix: "/x"},
	} {
		got, ok := ParseBranch(branch)
		if !ok || got != want {
			t.Errorf("ParseBranch(%q) = %+v, %v; want %+v, true", branch, got, ok, want)
		}
	}
	for _, branch := range []string{"", "amux/", "main", "feature/3f9c61-3581b6", "amux/host1/3f9c61", "amux/-x", "amux/3f9c61-"} {
		if got, ok := ParseBranch(branch); ok {
			t.Errorf("ParseBranch(%q) = %+v, want not ok", branch, got)
		}
	}
	// Round trip through BranchFor for every broker id shape.
	for _, broker := range []string{"host1", "abcdef", "deadbeef-mini", "x"} {
		restore := SetBrokerIDForTest(broker)
		b := BranchFor("3f9c61", "3581b6")
		p, ok := ParseBranch(b)
		restore()
		if !ok || p.Broker != SanitizeBrokerID(broker) || p.Root != "3f9c61" || p.Agent != "3581b6" || p.Suffix != "" {
			t.Errorf("ParseBranch(BranchFor) for broker %q: %q → %+v, %v", broker, b, p, ok)
		}
	}
}
