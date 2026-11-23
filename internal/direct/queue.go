package direct

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"amux/internal/core"
	"amux/internal/daemon"
	"amux/internal/hostbackend"
	"amux/internal/items"
	"amux/internal/queue"

	"github.com/gorilla/websocket"
	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

// The user question queue and the overseer over direct access
// (docs/direct-console.md §5, docs/direct-access.md "Question queue").
//
// Every route reads fresh daemon state and projects it to the grant's scope:
// an all_sessions grant sees every entry and global-inbox item except the admin
// console's; a workgroups grant sees only entries and items of its own
// workgroups. Answers and triage run through the daemon's own host verbs
// (ActionQueueAnswer, ActionItemState), which re-check the entry against fresh
// runtime state before delivering anything.

// Queue routes.
const (
	PathQueue      = "/direct/v1/queue"
	PathQueueWatch = "/direct/v1/queue/watch"
	PathSummary    = "/direct/v1/summary"
	// PathItems is POST PathItems/{item_id}/state: triage one global-inbox item.
	PathItems = "/direct/v1/items"
	// IncludeOverseer is the GET /sessions and /sessions/watch query value
	// (?include=overseer) that adds the overseer row for a grant that may see it.
	IncludeOverseer = "overseer"
)

// Queue error codes (409).
const (
	CodeAskerGone = "asker_gone" // the question is no longer open: answered elsewhere, replaced, or its session went away
	CodeAnswered  = "answered"   // someone already answered it (or is answering it: first answer wins)
	// Question prompts v2 (docs/question-prompts.md).
	CodeNeedsNewerClient = hp.CodeNeedsNewerClient // the legacy answer fields cannot answer this form
	CodeDeliveryFailed   = hp.CodeDeliveryFailed   // amux could not deliver; nothing reached the agent, the entry is open again
	CodeAnswerInPane     = hp.CodeAnswerInPane     // amux cannot deliver this form; answer it in the agent's pane
)

// queueConflict is the client message for a 409 queue code, or "" when code
// is not one.
func queueConflict(code string) string {
	switch code {
	case CodeAskerGone:
		return "the question is no longer open: it was answered elsewhere, replaced, or its session went away"
	case CodeAnswered:
		return "this question was already answered"
	case CodeNeedsNewerClient:
		return "this question needs a newer amux client to answer"
	case CodeDeliveryFailed:
		return "amux could not deliver the answer; nothing reached the agent and the question is open again"
	case CodeAnswerInPane:
		return "amux cannot deliver an answer to this question; answer it in the agent's pane"
	}
	return ""
}

// Entry states. A listed entry is always open: an answered or stale question
// leaves the list, and answering it reports 409 answered or asker_gone.
const EntryStateOpen = "open"

// Item triage states (the #330 item verbs; the item state names they produce
// are open, acknowledged, dismissed, done).
const (
	TriageAck     = "ack"
	TriageDismiss = "dismiss"
	TriageDone    = "done"
	TriageReopen  = "reopen"
)

// QueueOption is one choice of an AskUserQuestion question.
type QueueOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// QueueQuestion is one question of an AskUserQuestion call.
type QueueQuestion struct {
	Header      string        `json:"header,omitempty"`
	Question    string        `json:"question"`
	Options     []QueueOption `json:"options"`
	MultiSelect bool          `json:"multi_select"`
}

// QueueEntry is one pending question for the user. Text fields come from
// agents and repositories: clients render them as plain text.
type QueueEntry struct {
	ID           string `json:"id"`
	Kind         string `json:"kind"`  // question | permission | goal_input | decision_needed
	State        string `json:"state"` // always "open"
	SessionID    string `json:"session_id"`
	SessionTitle string `json:"session_title,omitempty"`
	Workgroup    string `json:"workgroup,omitempty"`
	Title        string `json:"title"`
	Body         string `json:"body,omitempty"`
	// CreatedAt is Unix milliseconds, when known.
	CreatedAt int64 `json:"created_at,omitempty"`
	// Questions: kind question.
	Questions []QueueQuestion `json:"questions,omitempty"`
	// Form is the question prompt v2 envelope (docs/question-prompts.md):
	// every feature of the question and how amux delivers an answer. A client
	// that understands it answers with QueueAnswerRequest.Form; Questions is
	// the legacy view (absent when a legacy client cannot answer correctly).
	Form *hp.Form `json:"form,omitempty"`
	// Tool, Action, RequestID, RuntimeGeneration: kind permission. Echo
	// runtime_generation back unchanged (a string) when answering.
	Tool              string `json:"tool,omitempty"`
	Action            string `json:"action,omitempty"`
	RequestID         string `json:"request_id,omitempty"`
	RuntimeGeneration string `json:"runtime_generation,omitempty"`
	// ItemID, SuggestedAction, PRs, Links: kind decision_needed.
	ItemID          string   `json:"item_id,omitempty"`
	SuggestedAction string   `json:"suggested_action,omitempty"`
	PRs             []string `json:"prs,omitempty"`
	Links           []string `json:"links,omitempty"`
}

