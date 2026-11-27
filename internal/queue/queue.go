// Package queue is the user question queue's vocabulary: one list of every
// pending question for the user, from any session, with answers routed back to
// the asking session. It also holds the workgroup summary the overseer and
// `amux ls --json` show (needs you / running / waiting for review or merge
// / done).
//
// Entries come in two lifetimes (docs/direct-console.md §4):
//
//   - live questions — an open Claude AskUserQuestion, a permission request,
//     a coordinator's `amux agent goal ask` — are derived by the daemon from
//     runtime state at read time and disappear once answered or stale;
//   - decision_needed items in the global inbox (internal/items) are durable.
//
// An entry's id is deterministic over what identifies the question (session,
// runtime generation, tool-use, request or goal identity), so the same
// question seen through several signals is one entry, and an answer can never
// land on a later question.
package queue

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"amux/internal/core"
	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// Entry kinds.
const (
	KindQuestion   = "question"        // Claude AskUserQuestion
	KindPermission = "permission"      // a runtime permission request
	KindGoalInput  = "goal_input"      // `amux agent goal ask`
	KindDecision   = "decision_needed" // a decision_needed item in the global inbox
)

// Kinds lists the entry kinds in display order.
func Kinds() []string { return []string{KindPermission, KindQuestion, KindGoalInput, KindDecision} }

// Option is one AskUserQuestion choice.
type Option struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// Question is one AskUserQuestion question (a call may ask several).
type Question struct {
	Header      string   `json:"header,omitempty"`
	Question    string   `json:"question"`
	Options     []Option `json:"options,omitempty"`
	MultiSelect bool     `json:"multiSelect,omitempty"`
}

// Entry is one pending question for the user.
type Entry struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	// Session is the asking session; Workgroup its workgroup root ("" outside
	// one); SessionTitle its display name.
	Session      string `json:"session"`
	SessionTitle string `json:"sessionTitle,omitempty"`
	Workgroup    string `json:"workgroup,omitempty"`
	// Title is a one-line summary; Body the full question text (untrusted:
	// render as plain text).
	Title string `json:"title"`
	Body  string `json:"body,omitempty"`
	// Questions are an AskUserQuestion's questions and options.
	Questions []Question `json:"questions,omitempty"`
	// Tool and Action describe a permission request; RequestID and
	// RuntimeGeneration bind its answer.
	Tool              string `json:"tool,omitempty"`
	Action            string `json:"action,omitempty"`
	RequestID         string `json:"requestId,omitempty"`
	RuntimeGeneration string `json:"runtimeGeneration,omitempty"`
	// ToolUseID binds an AskUserQuestion answer to the open call.
	ToolUseID string `json:"toolUseId,omitempty"`
	// Item is the item id of a decision_needed entry; Suggest its suggested
	// action and PRs/Links its references.
	Item    string   `json:"item,omitempty"`
	Suggest string   `json:"suggestedAction,omitempty"`
	PRs     []string `json:"prs,omitempty"`
	Links   []string `json:"links,omitempty"`
	// Created is when the question was raised, when known (Unix ms).
	Created int64 `json:"created,omitempty"`
	// Dismissed marks an entry the user hid (listed only on request).
	Dismissed bool `json:"dismissed,omitempty"`
}

// EntryID derives a stable entry id from the kind and the parts that identify
// the question. Parts are length-prefixed so no two tuples collide.
func EntryID(kind string, parts ...string) string {
	h := sha256.New()
	h.Write([]byte(kind))
	for _, p := range parts {
		h.Write([]byte{0})
		h.Write([]byte(strings.TrimSpace(p)))
	}
	sum := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(h.Sum(nil))
	return kindPrefix(kind) + strings.ToLower(sum[:10])
}

func kindPrefix(kind string) string {
	switch kind {
	case KindQuestion:
		return "q-"
	case KindPermission:
		return "p-"
	case KindGoalInput:
		return "g-"
	case KindDecision:
		return "d-"
	}
	return "x-"
}

