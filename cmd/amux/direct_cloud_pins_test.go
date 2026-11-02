package main

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
	"time"

	"amux/internal/direct"
	"amux/internal/provider"
)

func registeredStatus() provider.Status {
	return provider.Status{
		State: provider.StateRegistered, Orchestrator: "harness.example:7443",
		ProviderID: "prov-7", OwnerUserID: "usr_1", OwnerEmail: "o@example.com",
		RegisteredAt: time.Now().Add(-time.Minute),
	}
}

// The simple path: no ids on the command line, both pins come from the
// registration Harness acknowledged, and the operator sees which account,
// provider and Harness they are.
func TestResolveCloudPinsFromRegistration(t *testing.T) {
	pins, err := resolveCloudPins("", "", registeredStatus(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if pins.Owner != "usr_1" || pins.Provider != "prov-7" || !pins.FromReg {
		t.Fatalf("pins = %+v", pins)
	}
	d := pins.describe()
	for _, want := range []string{"harness.example:7443", "usr_1 (o@example.com)", "prov-7"} {
		if !strings.Contains(d, want) {
			t.Errorf("describe() = %q, missing %q", d, want)
		}
	}
	// A matching single flag is just a confirmation.
	if pins, err := resolveCloudPins("usr_1", "", registeredStatus(), nil); err != nil || pins.Provider != "prov-7" {
		t.Fatalf("matching --owner: %+v, %v", pins, err)
	}
	if pins, err := resolveCloudPins("", "prov-7", registeredStatus(), nil); err != nil || pins.Owner != "usr_1" {
		t.Fatalf("matching --provider: %+v, %v", pins, err)
	}
}

// Both flags are the advanced override: used verbatim, even with no
// registration at all, with a visible note when they disagree with it.
func TestResolveCloudPinsExplicitOverride(t *testing.T) {
	pins, err := resolveCloudPins("usr_x", "prov-x", provider.Status{}, fs.ErrNotExist)
	if err != nil || pins.Owner != "usr_x" || pins.Provider != "prov-x" || pins.FromReg || pins.Note != "" {
		t.Fatalf("explicit without registration: %+v, %v", pins, err)
	}
	pins, err = resolveCloudPins("usr_x", "prov-7", registeredStatus(), nil)
	if err != nil || pins.Owner != "usr_x" || !strings.Contains(pins.Note, "differ") {
		t.Fatalf("explicit disagreeing pins: %+v, %v", pins, err)
	}
	if pins, _ := resolveCloudPins("usr_1", "prov-7", registeredStatus(), nil); pins.Note != "" {
		t.Fatalf("agreeing explicit pins carry a note: %q", pins.Note)
	}
}

// Absent, unusable or contradicted registrations fail with a reason and the
// explicit escape hatch; nothing is guessed and no other id is substituted.
func TestResolveCloudPinsRefusals(t *testing.T) {
	rejected := registeredStatus()
	rejected.State, rejected.LastError = provider.StateRejected, "revoked"
	never := provider.Status{State: provider.StateDialing}
	unowned := registeredStatus()
	unowned.OwnerUserID, unowned.OwnerEmail = "", ""

	cases := []struct {
		name        string
		owner, prov string
		st          provider.Status
		stErr       error
		want        string
	}{
		{"no status file", "", "", provider.Status{}, fs.ErrNotExist, "no Harness provider registration"},
		{"unreadable status", "", "", provider.Status{}, errors.New("bad json"), "bad json"},
		{"rejected", "", "", rejected, nil, "rejected (revoked)"},
		{"never registered", "", "", never, nil, "has not completed a registration"},
		{"no owner reported", "", "", unowned, nil, "reported no owner for provider prov-7"},
		{"owner only, no owner reported", "usr_1", "", unowned, nil, "reported no owner"},
		{"other provider", "", "prov-9", registeredStatus(), nil, "not this host's registered provider prov-7"},
		{"other owner", "usr_9", "", registeredStatus(), nil, "not the registered owner usr_1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pins, err := resolveCloudPins(c.owner, c.prov, c.st, c.stErr)
			if err == nil {
				t.Fatalf("resolved %+v, want an error", pins)
			}
			if !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "--owner") {
				t.Fatalf("error = %q, want %q and the explicit-pin advice", err, c.want)
			}
		})
	}
}

func TestCloudPinDrift(t *testing.T) {
	on := direct.CloudStatus{Enabled: true, OwnerPin: "usr_1", ProviderPin: "prov-7"}
	if n := cloudPinDrift(on, registeredStatus(), nil); n != "" {
		t.Fatalf("matching pins drift: %q", n)
	}
	moved := registeredStatus()
	moved.OwnerUserID = "usr_2"
	if n := cloudPinDrift(on, moved, nil); !strings.Contains(n, "usr_2") {
		t.Fatalf("reassigned owner not reported: %q", n)
	}
	if n := cloudPinDrift(direct.CloudStatus{}, moved, nil); n != "" {
		t.Fatalf("disabled policy reported drift: %q", n)
	}
	if n := cloudPinDrift(on, provider.Status{}, fs.ErrNotExist); n != "" {
		t.Fatalf("no registration reported drift: %q", n)
	}
}