// InboxItem is one active global-inbox item (internal/items), projected:
// no history, recipients or notification bookkeeping. Timestamps are Unix
// milliseconds. PRs are "owner/repo#n" or "repo#n" strings; links are URLs.
type InboxItem struct {
	ID              string   `json:"id"`
	Kind            string   `json:"kind"` // follow_up | outstanding | decision_needed | risk | fyi
	Title           string   `json:"title"`
	Detail          string   `json:"detail,omitempty"`
	PRs             []string `json:"prs,omitempty"`
	Links           []string `json:"links,omitempty"`
	SuggestedAction string   `json:"suggested_action,omitempty"`
	State           string   `json:"state"` // open | acknowledged
	Source          string   `json:"source"`
	Workgroup       string   `json:"workgroup,omitempty"`
	Raised          int      `json:"raised,omitempty"`
	Created         int64    `json:"created"`
	Updated         int64    `json:"updated"`
}

// QueueResponse is GET /direct/v1/queue: the entries and the active global
// inbox items in one consistent read. A decision_needed item appears both as
// an item and as a decision_needed entry (whose item_id names it).
type QueueResponse struct {
	ServerID string       `json:"server_id"`
	Entries  []QueueEntry `json:"entries"`
	Items    []InboxItem  `json:"items"`
}

// SummaryBuckets counts the grant's active workgroups per bucket.
type SummaryBuckets struct {
	NeedsYou      int `json:"needs_you"`
	Running       int `json:"running"`
	WaitingReview int `json:"waiting_review"`
	Done          int `json:"done"`
}

// Summary bucket names (SummaryWorkgroup.Bucket).
const (
	BucketNeedsYou      = "needs_you"
	BucketRunning       = "running"
	BucketWaitingReview = "waiting_review"
	BucketDone          = "done"
)

// SummaryWorkgroup is one active workgroup and why it is in its bucket.
type SummaryWorkgroup struct {
	ID       string   `json:"id"`
	Title    string   `json:"title,omitempty"`
	Bucket   string   `json:"bucket"`
	State    string   `json:"state,omitempty"`
	Goal     string   `json:"goal,omitempty"`
	NeedsYou []string `json:"needs_you,omitempty"`
	OpenPRs  []string `json:"open_prs,omitempty"`
	Members  int      `json:"members"`
	Queue    int      `json:"queue,omitempty"` // open queue entries from this workgroup
}

// Summary is the workgroup summary.
type Summary struct {
	Buckets    SummaryBuckets     `json:"buckets"`
	Workgroups []SummaryWorkgroup `json:"workgroups"`
}

// SummaryResponse is GET /direct/v1/summary.
type SummaryResponse struct {
	ServerID string `json:"server_id"`
	Summary
}

// QueueWatchSnapshot is one /queue/watch frame: the whole queue, the items
// and the summary, sent on connect and whenever any of them changes.
type QueueWatchSnapshot struct {
	Type    string       `json:"type"` // "snapshot"
	Entries []QueueEntry `json:"entries"`
	Items   []InboxItem  `json:"items"`
	Summary Summary      `json:"summary"`
}

// QueueAnswerRequest is POST /direct/v1/queue/{entry_id}/answer. Per kind:
//
//   - question: answer (free text) or choice (option labels; only for an entry
//     with exactly one question, several labels only when it is multi_select);
//     runtime_generation is accepted and ignored;
//   - permission: decision (allow | deny) and runtime_generation (the entry's,
//     unchanged);
//   - goal_input, decision_needed: answer.
type QueueAnswerRequest struct {
	IdemKey           string   `json:"idem_key"`
	Answer            string   `json:"answer,omitempty"`
	Choice            []string `json:"choice,omitempty"`
	Decision          string   `json:"decision,omitempty"`
	RuntimeGeneration string   `json:"runtime_generation,omitempty"`
	// Form answers a question entry with every feature of its form (send it
	// only when capabilities.queue.form says the server takes it; an older
	// server refuses the unknown key). Exclusive with answer and choice.
	Form *hp.FormAnswer `json:"form,omitempty"`
}

// ItemStateRequest is POST /direct/v1/items/{item_id}/state.
type ItemStateRequest struct {
	State   string `json:"state"` // ack | dismiss | done | reopen
	IdemKey string `json:"idem_key"`
}

// ItemStateResponse answers it with the item's new state.
type ItemStateResponse struct {
	OK    bool   `json:"ok"`
	State string `json:"state"`
}

