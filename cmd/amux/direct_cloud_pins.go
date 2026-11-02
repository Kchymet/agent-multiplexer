package main

import (
	"errors"
	"fmt"
	"io/fs"
	"time"

	"amux/internal/direct"
	"amux/internal/provider"
)

// cloudPins is the owner/provider pair `amux direct cloud enable` pins, and
// where it came from.
//
// Resolution source and trust boundary: an omitted pin is taken from this
// host's own provider status file, which `amux provide` writes from the
// orchestrator's `registered` frame — the reply to a registration
// authenticated with this host's provider token over TLS to the orchestrator
// the operator configured. The orchestrator fills the owner from its live
// provider row, so the defaults are exactly what Harness itself will send as
// the direct-enroll requester and the provider id it vouches for. Nothing is
// inferred from labels, the display email, a relayed request or an id typed
// elsewhere, and a supplied flag that disagrees with the registration is
// refused rather than silently reconciled. Supplying both flags skips the
// registration entirely (the advanced override).
type cloudPins struct {
	Owner      string
	Provider   string
	OwnerEmail string // display only
	Origin     string // the Harness provider endpoint the registration came from
	FromReg    bool   // at least one pin was resolved from the registration
	State      string
	Registered time.Time
	Note       string // explicit pins that differ from the registration
}

// errCloudPinsUsage is the advice every resolution failure ends with.
const cloudPinsOverride = "pass both --owner <harness-user-id> and --provider <provider-id> to pin explicitly"

// resolveCloudPins fills omitted pins from the provider registration. st/stErr
// are provider.ReadStatus's result, read by the caller (so tests can drive it).
func resolveCloudPins(owner, prov string, st provider.Status, stErr error) (cloudPins, error) {
	if owner != "" && prov != "" {
		pins := cloudPins{Owner: owner, Provider: prov}
		if stErr == nil && st.ProviderID != "" && (st.ProviderID != prov || (st.OwnerUserID != "" && st.OwnerUserID != owner)) {
			pins.Note = fmt.Sprintf("note: these explicit pins differ from this host's provider registration (owner %s, provider %s); issuance answers owner_mismatch/provider_mismatch unless Harness agrees with them",
				orNone(st.OwnerUserID), st.ProviderID)
		}
		return pins, nil
	}
	switch {
	case errors.Is(stErr, fs.ErrNotExist):
		return cloudPins{}, fmt.Errorf("no Harness provider registration on this host (%s does not exist): run `amux provide` against Harness until `amux doctor` shows it registered, or %s", provider.StatusPath(), cloudPinsOverride)
	case stErr != nil:
		return cloudPins{}, fmt.Errorf("read the provider registration %s: %w; or %s", provider.StatusPath(), stErr, cloudPinsOverride)
	case st.State == provider.StateRejected:
		return cloudPins{}, fmt.Errorf("the provider's last registration with %s was rejected (%s): fix the provider registration first, or %s", orNone(st.Orchestrator), orNone(st.LastError), cloudPinsOverride)
	case st.ProviderID == "":
		return cloudPins{}, fmt.Errorf("the provider has not completed a registration with Harness (state %s): wait until `amux doctor` shows it registered, or %s", orNone(st.State), cloudPinsOverride)
	case st.OwnerUserID == "":
		return cloudPins{}, fmt.Errorf("Harness reported no owner for provider %s: direct enrollment needs a provider registered to your account (not shared or unowned), and a Harness that reports registration owners. Restart `amux provide` after upgrading Harness, or %s", st.ProviderID, cloudPinsOverride)
	}
	if prov != "" && prov != st.ProviderID {
		return cloudPins{}, fmt.Errorf("--provider %s is not this host's registered provider %s; the owner cannot be derived for a different provider: %s", prov, st.ProviderID, cloudPinsOverride)
	}
	if owner != "" && owner != st.OwnerUserID {
		return cloudPins{}, fmt.Errorf("--owner %s is not the registered owner %s of provider %s: %s", owner, st.OwnerUserID, st.ProviderID, cloudPinsOverride)
	}
	return cloudPins{
		Owner: st.OwnerUserID, Provider: st.ProviderID, OwnerEmail: st.OwnerEmail, Origin: st.Orchestrator,
		FromReg: true, State: st.State, Registered: st.RegisteredAt,
	}, nil
}

// describe is the operator-facing line naming what was resolved, printed before
// the policy is applied so the pinned account is visible, not implied.
func (p cloudPins) describe() string {
	if !p.FromReg {
		return fmt.Sprintf("pinning explicit owner %s, provider %s", p.Owner, p.Provider)
	}
	account := p.Owner
	if p.OwnerEmail != "" {
		account += " (" + p.OwnerEmail + ")"
	}
	return fmt.Sprintf("resolved from this host's provider registration (%s, registered %s):\n  Harness   %s\n  account   %s\n  provider  %s",
		p.State, ago(p.Registered), orNone(p.Origin), account, p.Provider)
}

// cloudPinDrift explains, for `amux direct cloud status`, an enabled policy
// whose pins no longer match this host's latest provider registration — the
// usual cause of owner_mismatch/provider_mismatch after Harness reassigns or
// recreates the provider. It never changes the policy; re-pinning is an
// explicit `amux direct cloud enable`.
func cloudPinDrift(c direct.CloudStatus, st provider.Status, stErr error) string {
	if !c.Enabled || stErr != nil || st.ProviderID == "" {
		return ""
	}
	if st.ProviderID == c.ProviderPin && (st.OwnerUserID == "" || st.OwnerUserID == c.OwnerPin) {
		return ""
	}
	return fmt.Sprintf("  ⚠ pins differ from this host's provider registration (owner %s, provider %s): issuance will refuse until you re-run amux direct cloud enable",
		orNone(st.OwnerUserID), st.ProviderID)
}
