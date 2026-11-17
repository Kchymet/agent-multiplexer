package harnessproto

import (
	"encoding/json"
	"flag"
	"os"
	"testing"
)

var updateTurnKeys = flag.Bool("update-turnkeys", false, "rewrite testdata/dte1-turnkey.json")

type turnKeyCase struct {
	Name        string `json:"name"`
	SessionID   string `json:"session_id"`
	Incarnation string `json:"incarnation"`
	Kind        string `json:"kind"`
	Generation  string `json:"generation"`
	Index       int64  `json:"index"`
	Key         string `json:"key"`
}

type turnKeyFixtures struct {
	Doc         string        `json:"doc"`
	SourceKinds []string      `json:"source_kinds"`
	Cases       []turnKeyCase `json:"cases"`
	// SameKey and DifferentKey name case pairs whose keys must (not) match.
	SameKey      [][2]string `json:"same_key"`
	DifferentKey [][2]string `json:"different_key"`
}

func turnKeyCases() []turnKeyCase {
	base := turnKeyCase{SessionID: "agent-7", Incarnation: "inc-3f9a", Kind: "codex", Generation: "Q2F0Zm9vZGJhcg.bGluZTFkaWdl", Index: 12}
	c := func(name string, f func(*turnKeyCase)) turnKeyCase { x := base; x.Name = name; f(&x); return x }
	return []turnKeyCase{
		c("base", func(*turnKeyCase) {}),
		// restore-before-resume copy: new file identity, same first line
		c("restore_copy", func(x *turnKeyCase) { x.Generation = "TmV3RmlsZUlkZW50.bGluZTFkaWdl" }),
		c("next_turn_end", func(x *turnKeyCase) { x.Index = 13 }),
		c("reused_id", func(x *turnKeyCase) { x.Incarnation = "inc-77c1" }),
		c("new_transcript", func(x *turnKeyCase) { x.Generation = "Q2F0Zm9vZGJhcg.b3RoZXJmaXJz" }),
		c("other_source", func(x *turnKeyCase) { x.Kind = "journal" }),
		c("structured_source", func(x *turnKeyCase) { x.Kind = "structured" }),
		c("claude_source", func(x *turnKeyCase) { x.Kind = "claude" }),
		// unknown inputs: no key
		c("no_first_line", func(x *turnKeyCase) { x.Generation = "Q2F0Zm9vZGJhcg." }),
		c("unknown_generation", func(x *turnKeyCase) { x.Generation = "" }),
		c("unlabeled_batch", func(x *turnKeyCase) { x.Incarnation = "" }),
		c("no_index", func(x *turnKeyCase) { x.Index = 0 }),
	}
}

func TestTurnKeyFixtures(t *testing.T) {
	want := turnKeyFixtures{
		Doc:          "dte1 shared turn-end occurrence key fixtures (amux docs/direct-wake-contract.md). key = \"dte1_\" + base64url(first 16 bytes of SHA-256(join(\"\\x00\", \"amux-turn-end-v1\", session_id, incarnation, kind, line_digest, decimal(index))))); line_digest = generation after its last '.'; empty key when any input is unknown.",
		SourceKinds:  TurnKeySourceKinds,
		SameKey:      [][2]string{{"base", "restore_copy"}},
		DifferentKey: [][2]string{{"base", "next_turn_end"}, {"base", "reused_id"}, {"base", "new_transcript"}, {"base", "other_source"}},
	}
	for _, c := range turnKeyCases() {
		c.Key = DirectTurnKey(c.SessionID, c.Incarnation, c.Kind, TurnLineDigest(c.Generation), c.Index)
		want.Cases = append(want.Cases, c)
	}
	path := "testdata/dte1-turnkey.json"
	if *updateTurnKeys {
		b, _ := json.MarshalIndent(want, "", "  ")
		if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got turnKeyFixtures
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	keys := map[string]string{}
	for i, c := range got.Cases {
		if k := DirectTurnKey(c.SessionID, c.Incarnation, c.Kind, TurnLineDigest(c.Generation), c.Index); k != c.Key || k != want.Cases[i].Key {
			t.Fatalf("%s: key %q, fixture %q", c.Name, k, c.Key)
		}
		if c.Key != "" && !ValidTurnKey(c.Key) {
			t.Fatalf("%s: invalid key shape %q", c.Name, c.Key)
		}
		keys[c.Name] = c.Key
	}
	for _, name := range []string{"no_first_line", "unknown_generation", "unlabeled_batch", "no_index"} {
		if keys[name] != "" {
			t.Fatalf("%s produced a key", name)
		}
	}
	for _, p := range got.SameKey {
		if keys[p[0]] == "" || keys[p[0]] != keys[p[1]] {
			t.Fatalf("%s and %s must share a key", p[0], p[1])
		}
	}
	for _, p := range got.DifferentKey {
		if keys[p[0]] == keys[p[1]] {
			t.Fatalf("%s and %s must differ", p[0], p[1])
		}
	}
	if len(got.SourceKinds) != len(TurnKeySourceKinds) {
		t.Fatalf("source kinds changed: %v", got.SourceKinds)
	}
}