// QueueCapabilities is CapabilitiesResponse.Queue. Absent: the server predates
// the queue (its routes answer 404 without a JSON code).
type QueueCapabilities struct {
	Read bool `json:"read"`
	// AnswerKinds are the entry kinds this grant may answer: question,
	// goal_input and decision_needed with steer; permission with approve.
	AnswerKinds []string `json:"answer_kinds"`
	// Triage: dismiss, done and reopen of in-scope items (steer).
	Triage bool `json:"triage"`
	// Acknowledge: ack, the user's go-ahead that lets the overseer create a
	// workgroup from an item (steer + create on an all_sessions grant).
	Acknowledge bool `json:"acknowledge"`
	// Form: question entries carry a v2 form and answers may be one. Absent:
	// the server predates question prompts v2.
	Form *FormCapabilities `json:"form,omitempty"`
}

// FormCapabilities is QueueCapabilities.Form.
type FormCapabilities struct {
	Version int      `json:"version"` // hp.FormVersion
	Sources []string `json:"sources"` // form sources this server emits
}

// formCaps is what this server offers for question forms.
func formCaps() *FormCapabilities {
	return &FormCapabilities{Version: hp.FormVersion, Sources: []string{hp.FormSourceClaudeAsk, hp.FormSourceCodexUserInput}}
}

// OverseerCapabilities is CapabilitiesResponse.Overseer.
type OverseerCapabilities struct {
	// Read: ?include=overseer adds its row and its history/stream are readable
	// (an all_sessions grant).
	Read bool `json:"read"`
	// Steer: prompt, interject and stop on it (all_sessions + steer + manage +
	// create: everything it could do itself).
	Steer bool `json:"steer"`
}

// QueueBackend is the Backend's optional queue surface (HostBackend).
type QueueBackend interface {
	Queue(ctx context.Context) (queue.Overview, error)
	InboxItems(ctx context.Context) ([]items.Item, error)
	Item(ctx context.Context, id string) (items.Item, bool, error)
}

func (b HostBackend) Queue(ctx context.Context) (queue.Overview, error) { return b.H.Queue(ctx) }
func (b HostBackend) InboxItems(ctx context.Context) ([]items.Item, error) {
	return b.H.InboxItems(ctx)
}
func (b HostBackend) Item(ctx context.Context, id string) (items.Item, bool, error) {
	return b.H.Item(ctx, id)
}

// mayDriveOverseer reports whether a grant may steer the overseer: it must
// already hold everything the overseer can do (steer coordinators, archive
// and start workgroups, create workgroups), so steering it widens nothing.
func mayDriveOverseer(g Grant) bool {
	return g.Resource.Kind == ResourceAllSessions && g.Can(PermSteer) && g.Can(PermManage) && g.Can(PermCreate)
}

// overseerVerbAllowed reports whether verb may target the overseer for g.
func overseerVerbAllowed(g Grant, verb string) bool {
	switch verb {
	case VerbPrompt, VerbInterject, VerbStop:
		return mayDriveOverseer(g)
	}
	return false
}

// answerPerm is the permission answering an entry of kind needs.
func answerPerm(kind string) string {
	if kind == queue.KindPermission {
		return PermApprove
	}
	return PermSteer
}

// answerKinds lists the kinds g may answer.
func answerKinds(g Grant) []string {
	out := []string{}
	for _, k := range []string{queue.KindQuestion, queue.KindPermission, queue.KindGoalInput, queue.KindDecision} {
		if g.Can(answerPerm(k)) {
			out = append(out, k)
		}
	}
	return out
}

// mayAcknowledge reports whether g may acknowledge items (the user's
// go-ahead for the overseer's create-from-item).
func mayAcknowledge(g Grant) bool {
	return g.Can(PermSteer) && g.Can(PermCreate) && g.Resource.Kind == ResourceAllSessions
}

func queueCaps(g Grant) (*QueueCapabilities, *OverseerCapabilities) {
	return &QueueCapabilities{Read: true, AnswerKinds: answerKinds(g), Triage: g.Can(PermSteer), Acknowledge: mayAcknowledge(g), Form: formCaps()},
		&OverseerCapabilities{Read: g.Resource.Kind == ResourceAllSessions, Steer: mayDriveOverseer(g)}
}

var entryIDRE = regexp.MustCompile(`^[qpgd]-[a-z2-7]{10}$`)

// entryKind reads an entry id's kind from its prefix (queue.EntryID).
func entryKind(id string) (string, bool) {
	if !entryIDRE.MatchString(id) {
		return "", false
	}
	switch id[0] {
	case 'q':
		return queue.KindQuestion, true
	case 'p':
		return queue.KindPermission, true
	case 'g':
		return queue.KindGoalInput, true
	case 'd':
		return queue.KindDecision, true
	}
	return "", false
}

// sourceVisible reports whether g may see something raised by session src in
// workgroup wg. The admin console's never; otherwise the source's row decides
// when it is in the inventory, and the recorded workgroup when it is gone.
func sourceVisible(g Grant, src, wg string, byID map[string]core.Session) bool {
	if src == ConsoleID {
		return false
	}
	if row, ok := byID[src]; ok {
		return inScope(g, row, byID)
	}
	switch g.Resource.Kind {
	case ResourceAllSessions:
		return true
	case ResourceWorkgroups:
		return wg != "" && slices.Contains(g.Resource.Workgroups, wg)
	}
	return false
}

