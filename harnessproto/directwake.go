package harnessproto

import (
	"crypto/sha256"
	"encoding/base64"
)

// DirectWakeV1 is content-free cloud wake-ups for direct clients
// (docs/direct-wake-contract.md). A provider advertises it only while the host
// operator has enabled wake-ups on this host's `amux direct serve`
// (`amux direct wake enable`) and the provider was started with --direct-wake.
// It needs neither "sessions" nor "runtime-events": the direct server detects
// events itself and the provider forwards only opaque routing handles.
//
// The orchestrator accepts the feature by echoing it in registered.features;
// a provider sends direct-wake* frames only after that echo. Each frame is
// answered by exactly one direct-wake-result with the same ReqID.
//
// No frame carries session ids, titles, prompts, permission text, request ids,
// runtime generations, or any direct token, verifier or bundle. The only
// secret is ActivationSecret on direct-wake-activate: it must never be logged
// and the orchestrator stores only its SHA-256 (the challenge the phone
// registered). No frame may be queued durably or replayed by the orchestrator.
const (
	DirectWakeV1 = "direct-wake-v1"

	HDirectWakeActivate = "direct-wake-activate" // provider -> orchestrator
	HDirectWake         = "direct-wake"          // provider -> orchestrator
	HDirectWakeRoutes   = "direct-wake-routes"   // provider -> orchestrator
	MDirectWakeResult   = "direct-wake-result"   // orchestrator -> provider

	// MaxDirectWakeBatch bounds the wakes in one direct-wake frame.
	MaxDirectWakeBatch = 16
	// MaxDirectWakeRoutes bounds the routes in one direct-wake-routes part.
	MaxDirectWakeRoutes = 512

	// Opaque handle prefixes: prefix + unpadded base64url of N random bytes.
	DirectWakeRoutePrefix = "dwr_" // 32 bytes, minted by the orchestrator
	DirectWakeIDPrefix    = "dwk_" // 16 bytes, minted by the host
)

// Direct wake result statuses (DirectWakeResultItem.Status) and the
// frame-level DirectWakeResult.Error.
const (
	// Delivered to every recipient device (or an exact replay of an accepted
	// wake), or the activation was accepted. Terminal.
	DirectWakeAccepted = "accepted"
	// The route's client has no registered device. Terminal for this wake.
	DirectWakeNoRecipient = "no_recipient"
	// The route is unknown, foreign, revoked, expired, or its binding does
	// not match. Terminal: the host marks the subscription failed.
	DirectWakeRouteGone = "route_gone"
	// Rate limit exceeded. Retryable.
	DirectWakeRateLimited = "rate_limited"
	// Transient, or the route is still pending activation. Retryable.
	DirectWakeUnavailable = "unavailable"
	// A concurrent delivery attempt holds the lease, or too many frames are
	// in flight. Retryable; never a terminal acknowledgment.
	DirectWakeBusy = "busy"
)

// Subscription states carried by direct-wake-routes.
const (
	DirectWakeStateActivating = "activating"
	DirectWakeStateActive     = "active"
)

// DirectWakeCapabilities advertises the host's direct server for wake-ups
// (Capabilities.DirectWake). ServerID is the direct server identity the
// orchestrator binds routes to.
type DirectWakeCapabilities struct {
	ServerID string `json:"serverId"`
}

// DirectWakeRef is one opaque wake. GrantRef is the host's opaque reference
// to the subscribing direct grant; it must match the route's activated grant.
type DirectWakeRef struct {
	RouteID  string `json:"routeId"`
	WakeID   string `json:"wakeId"`
	GrantRef string `json:"grantRef"`
}

// DirectWakeRouteRef is one subscription in a direct-wake-routes snapshot.
type DirectWakeRouteRef struct {
	RouteID  string `json:"routeId"`
	GrantRef string `json:"grantRef"`
	State    string `json:"state"` // activating | active
}

// DirectWake is the body of every provider -> orchestrator direct-wake* frame
// (HarnessMsg.DirectWake). Which fields are set depends on the frame type:
//
//   - direct-wake-activate: RouteID, ActivationSecret, GrantRef, Requester?
//   - direct-wake: Wakes
//   - direct-wake-routes: Epoch, Routes, Part, Final
//
// ServerID is always set and must equal the connection's advertised
// DirectWakeCapabilities.ServerID.
type DirectWake struct {
	ServerID string `json:"serverId"`

	// direct-wake-activate. Requester is set only when the subscribing grant
	// was cloud-brokered (the Harness user id recorded on it at redemption).
	RouteID          string `json:"routeId,omitempty"`
	ActivationSecret string `json:"activationSecret,omitempty"`
	GrantRef         string `json:"grantRef,omitempty"`
	Requester        string `json:"requester,omitempty"`

	// direct-wake.
	Wakes []DirectWakeRef `json:"wakes,omitempty"`

	// direct-wake-routes. A snapshot is the parts 0..n of one Epoch; Final is
	// set on part n. The orchestrator applies only a complete epoch.
	Epoch  string               `json:"epoch,omitempty"`
	Routes []DirectWakeRouteRef `json:"routes,omitempty"`
	Part   int                  `json:"part,omitempty"`
	Final  bool                 `json:"final,omitempty"`
}

