// Package items defines amux's upward signals: structured items a session
// raises at the end of a turn or of its work — a follow-up, outstanding work,
// a decision it needs, a risk, or something deliberately left out of scope —
// instead of prose buried in a final message.
//
// An item travels upward. A workgroup member raises it to its coordinator; a
// coordinator triages what it receives (resolves it, or escalates it to the
// global inbox), and what a coordinator (or any session outside a workgroup)
// raises goes to the global inbox directly. The global inbox spans every
// workgroup; its owner is resolved in one place (GlobalInbox).
//
// This package is the vocabulary and the pure rules: kinds, states, input
// validation and normalization, the de-duplication key and the transition
// table. Persistence is internal/store (items.go), authorization is
// internal/access (items.go), and the daemon (internal/daemon/items.go) wires
// both behind the authenticated session RPC and the wake-up queue.
package items

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Kinds (Item.Kind).
const (
	KindFollowUp       = "follow_up"       // a follow-up worth doing after this work
	KindOutstanding    = "outstanding"     // work this session did not finish
	KindDecisionNeeded = "decision_needed" // a decision only someone upstream can make
	KindRisk           = "risk"            // a risk the recipient should know and weigh
	KindFYI            = "fyi"             // deliberately out of scope, or plain information
)

// Kinds lists every kind in display order.
func Kinds() []string {
	return []string{KindFollowUp, KindOutstanding, KindDecisionNeeded, KindRisk, KindFYI}
}

// KnownKind reports whether kind is one of Kinds.
func KnownKind(kind string) bool {
	for _, k := range Kinds() {
		if k == kind {
			return true
		}
	}
	return false
}

// States (Item.State). Open and acknowledged items are active: they are in
// someone's inbox and de-duplicate new raises. Dismissed and done are closed.
const (
	StateOpen         = "open"
	StateAcknowledged = "acknowledged"
	StateDismissed    = "dismissed"
	StateDone         = "done"
)

// States lists every state.
func States() []string {
	return []string{StateOpen, StateAcknowledged, StateDismissed, StateDone}
}

// KnownState reports whether state is one of States.
func KnownState(state string) bool {
	for _, s := range States() {
		if s == state {
			return true
		}
	}
	return false
}

// Active reports whether state is open or acknowledged.
func Active(state string) bool { return state == StateOpen || state == StateAcknowledged }

// GlobalInbox is the recipient id of the global inbox: the one owner of the
// cross-workgroup inbox that escalations and archive carry-overs go to, and
// that receives the items of sessions outside a coordinated workgroup. It is
// resolved here and only here, so the owner can be repointed in one place: it
// is the overseer (internal/overseer.ID), the coordinator of coordinators. The
// admin console keeps an all-items read and triage view as an operator
// override, but is no longer escalated to.
func GlobalInbox() string { return globalInboxOwner }

const globalInboxOwner = "overseer"

// IsGlobalInbox reports whether id is the global inbox owner.
func IsGlobalInbox(id string) bool { return id != "" && id == GlobalInbox() }

// History actions (Event.Action).
const (
	EventRaised       = "raised"
	EventUpdated      = "updated" // a de-duplicated re-raise refreshed the item
	EventAcknowledged = "acknowledged"
	EventDismissed    = "dismissed"
	EventDone         = "done"
	EventReopened     = "reopened"
	EventEscalated    = "escalated"
	// EventCarriedOver: the recipient was archived or deleted with the item
	// still active, so the item moved to the global inbox.
	EventCarriedOver = "carried_over"
)

// Event is one entry of an item's history: who moved it, between which
// recipients, and why. Every escalation and carry-over is recorded here.
type Event struct {
	At     int64  `json:"at"`
	By     string `json:"by,omitempty"`
	Action string `json:"action"`
	From   string `json:"from,omitempty"`
	To     string `json:"to,omitempty"`
	Note   string `json:"note,omitempty"`
	// Host marks a change the host (the user, at the CLI, TUI or a future
	// direct client) made rather than a session: a user acknowledgment is what
	// lets the overseer create a workgroup from an item.
	Host bool `json:"host,omitempty"`
}

// MaxHistory bounds an item's history; the oldest entries after the first
// (the raise) are dropped.
const MaxHistory = 32