func projectEntry(e queue.Entry) QueueEntry {
	out := QueueEntry{
		ID: e.ID, Kind: e.Kind, State: EntryStateOpen, SessionID: e.Session, SessionTitle: e.SessionTitle,
		Workgroup: e.Workgroup, Title: e.Title, Body: e.Body, CreatedAt: e.Created,
		Tool: e.Tool, Action: e.Action, RequestID: e.RequestID, RuntimeGeneration: e.RuntimeGeneration,
		ItemID: e.Item, SuggestedAction: e.Suggest, PRs: e.PRs, Links: e.Links, Form: e.Form,
	}
	for _, q := range e.Questions {
		qq := QueueQuestion{Header: q.Header, Question: q.Question, MultiSelect: q.MultiSelect, Options: []QueueOption{}}
		for _, o := range q.Options {
			qq.Options = append(qq.Options, QueueOption{Label: o.Label, Description: o.Description})
		}
		out.Questions = append(out.Questions, qq)
	}
	return out
}

func projectItem(it items.Item) InboxItem {
	return InboxItem{
		ID: it.ID, Kind: it.Kind, Title: it.Title, Detail: it.Detail, PRs: it.PRs, Links: it.Links,
		SuggestedAction: it.Suggest, State: it.State, Source: it.Source, Workgroup: it.Workgroup,
		Raised: it.Raised, Created: it.Created, Updated: it.Updated,
	}
}

// queueView is one grant's projection of the fresh queue, items and rows.
type queueView struct {
	entries []QueueEntry
	raw     []queue.Entry // the visible entries, unprojected (for the summary)
	items   []InboxItem
	rows    []core.Session
	byID    map[string]core.Session
}

func (s *Server) queueBackend() (QueueBackend, bool) {
	qb, ok := s.cfg.Backend.(QueueBackend)
	return qb, ok
}

// readQueue reads and projects the queue for l's grant.
func (s *Server) readQueue(ctx context.Context, l *Lease) (queueView, error) {
	qb, ok := s.queueBackend()
	if !ok {
		return queueView{}, hostbackend.ErrQueueUnsupported
	}
	rows, err := s.cfg.Backend.Snapshot(ctx)
	if err != nil {
		return queueView{}, err
	}
	ov, err := qb.Queue(ctx)
	if err != nil {
		return queueView{}, err
	}
	inbox, err := qb.InboxItems(ctx)
	if err != nil {
		return queueView{}, err
	}
	v := queueView{entries: []QueueEntry{}, items: []InboxItem{}, rows: rows, byID: indexRows(rows)}
	for _, e := range ov.Entries {
		if e.Dismissed || !sourceVisible(l.Grant, e.Session, e.Workgroup, v.byID) {
			continue
		}
		v.raw = append(v.raw, e)
		v.entries = append(v.entries, projectEntry(e))
	}
	for _, it := range inbox {
		if it.Recipient != items.GlobalInbox() || !items.Active(it.State) || !sourceVisible(l.Grant, it.Source, it.Workgroup, v.byID) {
			continue
		}
		v.items = append(v.items, projectItem(it))
	}
	return v, nil
}

// summarize buckets the grant's in-scope active workgroups.
func summarize(g Grant, v queueView) Summary {
	var rows []core.Session
	for _, r := range v.rows {
		root := rootOf(r, v.byID)
		if isConsole(r) || isOverseer(r) || root == "" {
			continue
		}
		if g.Resource.Kind == ResourceWorkgroups && !slices.Contains(g.Resource.Workgroups, root) {
			continue
		}
		rows = append(rows, r)
	}
	qs := queue.Summarize(rows, v.raw)
	out := Summary{Workgroups: []SummaryWorkgroup{}, Buckets: SummaryBuckets{NeedsYou: qs.NeedsYou, Running: qs.Running, WaitingReview: qs.Review, Done: qs.Done}}
	for _, w := range qs.Workgroups {
		b := w.Bucket
		if b == queue.BucketReview {
			b = BucketWaitingReview
		}
		out.Workgroups = append(out.Workgroups, SummaryWorkgroup{ID: w.ID, Title: w.Title, Bucket: b, State: w.State, Goal: w.Goal,
			NeedsYou: w.NeedsYou, OpenPRs: w.OpenPRs, Members: w.Members, Queue: w.Queue})
	}
	return out
}

// writeQueueErr reports a queue read failure.
func (s *Server) writeQueueErr(w http.ResponseWriter, l *Lease, err error) {
	if errors.Is(err, hostbackend.ErrQueueUnsupported) {
		writeErr(w, http.StatusServiceUnavailable, CodeDaemonUpgradeRequired, "the amux daemon predates the question queue; upgrade and restart amux")
		return
	}
	s.writeBackendErr(w, l, err)
}

