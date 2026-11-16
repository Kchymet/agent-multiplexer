package harnessproto

import (
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"strings"
)

// Shared turn-end occurrence key dte1 (amux docs/direct-wake-contract.md).
//
// A turn_end event is identified by the session instance (id + incarnation)
// and its exact position in an append-only source: the source kind, the
// source's first-line digest (the part of the source generation after the
// last '.'), and the 1-based index of the event among the source's parsed
// events (the index runtimeevents.ReadSources uses). The direct wake detector
// and the relay provider compute the same key for the same event; the phone
// merges a relay card and a direct card only on an exactly equal non-empty key
// under the verified provider/server pairing.
const (
	TurnKeyPrefix  = "dte1_"
	turnKeyContext = "amux-turn-end-v1"
)

// Turn-key source kinds (runtimeevents.PageSourceKind values), pinned by the
// shared fixtures.
var TurnKeySourceKinds = []string{"structured", "claude", "codex", "permission", "journal"}

// TurnSourcePos locates one event in its source (internal handoff from the
// runtime-events reader to the provider; never on the wire).
type TurnSourcePos struct {
	Kind       string
	Index      int64
	LineDigest string
}

// TurnLineDigest extracts the first-line digest from a source generation
// ("<file identity>.<first-line digest>"), or "" when the file has no
// complete first line yet (or the generation is unknown).
func TurnLineDigest(generation string) string {
	i := strings.LastIndexByte(generation, '.')
	if i < 0 {
		return ""
	}
	return generation[i+1:]
}

// DirectTurnKey returns the dte1 key, or "" when any input is unknown: an
// empty session id, incarnation, kind or line digest, or an index below 1.
// Callers never invent an input (an unlabeled batch has no incarnation and
// gets no key).
func DirectTurnKey(sessionID, incarnation, kind, lineDigest string, index int64) string {
	if sessionID == "" || incarnation == "" || kind == "" || lineDigest == "" || index < 1 {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{turnKeyContext, sessionID, incarnation, kind, lineDigest, strconv.FormatInt(index, 10)}, "\x00")))
	return TurnKeyPrefix + base64.RawURLEncoding.EncodeToString(sum[:16])
}

// ValidTurnKey reports whether s has the dte1 shape.
func ValidTurnKey(s string) bool {
	if !strings.HasPrefix(s, TurnKeyPrefix) {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(s[len(TurnKeyPrefix):])
	return err == nil && len(b) == 16
}
