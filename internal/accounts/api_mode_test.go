package accounts

import (
	"errors"
	"testing"
)

// A mode change or API-key replacement never retargets the running process,
// and the next launch takes it without any rebind.
func TestReplacementServesNextLaunchOnly(t *testing.T) {
	for _, harness := range []string{"claude", "codex"} {
		for _, change := range []string{"subscription-to-api", "api-to-subscription", "api-replacement"} {
			t.Run(harness+"/"+change, func(t *testing.T) {
				isolated(t)
				before, after := "api", "api"
				if change == "subscription-to-api" {
					before = "subscription"
				}
				if change == "api-to-subscription" {
					after = "subscription"
				}
				id := ID(harness, "central")
				if err := Update(func(s *State) error {
					p := s.Profiles[id]
					p.Mode, p.State = before, "ready"
					s.Profiles[id] = p
					s.CredentialVersion[id] = 41
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				first, err := BeginLaunch("existing", harness)
				if err != nil {
					t.Fatal(err)
				}
				b, p, err := Delivery("existing", harness)
				if err != nil {
					t.Fatal(err)
				}
				if err := Accept("existing", b.Revision, p.DeliveryEpoch, "", 41, first); err != nil {
					t.Fatal(err)
				}
				if err := Update(func(s *State) error {
					p := s.Profiles[id]
					p.Mode = after
					p.DeliveryEpoch++
					p.Revision++
					s.Profiles[id] = p
					s.CredentialVersion[id] = 42
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				b, p, err = Delivery("existing", harness)
				if err != nil {
					t.Fatal(err)
				}
				if err := Accept("existing", b.Revision, p.DeliveryEpoch, "", 42, first); !errors.Is(err, ErrSuperseded) {
					t.Fatalf("running process retargeted: %v", err)
				}
				// Rediscovery cannot move the running process either.
				if err := BindExisting("existing", harness); err != nil {
					t.Fatal(err)
				}
				if err := Accept("existing", b.Revision, p.DeliveryEpoch, "", 42, first); !errors.Is(err, ErrSuperseded) {
					t.Fatalf("rediscovery removed fence: %v", err)
				}
				next, err := BeginLaunch("existing", harness)
				if err != nil {
					t.Fatal(err)
				}
				if err := Accept("existing", b.Revision, p.DeliveryEpoch, "", 42, next); err != nil {
					t.Fatal("next launch refused the replacement", err)
				}
				s, _ := Read()
				got := s.Bindings["existing"]
				if got.CredentialMode != after || got.CredentialVersion != 42 || got.Launch != next.Seq {
					t.Fatalf("next launch not pinned: %+v", got)
				}
				if len(got.History) != 1 || got.History[0].Launch != first.Seq || got.History[0].Mode != before || got.History[0].Version != 41 {
					t.Fatalf("previous runtime evidence lost: %+v", got.History)
				}
				if err := Accept("existing", b.Revision, p.DeliveryEpoch, "", 42, first); !errors.Is(err, ErrSuperseded) {
					t.Fatalf("superseded process accepted: %v", err)
				}
			})
		}
	}
}