func noQuery(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.RawQuery != "" {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "this route takes no query parameters")
		return false
	}
	return true
}

// queueRoute is GET /direct/v1/queue (read).
func (s *Server) queueRoute(w http.ResponseWriter, r *http.Request) {
	if !noQuery(w, r) {
		return
	}
	l := s.admit(w, r, PermRead)
	if l == nil {
		return
	}
	defer l.Done()
	v, err := s.readQueue(l.Context(), l)
	if err != nil {
		s.writeQueueErr(w, l, err)
		return
	}
	if code := l.AuthEnded(); code != "" {
		writeErr(w, http.StatusUnauthorized, code, "access ended")
		return
	}
	writeJSON(w, http.StatusOK, QueueResponse{ServerID: s.cfg.Authority.ServerID(), Entries: v.entries, Items: v.items})
}

// summaryRoute is GET /direct/v1/summary (read).
func (s *Server) summaryRoute(w http.ResponseWriter, r *http.Request) {
	if !noQuery(w, r) {
		return
	}
	l := s.admit(w, r, PermRead)
	if l == nil {
		return
	}
	defer l.Done()
	v, err := s.readQueue(l.Context(), l)
	if err != nil {
		s.writeQueueErr(w, l, err)
		return
	}
	if code := l.AuthEnded(); code != "" {
		writeErr(w, http.StatusUnauthorized, code, "access ended")
		return
	}
	writeJSON(w, http.StatusOK, SummaryResponse{ServerID: s.cfg.Authority.ServerID(), Summary: summarize(l.Grant, v)})
}

// queueWatch is GET /direct/v1/queue/watch: the sessions-watch handshake (an
// auth frame first, then welcome), then a QueueWatchSnapshot on connect and
// on every change, polled like the inventory.
func (s *Server) queueWatch(w http.ResponseWriter, r *http.Request) {
	s.serveWatch(w, r, func(ctx context.Context, l *Lease) (any, error) {
		v, err := s.readQueue(ctx, l)
		if err != nil {
			return nil, err
		}
		return QueueWatchSnapshot{Type: "snapshot", Entries: v.entries, Items: v.items, Summary: summarize(l.Grant, v)}, nil
	})
}

// answerHash binds an idempotency key to one answer.
func answerHash(route, id string, v any) string {
	canon, _ := json.Marshal(struct {
		Route string `json:"route"`
		ID    string `json:"id"`
		Body  any    `json:"body"`
	}{route, id, v})
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:])
}

// readStrict decodes a JSON body strictly and checks its idem_key.
func readStrict(w http.ResponseWriter, r *http.Request, body []byte, v any, key func() string) bool {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "content type must be application/json")
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil || dec.More() {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "invalid request body")
		return false
	}
	k := key()
	if k == "" || len(k) > MaxIdemKeyLen || strings.ContainsAny(k, "\x00\r\n") {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "idem_key is required (1-128 bytes)")
		return false
	}
	return true
}

// answerFields validates an answer against its kind and maps it to the
// daemon's ActionQueueAnswer fields. e is the live entry when known.
func answerFields(kind string, req QueueAnswerRequest, e *queue.Entry) (map[string]string, error) {
	bad := errors.New("invalid answer fields for " + kind)
	text := strings.TrimSpace(req.Answer)
	if len(req.Answer) > MaxPromptBytes || len(req.RuntimeGeneration) > MaxFieldBytes || len(req.Choice) > 32 {
		return nil, bad
	}
	f := map[string]string{}
	switch kind {
	case queue.KindPermission:
		if text != "" || len(req.Choice) > 0 || (req.Decision != core.SteerAllow && req.Decision != core.SteerDeny) || req.RuntimeGeneration == "" {
			return nil, bad
		}
		if e != nil && req.RuntimeGeneration != e.RuntimeGeneration {
			return nil, errAskerGone
		}
		f[core.SteerDecision] = req.Decision
	case queue.KindQuestion:
		if req.Form != nil {
			if req.Decision != "" || req.Answer != "" || len(req.Choice) > 0 {
				return nil, bad
			}
			if e != nil {
				if e.Form == nil {
					return nil, bad
				}
				if err := hp.ValidateAnswer(*e.Form, *req.Form); err != nil {
					return nil, fmt.Errorf("%w: %v", bad, err)
				}
			}
			b, err := json.Marshal(req.Form)
			if err != nil {
				return nil, bad
			}
			f[core.QueueFormField] = string(b)
			break
		}
		if req.Decision != "" || (text == "") == (len(req.Choice) == 0) {
			return nil, bad
		}
		if len(req.Choice) > 0 {
			if e != nil && (len(e.Questions) != 1 || len(req.Choice) > 1 && !e.Questions[0].MultiSelect) {
				return nil, bad
			}
			for _, c := range req.Choice {
				if c == "" || len(c) > MaxFieldBytes || strings.ContainsAny(c, "|\n\r") {
					return nil, bad
				}
			}
			f[core.QueueChoiceField] = strings.Join(req.Choice, "|")
		} else {
			f[core.QueueTextField] = req.Answer
		}
	case queue.KindGoalInput, queue.KindDecision:
		if text == "" || len(req.Choice) > 0 || req.Decision != "" || req.RuntimeGeneration != "" {
			return nil, bad
		}
		f[core.QueueTextField] = req.Answer
	default:
		return nil, bad
	}
	return f, nil
}