// Item is one upward signal. Timestamps are Unix milliseconds.
type Item struct {
	ID     string   `json:"id"`
	Kind   string   `json:"kind"`
	Title  string   `json:"title"`
	Detail string   `json:"detail,omitempty"`
	PRs    []string `json:"prs,omitempty"`   // "owner/repo#n" or "repo#n"
	Links  []string `json:"links,omitempty"` // http(s) URLs
	// Suggest is the action the source suggests to the recipient.
	Suggest string `json:"suggested_action,omitempty"`
	State   string `json:"state"`
	// Source is the session that raised the item; Workgroup is its
	// coordinator's id when it was raised ("" outside a workgroup).
	Source    string `json:"source"`
	Workgroup string `json:"workgroup,omitempty"`
	// Recipient is whose inbox the item is in now: a coordinator's session id
	// or the global inbox owner (GlobalInbox).
	Recipient string `json:"recipient"`
	// Raised counts how many times the source raised it (de-duplicated
	// re-raises refresh the item and increment this).
	Raised  int     `json:"raised"`
	Created int64   `json:"created"`
	Updated int64   `json:"updated"`
	Closed  int64   `json:"closed,omitempty"`
	History []Event `json:"history,omitempty"`
	// Unnotified is true while the current recipient has not yet been woken
	// for the item's latest change.
	Unnotified bool `json:"unnotified,omitempty"`
}

// Active reports whether the item is open or acknowledged.
func (it Item) Active() bool { return Active(it.State) }

// AddEvent appends to the history, keeping the raise and the newest entries.
func (it *Item) AddEvent(ev Event) {
	it.History = append(it.History, ev)
	if n := len(it.History); n > MaxHistory {
		it.History = append(it.History[:1:1], it.History[n-MaxHistory+1:]...)
	}
}

// Input bounds.
const (
	MaxTitleRunes   = 200
	MaxDetailRunes  = 4000
	MaxSuggestRunes = 1000
	MaxNoteRunes    = 1000
	MaxRefs         = 10
	MaxRefBytes     = 512
	// MaxActivePerSource bounds the active items one session may have raised,
	// so a looping agent cannot flood an inbox.
	MaxActivePerSource = 100
)

// Raise is a validated request to raise an item.
type Raise struct {
	Kind    string
	Title   string
	Detail  string
	PRs     []string
	Links   []string
	Suggest string
}

var prRef = regexp.MustCompile(`^(?:[A-Za-z0-9_.-]+/)?[A-Za-z0-9_.-]+#[1-9][0-9]{0,9}$`)

// Validate normalizes and checks a raise. It trims text, rejects control
// characters (newlines and tabs are allowed in the detail and suggestion),
// bounds every field, and de-duplicates the references.
func (r Raise) Validate() (Raise, error) {
	out := Raise{Kind: strings.TrimSpace(r.Kind)}
	if !KnownKind(out.Kind) {
		return Raise{}, fmt.Errorf("unknown kind %q (want one of %s)", r.Kind, strings.Join(Kinds(), ", "))
	}
	var err error
	if out.Title, err = cleanText("title", r.Title, MaxTitleRunes, false); err != nil {
		return Raise{}, err
	}
	if out.Title == "" {
		return Raise{}, fmt.Errorf("title is required")
	}
	if out.Detail, err = cleanText("detail", r.Detail, MaxDetailRunes, true); err != nil {
		return Raise{}, err
	}
	if out.Suggest, err = cleanText("suggested action", r.Suggest, MaxSuggestRunes, true); err != nil {
		return Raise{}, err
	}
	if out.PRs, err = cleanRefs("pr", r.PRs, func(s string) bool { return prRef.MatchString(s) }); err != nil {
		return Raise{}, err
	}
	if out.Links, err = cleanRefs("link", r.Links, validLink); err != nil {
		return Raise{}, err
	}
	return out, nil
}

// CleanNote validates a free-text note (escalation, state change).
func CleanNote(note string) (string, error) {
	return cleanText("note", note, MaxNoteRunes, true)
}

