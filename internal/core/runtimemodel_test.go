package core

import "testing"

func TestRuntimeModelRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := WriteRuntimeModel("", "gpt-5.6"); err != nil {
		t.Fatalf("blank identity: %v", err)
	}
	if err := WriteRuntimeModel("session/one", "  gpt-5.6  "); err != nil {
		t.Fatal(err)
	}
	got, ok := RuntimeModel("session/one")
	if !ok || got.Model != "gpt-5.6" || got.Updated == 0 {
		t.Fatalf("RuntimeModel = %+v, %v; want gpt-5.6 with timestamp", got, ok)
	}
}

func TestSessionRuntimeModelScopesEqualRuntimeIDsBySubject(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := WriteSessionRuntimeModel("a", "same", "model-a"); err != nil {
		t.Fatal(err)
	}
	if err := WriteSessionRuntimeModel("b", "same", "model-b"); err != nil {
		t.Fatal(err)
	}
	a, okA := SessionRuntimeModel("a", "same")
	b, okB := SessionRuntimeModel("b", "same")
	if !okA || !okB || a.Model != "model-a" || b.Model != "model-b" {
		t.Fatalf("scoped models a=%+v/%v b=%+v/%v", a, okA, b, okB)
	}
	if _, ok := RuntimeModel("same"); ok {
		t.Fatal("managed model leaked into legacy UUID-only state")
	}
}