var (
	errAskerGone        = errors.New(CodeAskerGone)
	errOverseerDenied   = errors.New("overseer steer denied")
	errAnswered         = errors.New(CodeAnswered)
	errNeedsNewerClient = errors.New(CodeNeedsNewerClient)
)

// answerRoute is POST /direct/v1/queue/{entry_id}/answer. Order: credential
// and the kind's permission (from the id's prefix; no backend call), strict
// body and idem key, the kind's field rules, a fresh read (the entry must be
// in scope), the audit record, then the daemon's own re-checked answer.
func (s *Server) answerRoute(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	kind, ok := entryKind(id)
	if !ok {
		writeErr(w, http.StatusNotFound, CodeNotFound, "no such queue entry")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxJSONBody))
	if err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "body too large")
		return
	}
	perm := answerPerm(kind)
	l := s.admit(w, r, perm)
	if l == nil {
		return
	}
	defer l.Done()
	var req QueueAnswerRequest
	if !readStrict(w, r, body, &req, func() string { return req.IdemKey }) {
		return
	}
	if _, err := answerFields(kind, req, nil); err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, err.Error())
		return
	}
	text := req.Answer
	if len(req.Choice) > 0 {
		text = strings.Join(req.Choice, ", ")
	} else if req.Decision != "" {
		text = req.Decision
	} else if req.Form != nil {
		text = formAuditText(*req.Form)
	}
	var act core.Action
	s.runQueueOp(w, l, req.IdemKey, answerHash("answer", id, req), auditRecord{Route: "queue.answer", Verb: kind, Target: id, Text: text},
		func(l *Lease) error {
			v, err := s.readQueue(l.Context(), l)
			if err != nil {
				return err
			}
			var live *queue.Entry
			for i := range v.raw {
				if v.raw[i].ID == id {
					live = &v.raw[i]
				}
			}
			if live == nil {
				// Not open for this grant: out of scope, or gone. The daemon
				// tells gone from recently answered; scope cannot leak through
				// an entry that no longer exists.
				if sourceHidden(l, s, id) {
					return errNotFound
				}
			}
			fields, err := answerFields(kind, req, live)
			if err != nil {
				return err
			}
			if live != nil && live.Form != nil && req.Form == nil && !hp.LegacyAnswerable(*live.Form) {
				return errNeedsNewerClient
			}
			fields[core.QueueEntryField] = id
			fields[core.QueueByField] = "direct:" + l.ClientID
			act = core.Action{Action: core.ActionQueueAnswer, Fields: fields}
			return nil
		},
		func(ctx context.Context) (any, error) {
			_, err := s.cfg.Backend.Apply(ctx, act)
			return ActionResponse{OK: true}, err
		})
}

// sourceHidden reports an entry id that is open on the machine but outside l's
// scope (so it answers 404, never asker_gone).
func sourceHidden(l *Lease, s *Server, id string) bool {
	qb, ok := s.queueBackend()
	if !ok {
		return false
	}
	ov, err := qb.Queue(l.Context())
	if err != nil {
		return false
	}
	for _, e := range ov.Entries {
		if e.ID == id {
			return true // open, yet not in this grant's view
		}
	}
	return false
}

// triageVerb canonicalizes a triage state; "acknowledge" is accepted for ack.
func triageVerb(s string) (verb, state string, ok bool) {
	switch s {
	case TriageAck, "acknowledge":
		return TriageAck, items.StateAcknowledged, true
	case TriageDismiss:
		return TriageDismiss, items.StateDismissed, true
	case TriageDone:
		return TriageDone, items.StateDone, true
	case TriageReopen:
		return TriageReopen, items.StateOpen, true
	}
	return "", "", false
}