// DirectWakeResultItem is the orchestrator's verdict on one route or wake.
type DirectWakeResultItem struct {
	RouteID string `json:"routeId"`
	WakeID  string `json:"wakeId,omitempty"`
	Status  string `json:"status"`
}

// DirectWakeResult is the direct-wake-result body (MuxMsg.DirectWakeResult).
// Error is set (and Results empty) when the whole frame was refused:
// DirectWakeUnavailable (connection-level mismatch) or DirectWakeBusy.
type DirectWakeResult struct {
	Results []DirectWakeResultItem `json:"results,omitempty"`
	Error   string                 `json:"error,omitempty"`
}

// ValidDirectWakeRouteID reports whether s is a well-formed route handle.
func ValidDirectWakeRouteID(s string) bool { return validHandle(s, DirectWakeRoutePrefix, 32) }

// ValidDirectWakeID reports whether s is a well-formed wake handle.
func ValidDirectWakeID(s string) bool { return validHandle(s, DirectWakeIDPrefix, 16) }

// ValidDirectWakeSecret reports whether s is an activation secret: unpadded
// base64url of exactly 32 bytes.
func ValidDirectWakeSecret(s string) bool { return validHandle(s, "", 32) }

// DirectWakeChallenge is the activation challenge for a secret: unpadded
// base64url of its SHA-256. The phone registers the challenge; the
// orchestrator compares it with the secret the host relays.
func DirectWakeChallenge(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func validHandle(s, prefix string, n int) bool {
	if len(s) != len(prefix)+base64.RawURLEncoding.EncodedLen(n) || s[:len(prefix)] != prefix {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(s[len(prefix):])
	return err == nil && len(b) == n
}

// validServerID admits the direct server identity shape: the RFC 7638 SHA-256
// JWK thumbprint, unpadded base64url of 32 bytes (docs/direct-access.md).
func validServerID(s string) bool { return validHandle(s, "", 32) }

// validRequester admits an empty requester or a Harness user id of 1-128
// printable bytes (the direct authority's principal rule).
func validRequester(s string) bool {
	if len(s) > 128 {
		return false
	}
	for _, r := range s {
		if r <= 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func validGrantRef(s string) bool { return len(s) == 22 && validB64(s) }

func validB64(s string) bool {
	_, err := base64.RawURLEncoding.DecodeString(s)
	return err == nil
}

// MaxDirectWakeRouteParts bounds one route-sync epoch (parts 0..n).
const MaxDirectWakeRouteParts = 8

// ValidDirectWakeFrame checks a provider frame's shape for its type (not its
// authority). Each frame type admits only its own fields: a field belonging to
// another frame type makes the frame invalid, so a secret can never ride on a
// wake or routes frame and a wake can never smuggle activation fields.
func ValidDirectWakeFrame(typ string, w *DirectWake) bool {
	if w == nil || !validServerID(w.ServerID) || !validRequester(w.Requester) {
		return false
	}
	activation := w.RouteID != "" || w.ActivationSecret != "" || w.GrantRef != "" || w.Requester != ""
	wakes := len(w.Wakes) != 0
	sync := w.Epoch != "" || len(w.Routes) != 0 || w.Part != 0 || w.Final
	switch typ {
	case HDirectWakeActivate:
		return !wakes && !sync && ValidDirectWakeRouteID(w.RouteID) && ValidDirectWakeSecret(w.ActivationSecret) &&
			validGrantRef(w.GrantRef)
	case HDirectWake:
		if activation || sync || len(w.Wakes) > MaxDirectWakeBatch || !wakes {
			return false
		}
		for _, r := range w.Wakes {
			if !ValidDirectWakeRouteID(r.RouteID) || !ValidDirectWakeID(r.WakeID) || !validGrantRef(r.GrantRef) {
				return false
			}
		}
		return true
	case HDirectWakeRoutes:
		if activation || wakes || w.Epoch == "" || len(w.Epoch) > 64 || len(w.Routes) > MaxDirectWakeRoutes ||
			w.Part < 0 || w.Part >= MaxDirectWakeRouteParts {
			return false
		}
		for _, r := range w.Routes {
			if !ValidDirectWakeRouteID(r.RouteID) || !validGrantRef(r.GrantRef) ||
				(r.State != DirectWakeStateActivating && r.State != DirectWakeStateActive) {
				return false
			}
		}
		return true
	}
	return false
}