func cleanText(field, s string, maxRunes int, multiline bool) (string, error) {
	if !utf8.ValidString(s) {
		return "", fmt.Errorf("%s is not valid UTF-8", field)
	}
	s = strings.TrimSpace(s)
	for _, r := range s {
		if r == '\n' || r == '\t' {
			if multiline {
				continue
			}
			return "", fmt.Errorf("%s must be one line", field)
		}
		if unicode.IsControl(r) || r == ' ' || r == ' ' {
			return "", fmt.Errorf("%s contains a control character", field)
		}
	}
	if utf8.RuneCountInString(s) > maxRunes {
		return "", fmt.Errorf("%s is longer than %d characters", field, maxRunes)
	}
	return s, nil
}

func cleanRefs(field string, refs []string, valid func(string) bool) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}
		if len(ref) > MaxRefBytes || !valid(ref) {
			return nil, fmt.Errorf("invalid %s %q", field, ref)
		}
		if seen[ref] {
			continue
		}
		seen[ref] = true
		out = append(out, ref)
	}
	if len(out) > MaxRefs {
		return nil, fmt.Errorf("at most %d %ss", MaxRefs, field)
	}
	return out, nil
}

func validLink(s string) bool {
	for _, r := range s {
		if r <= ' ' || r == 0x7f {
			return false
		}
	}
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != ""
}

// MergeRefs returns a followed by the members of b it lacks, bounded to
// MaxRefs (a de-duplicated re-raise accumulates references).
func MergeRefs(a, b []string) []string {
	out := append([]string(nil), a...)
	seen := map[string]bool{}
	for _, s := range a {
		seen[s] = true
	}
	for _, s := range b {
		if !seen[s] && len(out) < MaxRefs {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// DedupeKey normalizes a title for de-duplication: case-folded, whitespace
// collapsed, surrounding punctuation trimmed. Same source + kind + key on an
// active item is the same item.
func DedupeKey(title string) string {
	fields := strings.Fields(strings.ToLower(title))
	key := strings.Join(fields, " ")
	return strings.TrimFunc(key, func(r rune) bool { return unicode.IsPunct(r) || unicode.IsSpace(r) })
}

// Transition verbs (the state an item is moved to by name).
const (
	VerbAck     = "ack"
	VerbDismiss = "dismiss"
	VerbDone    = "done"
	VerbReopen  = "reopen"
)

// Verbs lists the transition verbs.
func Verbs() []string { return []string{VerbAck, VerbDismiss, VerbDone, VerbReopen} }

// TargetState maps a transition verb (or a state name) to the state it sets.
func TargetState(verb string) (string, bool) {
	switch verb {
	case VerbAck, StateAcknowledged:
		return StateAcknowledged, true
	case VerbDismiss, StateDismissed:
		return StateDismissed, true
	case VerbDone:
		return StateDone, true
	case VerbReopen, StateOpen:
		return StateOpen, true
	}
	return "", false
}

// CanTransition reports whether an item in state from may move to state to.
// Active items may be acknowledged, dismissed or marked done (acknowledging
// an acknowledged item is refused as a no-op); closed items may only be
// reopened, and an acknowledged item may be reopened to unread it.
func CanTransition(from, to string) bool {
	switch to {
	case StateAcknowledged:
		return from == StateOpen
	case StateDismissed, StateDone:
		return Active(from)
	case StateOpen:
		return from != StateOpen && KnownState(from)
	}
	return false
}

// EventFor names the history action of a transition to state.
func EventFor(state string) string {
	switch state {
	case StateAcknowledged:
		return EventAcknowledged
	case StateDismissed:
		return EventDismissed
	case StateDone:
		return EventDone
	case StateOpen:
		return EventReopened
	}
	return state
}

// NewID mints an item id ("it-" + 8 hex digits).
func NewID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return "it-" + hex.EncodeToString(b[:])
}

// ValidID reports whether id has NewID's shape.
func ValidID(id string) bool {
	if len(id) != 11 || !strings.HasPrefix(id, "it-") {
		return false
	}
	_, err := hex.DecodeString(id[3:])
	return err == nil && strings.ToLower(id) == id
}

// List is a page of items for a reader, newest first.
type List struct {
	Items []Item `json:"items"`
	// Truncated is true when more items matched than the page holds.
	Truncated bool `json:"truncated,omitempty"`
}