// itemStateRoute is POST /direct/v1/items/{item_id}/state: triage one
// global-inbox item (steer; ack also needs create on an all_sessions grant,
// because a user acknowledgment authorizes the overseer to create a workgroup
// from the item).
func (s *Server) itemStateRoute(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !items.ValidID(id) {
		writeErr(w, http.StatusNotFound, CodeNotFound, "no such item")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxJSONBody))
	if err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "body too large")
		return
	}
	l := s.admit(w, r, PermSteer)
	if l == nil {
		return
	}
	defer l.Done()
	var req ItemStateRequest
	if !readStrict(w, r, body, &req, func() string { return req.IdemKey }) {
		return
	}
	verb, state, ok := triageVerb(req.State)
	if !ok {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "state must be ack, dismiss, done or reopen")
		return
	}
	if verb == TriageAck && !mayAcknowledge(l.Grant) {
		writeDenied(w, PermCreate, "acknowledging an item authorizes the overseer to create a workgroup from it; that needs an all-sessions client with create")
		return
	}
	req.State = verb
	var act core.Action
	s.runQueueOp(w, l, req.IdemKey, answerHash("item.state", id, req), auditRecord{Route: "items.state", Verb: verb, Target: id},
		func(l *Lease) error {
			qb, ok := s.queueBackend()
			if !ok {
				return hostbackend.ErrQueueUnsupported
			}
			rows, err := s.cfg.Backend.Snapshot(l.Context())
			if err != nil {
				return err
			}
			it, found, err := qb.Item(l.Context(), id)
			if err != nil {
				return err
			}
			if !found || it.Recipient != items.GlobalInbox() || !sourceVisible(l.Grant, it.Source, it.Workgroup, indexRows(rows)) {
				return errNotFound
			}
			act = core.Action{Action: core.ActionItemState, ID: items.GlobalInbox(), Fields: map[string]string{
				core.ItemIDField: id, core.ItemStateField: verb,
				core.ItemNoteField: "via direct client " + s.cfg.Authority.ClientLabel(l.ClientID),
			}}
			return nil
		},
		func(ctx context.Context) (any, error) {
			_, err := s.cfg.Backend.Apply(ctx, act)
			return ItemStateResponse{OK: true, State: state}, err
		})
}

// runQueueOp is the idempotent, audited execution path for a queue answer or
// an item triage: peek the key, run the fresh precheck, write the audit
// record (refusing if it cannot be written: nothing runs unaudited), record
// the key, run the daemon verb under the bounded execution context, settle.
func (s *Server) runQueueOp(w http.ResponseWriter, l *Lease, key, bodyHash string, rec auditRecord, precheck func(*Lease) error, apply func(context.Context) (any, error)) {
	if out, idem, err := s.cfg.Authority.BeginAction(l, key, bodyHash, true); err != nil {
		writeAuthErr(w, err)
		return
	} else if s.idemReply(w, l, out, idem) {
		return
	}
	if err := precheck(l); err != nil {
		s.writeQueuePrecheckErr(w, l, err)
		return
	}
	rec.ClientID, rec.Label = l.ClientID, s.cfg.Authority.ClientLabel(l.ClientID)
	if err := s.cfg.Authority.Audit(rec.with("admitted")); err != nil {
		s.cfg.Logf("direct: audit: %v", err)
		writeErr(w, http.StatusServiceUnavailable, CodeUnavailable, "the audit log could not be written; nothing ran")
		return
	}
	if code := l.AuthEnded(); code != "" {
		writeErr(w, http.StatusUnauthorized, code, "access ended before the action ran")
		return
	}
	out, idem, err := s.cfg.Authority.BeginAction(l, key, bodyHash, false)
	if s.writeBeginErr(w, err) {
		return
	}
	if s.idemReply(w, l, out, idem) {
		return
	}
	exec, cancel := l.ExecContext(actionExecLimit)
	res, err := apply(exec)
	cancel()
	var rejected *daemon.RejectedError
	status, outcome := http.StatusOK, "ok"
	var resp any = res
	switch {
	case err == nil:
		s.cfg.Authority.FinishResult(l, key, ActionResponse{OK: true})
	case errors.As(err, &rejected) && queueConflict(rejected.Code) != "":
		msg := queueConflict(rejected.Code)
		s.cfg.Authority.FinishAction(l, key, false, rejected.Code, msg)
		status, outcome, resp = http.StatusConflict, rejected.Code, APIError{Code: rejected.Code, Error: msg}
	case errors.As(err, &rejected):
		s.cfg.Logf("direct: %s %s rejected: %v", rec.Route, rec.Target, err)
		s.cfg.Authority.FinishAction(l, key, false, CodeRejected, "the daemon rejected the request")
		status, outcome, resp = http.StatusUnprocessableEntity, CodeRejected, APIError{Code: CodeRejected, Error: "the daemon rejected the request"}
	case errors.Is(err, hostbackend.ErrUnavailable):
		s.cfg.Authority.FinishAction(l, key, false, CodeUnavailable, "the amux daemon was not reachable; nothing ran")
		status, outcome, resp = http.StatusServiceUnavailable, CodeUnavailable, APIError{Code: CodeUnavailable, Error: "the amux daemon was not reachable; nothing ran"}
	default:
		s.cfg.Authority.AmbiguousAction(l, key)
		status, outcome, resp = http.StatusConflict, CodeIdemAmbiguous, APIError{Code: CodeIdemAmbiguous, Error: "the outcome is unknown; it will not run again with this idem_key"}
	}
	if err := s.cfg.Authority.Audit(rec.with(outcome)); err != nil {
		s.cfg.Logf("direct: audit: %v", err)
	}
	writeJSON(w, status, resp)
}

