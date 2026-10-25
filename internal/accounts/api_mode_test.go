package accounts

import (
	"errors"
	"testing"
)

func TestOpaqueCredentialsPinModeAndGeneration(t *testing.T) {
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
				if err := Bind("existing", harness); err != nil {
					t.Fatal(err)
				}
				b, p, err := Delivery("existing", harness)
				if err != nil {
					t.Fatal(err)
				}
				// No native account ID: mode and generation must provide the fence.
				if err := Accept("existing", b.Revision, p.DeliveryEpoch, "", 41); err != nil {
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
				if err := Accept("existing", b.Revision, p.DeliveryEpoch, "", 42); !errors.Is(err, ErrBlocked) {
					t.Fatalf("replacement accepted: %v", err)
				}
				// Reading and rebinding discovery after restart cannot remove the fence.
				if err := Bind("existing", harness); err != nil {
					t.Fatal(err)
				}
				if err := Accept("existing", b.Revision, p.DeliveryEpoch, "", 42); !errors.Is(err, ErrBlocked) {
					t.Fatalf("rediscovery removed fence: %v", err)
				}
				if err := Bind("new", harness); err != nil {
					t.Fatal(err)
				}
				b, p, err = Delivery("new", harness)
				if err != nil {
					t.Fatal(err)
				}
				if err := Accept("new", b.Revision, p.DeliveryEpoch, "", 42); err != nil {
					t.Fatal("new workload refused", err)
				}
			})
		}
	}
}

func TestInitialAPIKeyAllowedButUnknownLegacyRefused(t *testing.T) {
	for _, harness := range []string{"claude", "codex"} {
		for _, unknown := range []bool{false, true} {
			t.Run(harness+map[bool]string{false: "/new", true: "/unknown-legacy"}[unknown], func(t *testing.T) {
				isolated(t)
				if err := Bind("waiting", harness); err != nil {
					t.Fatal(err)
				}
				if err := Update(func(s *State) error {
					b := s.Bindings["waiting"]
					b.CredentialUnknown = unknown
					s.Bindings["waiting"] = b
					p := s.Profiles[b.ProfileID]
					p.Mode, p.State = "api", "ready"
					p.DeliveryEpoch++
					s.Profiles[p.ID] = p
					s.CredentialVersion[p.ID] = 1
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if err := Bind("waiting", harness); err != nil {
					t.Fatal(err)
				}
				b, p, err := Delivery("waiting", harness)
				if err == nil {
					err = Accept("waiting", b.Revision, p.DeliveryEpoch, "", 1)
				}
				if unknown && err == nil {
					t.Fatal("unknown legacy binding adopted API key")
				}
				if !unknown && err != nil {
					t.Fatal("first authorized API login refused", err)
				}
			})
		}
	}
}
