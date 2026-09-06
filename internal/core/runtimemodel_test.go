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