func (s *Server) writeQueuePrecheckErr(w http.ResponseWriter, l *Lease, err error) {
	switch {
	case errors.Is(err, errNeedsNewerClient):
		writeErr(w, http.StatusConflict, CodeNeedsNewerClient, queueConflict(CodeNeedsNewerClient))
	case errors.Is(err, errAskerGone):
		writeErr(w, http.StatusConflict, CodeAskerGone, "the question is no longer open (its runtime generation changed)")
	case errors.Is(err, errNotFound):
		writeErr(w, http.StatusNotFound, CodeNotFound, "no such queue entry or item")
	case errors.Is(err, hostbackend.ErrQueueUnsupported):
		writeErr(w, http.StatusServiceUnavailable, CodeDaemonUpgradeRequired, "the amux daemon predates the question queue; upgrade and restart amux")
	case strings.HasPrefix(err.Error(), "invalid answer fields"):
		writeErr(w, http.StatusBadRequest, CodeBadRequest, err.Error())
	default:
		s.writeBackendErr(w, l, err)
	}
}

// ── watch plumbing shared by /sessions/watch and /queue/watch ─────────────

// serveWatch runs the watch handshake and pushes frame() whenever its JSON
// changes, polling at WatchPoll.
func (s *Server) serveWatch(w http.ResponseWriter, r *http.Request, frame func(context.Context, *Lease) (any, error)) {
	up := websocket.Upgrader{
		HandshakeTimeout: 10 * time.Second,
		CheckOrigin:      func(r *http.Request) bool { o := r.Header.Get("Origin"); return o == "" || s.origins[o] },
	}
	conn, err := up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetReadLimit(MaxWatchAuthSize)
	closeWith := func(code int, reason string) {
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(time.Second))
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var auth WatchAuth
	if err := conn.ReadJSON(&auth); err != nil || auth.Type != "auth" {
		closeWith(CloseAuthInvalid, CodeAuthInvalid)
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	l, err := s.cfg.Authority.Admit(r.Context(), auth.AccessToken, PermRead)
	if err != nil {
		switch {
		case errors.Is(err, errAuthExpired):
			closeWith(CloseAuthExpired, CodeAuthExpired)
		case errors.Is(err, errAuthRevoked):
			closeWith(CloseAuthRevoked, CodeAuthRevoked)
		case errors.Is(err, errDenied):
			closeWith(CloseDenied, CodeDenied)
		default:
			closeWith(CloseAuthInvalid, CodeAuthInvalid)
		}
		return
	}
	defer l.Done()
	ctx, cancel := context.WithCancel(l.Context())
	defer cancel()
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				cancel()
				return
			}
		}
	}()
	var wmu sync.Mutex
	write := func(v any) bool {
		wmu.Lock()
		defer wmu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		return conn.WriteJSON(v) == nil
	}
	if !write(WatchWelcome{Type: "welcome", ServerID: s.cfg.Authority.ServerID(), ExpiresAt: l.ExpiresAt}) {
		return
	}
	poll := time.NewTicker(s.cfg.WatchPoll)
	defer poll.Stop()
	var last []byte
	push := func() bool {
		v, err := frame(ctx, l)
		if err != nil || ctx.Err() != nil {
			return true // retry next tick; the loop reports a lease end
		}
		data, _ := json.Marshal(v)
		if bytes.Equal(data, last) {
			return true
		}
		last = data
		return write(v)
	}
	if !push() {
		return
	}
	for {
		select {
		case <-ctx.Done():
			switch l.AuthEnded() {
			case CodeAuthRevoked:
				closeWith(CloseAuthRevoked, CodeAuthRevoked)
			case CodeAuthExpired:
				closeWith(CloseAuthExpired, CodeAuthExpired)
			}
			return
		case <-poll.C:
			if !push() {
				return
			}
		}
	}
}

// includeOverseer parses GET /sessions' optional ?include=overseer. Any other
// parameter is ignored, as before (older clients send none).
func includeOverseer(r *http.Request) bool {
	for _, v := range r.URL.Query()["include"] {
		for _, part := range strings.Split(v, ",") {
			if strings.TrimSpace(part) == IncludeOverseer {
				return true
			}
		}
	}
	return false
}

// formAuditText records a form answer in the audit log without its free text
// (which may be a secret): the selected option indices and whether free text
// or a note was given.
func formAuditText(a hp.FormAnswer) string {
	if a.Action == hp.FormActionCancel {
		if a.Message != "" {
			return "form: dismissed with a reply"
		}
		return "form: dismissed"
	}
	parts := make([]string, 0, len(a.Answers))
	for _, qa := range a.Answers {
		p := qa.ID + "=" + strings.Trim(strings.Join(strings.Fields(fmt.Sprint(qa.Selected)), ","), "[]")
		if qa.Other != nil {
			p += "+other"
		}
		if qa.Note != "" {
			p += "+note"
		}
		parts = append(parts, p)
	}
	return "form: " + strings.Join(parts, " ")
}
