package amuxcfg

import (
	"encoding/json"
	"testing"
)

// goal.auto_resume defaults on, round-trips both values, unsets back to the
// default, and rejects a non-boolean without touching sibling keys.
func TestGoalAutoResume(t *testing.T) {
	isolate(t)
	on, err := ResolveGoalAutoResume()
	if err != nil || !on {
		t.Fatalf("default: on=%v err=%v", on, err)
	}
	if _, set, _ := GoalAutoResume(); set {
		t.Fatal("unconfigured switch reported as saved")
	}
	off := false
	if err := SetGoalAutoResume(&off); err != nil {
		t.Fatal(err)
	}
	if on, err := ResolveGoalAutoResume(); err != nil || on {
		t.Fatalf("after off: on=%v err=%v", on, err)
	}
	value, set, err := GoalAutoResume()
	if err != nil || !set || value {
		t.Fatalf("saved off: value=%v set=%v err=%v", value, set, err)
	}
	yes := true
	if err := SetGoalAutoResume(&yes); err != nil {
		t.Fatal(err)
	}
	if on, err := ResolveGoalAutoResume(); err != nil || !on {
		t.Fatalf("after on: on=%v err=%v", on, err)
	}
	if err := SetGoalAutoResume(nil); err != nil {
		t.Fatal(err)
	}
	top, err := Read()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := top["goal"]; ok {
		t.Fatal("unset left an empty goal section")
	}

	top["goal"], _ = json.Marshal(map[string]any{"auto_resume": "sometimes", "other": 1})
	if err := Write(top); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveGoalAutoResume(); err == nil {
		t.Fatal("non-boolean switch accepted")
	}
	if err := SetGoalAutoResume(&off); err != nil {
		t.Fatal(err)
	}
	top, _ = Read()
	var goal map[string]json.RawMessage
	if err := json.Unmarshal(top["goal"], &goal); err != nil || string(goal["other"]) != "1" || string(goal["auto_resume"]) != "false" {
		t.Fatalf("repair lost sibling keys: %s", top["goal"])
	}

	for in, want := range map[string]bool{"on": true, "TRUE": true, "1": true, "off": false, "false": false, " No ": false} {
		got, err := ParseGoalAutoResume(in)
		if err != nil || got != want {
			t.Errorf("parse %q: got=%v err=%v", in, got, err)
		}
	}
	if _, err := ParseGoalAutoResume("maybe"); err == nil {
		t.Error("parse accepted maybe")
	}
}