// ParseQuestions reads an AskUserQuestion call's input ({"questions":[...]}).
// ok is false for input that is not that shape.
func ParseQuestions(raw json.RawMessage) ([]Question, bool) {
	var in struct {
		Questions []Question `json:"questions"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &in) != nil || len(in.Questions) == 0 {
		return nil, false
	}
	out := in.Questions[:0]
	for _, q := range in.Questions {
		if strings.TrimSpace(q.Question) != "" {
			out = append(out, q)
		}
	}
	return out, len(out) > 0
}

// FormatAnswer renders a typed reply to an AskUserQuestion the way the phone's
// question card does (harness-android#73 questionReply): a lone question is
// answered with its chosen label(s); several are answered one per line as
// "header: answer". choices holds the chosen labels per question; a question
// with no choice takes text (a free-text answer), and with neither is left out.
func FormatAnswer(qs []Question, choices [][]string, text string) string {
	answer := func(i int) string {
		if i < len(choices) && len(choices[i]) > 0 {
			return strings.Join(choices[i], ", ")
		}
		return strings.TrimSpace(text)
	}
	if len(qs) <= 1 {
		return answer(0)
	}
	var lines []string
	for i, q := range qs {
		a := answer(i)
		if a == "" {
			continue
		}
		label := strings.TrimSpace(q.Header)
		if label == "" {
			label = q.Question
		}
		lines = append(lines, label+": "+a)
	}
	return strings.Join(lines, "\n")
}

// ParseChoices splits a choice field: one line per question, labels within a
// line separated by "|" (multi-select).
func ParseChoices(field string) [][]string {
	if strings.TrimSpace(field) == "" {
		return nil
	}
	var out [][]string
	for _, line := range strings.Split(field, "\n") {
		var labels []string
		for _, l := range strings.Split(line, "|") {
			if l = strings.TrimSpace(l); l != "" {
				labels = append(labels, l)
			}
		}
		out = append(out, labels)
	}
	return out
}

// ValidChoices reports whether every chosen label is one of its question's
// options (a question without options takes free text instead).
func ValidChoices(qs []Question, choices [][]string) bool {
	if len(choices) > len(qs) {
		return false
	}
	for i, chosen := range choices {
		if len(chosen) > 1 && !qs[i].MultiSelect {
			return false
		}
		for _, c := range chosen {
			ok := false
			for _, o := range qs[i].Options {
				if o.Label == c {
					ok = true
					break
				}
			}
			if !ok {
				return false
			}
		}
	}
	return true
}

// Counts summarizes the visible (not dismissed) entries.
type Counts struct {
	Total  int            `json:"total"`
	ByKind map[string]int `json:"byKind,omitempty"`
}

// Count tallies entries, leaving dismissed ones out.
func Count(entries []Entry) Counts {
	c := Counts{ByKind: map[string]int{}}
	for _, e := range entries {
		if e.Dismissed {
			continue
		}
		c.Total++
		c.ByKind[e.Kind]++
	}
	return c
}

// Sort orders entries for display: permissions first (a runtime is blocked
// on them), then questions, goal asks and decisions; oldest first within a
// kind, then by id for a stable order.
func Sort(entries []Entry) {
	rank := map[string]int{}
	for i, k := range Kinds() {
		rank[k] = i
	}
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if rank[a.Kind] != rank[b.Kind] {
			return rank[a.Kind] < rank[b.Kind]
		}
		if a.Created != b.Created {
			return a.Created < b.Created
		}
		return a.ID < b.ID
	})
}

// InboxCounts summarizes the global inbox's active items.
type InboxCounts struct {
	Open         int `json:"open"`
	Acknowledged int `json:"acknowledged"`
}

// Workgroup summary buckets.
const (
	BucketNeedsYou = "needs_you"
	BucketRunning  = "running"
	BucketReview   = "review" // waiting for review or merge: open PRs, nothing running
	BucketDone     = "done"   // finished, not archived yet
)

// WorkgroupSummary is one active workgroup's bucket and why.
type WorkgroupSummary struct {
	ID       string   `json:"id"`
	Title    string   `json:"title,omitempty"`
	Bucket   string   `json:"bucket"`
	State    string   `json:"state,omitempty"`
	Goal     string   `json:"goal,omitempty"`
	NeedsYou []string `json:"needsYou,omitempty"`
	OpenPRs  []string `json:"openPrs,omitempty"` // "owner/repo#n"
	Members  int      `json:"members"`
	Queue    int      `json:"queue,omitempty"` // queue entries from this workgroup
}

// Summary is every active workgroup, bucketed.
type Summary struct {
	NeedsYou   int                `json:"needsYou"`
	Running    int                `json:"running"`
	Review     int                `json:"review"`
	Done       int                `json:"done"`
	Workgroups []WorkgroupSummary `json:"workgroups"`
}

// Summarize buckets every active workgroup in a rail snapshot. A workgroup
// needs the user when anyone in it is blocked on the user (#327's
// Supervision.NeedsYou, a waiting state) or it has queue entries; otherwise it
// is running while anything in it runs; otherwise it waits for review or merge
// while any of its PRs is open; otherwise it is done.
func Summarize(rows []core.Session, entries []Entry) Summary {
	queued := map[string]int{}
	for _, e := range entries {
		if !e.Dismissed && e.Workgroup != "" {
			queued[e.Workgroup]++
		}
	}
	type acc struct {
		root    core.Session
		members int
		running bool
		open    []string
	}
	groups := map[string]*acc{}
	var order []string
	for _, r := range rows {
		if r.IsRoot && r.Role == "coordinator" && !r.Archived && r.Section != core.SectionArchived {
			groups[r.ID] = &acc{root: r}
			order = append(order, r.ID)
		}
	}
	addPRs := func(a *acc, r core.Session) {
		if r.PRs == nil {
			return
		}
		for _, pr := range *r.PRs {
			if pr.State == "open" {
				a.open = append(a.open, pr.Repo+"#"+strconv.Itoa(pr.Number))
			}
		}
	}
	for _, r := range rows {
		if r.Archived || r.Section == core.SectionArchived {
			continue
		}
		if a := groups[r.ID]; a != nil {
			addPRs(a, r)
			continue
		}
		a := groups[r.RootID]
		if a == nil || r.IsRoot {
			continue
		}
		a.members++
		if r.State == core.StateRunning {
			a.running = true
		}
		addPRs(a, r)
	}
	var s Summary
	for _, id := range order {
		a := groups[id]
		w := WorkgroupSummary{ID: id, Title: a.root.Title, State: a.root.State, Members: a.members, OpenPRs: a.open, Queue: queued[id]}
		var sup harnessproto.Supervision
		if a.root.Supervision != nil {
			sup = *a.root.Supervision
		}
		w.Goal, w.NeedsYou = sup.Goal, sup.NeedsYou
		switch {
		case len(sup.NeedsYou) > 0 || a.root.State == core.StateWaiting || w.Queue > 0:
			w.Bucket = BucketNeedsYou
			s.NeedsYou++
		case a.root.State == core.StateRunning || sup.OwnState == core.StateRunning || a.running:
			w.Bucket = BucketRunning
			s.Running++
		case len(a.open) > 0:
			w.Bucket = BucketReview
			s.Review++
		default:
			w.Bucket = BucketDone
			s.Done++
		}
		s.Workgroups = append(s.Workgroups, w)
	}
	return s
}

// Overview is the queue read (core.QueryQueue): the entries, their counts, the
// global inbox counts and the workgroup summary.
type Overview struct {
	Entries []Entry     `json:"entries"`
	Counts  Counts      `json:"counts"`
	Inbox   InboxCounts `json:"inbox"`
	Summary Summary     `json:"summary"`
	// Errors lists sources the daemon could not read this time (best effort;
	// the rest of the overview stands).
	Errors []string `json:"errors,omitempty"`
}
