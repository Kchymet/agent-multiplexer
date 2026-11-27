package codexapp

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// userinput.go serves the App Server's structured request-user-input
// (docs/question-prompts.md §4.3): the questions become a question form the
// daemon lists in the user question queue, and an answer from any amux surface
// goes back as the JSON-RPC response on the request's own id. The native TUI is
// a peer client of the same thread, so either side may answer: our response is
// only an attempt, and the server's serverRequest/resolved (or the turn ending)
// clears the request — the same model as approvals, on a tracker of its own so
// a permission verb can never target a question and vice versa.

// Both spellings of the method are accepted: amux's earlier handler and the
// harness pilot used "tool/requestUserInput", while the App Server v2 schema
// names it under item/ ("item/tool/requestUserInput", as amux's own fake App
// Server sends). Accepting both keeps the question answerable whichever a
// given Codex release sends; neither falls through to -32601.
const (
	methodUserInput       = "item/tool/requestUserInput"
	methodUserInputLegacy = "tool/requestUserInput"
)

func isUserInputMethod(method string) bool {
	return method == methodUserInput || method == methodUserInputLegacy
}

// userInputParams is ToolRequestUserInputParams.
type userInputParams struct {
	ThreadID  string `json:"threadId"`
	TurnID    string `json:"turnId"`
	ItemID    string `json:"itemId"`
	Questions []struct {
		ID       string `json:"id"`
		Header   string `json:"header"`
		Question string `json:"question"`
		IsOther  bool   `json:"isOther"`
		IsSecret bool   `json:"isSecret"`
		Options  []struct {
			Label       string `json:"label"`
			Description string `json:"description"`
		} `json:"options"`
	} `json:"questions"`
}

// UserInput is one open request-user-input, as the daemon lists it.
type UserInput struct {
	// RequestID is the JSON-RPC id's string key (stable for the request).
	RequestID string
	TurnID    string
	Form      harnessproto.Form
}

// userInputForm builds the request's form. A question without an id is keyed
// by its position ("q<i>"), the key its answer then uses. Codex has no
// multi-select, notes or reply; a question with no options takes free text.
func userInputForm(p userInputParams) harnessproto.Form {
	f := harnessproto.Form{Version: harnessproto.FormVersion, Source: harnessproto.FormSourceCodexUserInput,
		Cancel: true, Delivery: harnessproto.DeliveryStructured}
	for i, q := range p.Questions {
		id := strings.TrimSpace(q.ID)
		if id == "" {
			id = fmt.Sprintf("q%d", i)
		}
		fq := harnessproto.FormQuestion{ID: id, Header: q.Header, Question: q.Question,
			Other: q.IsOther || len(q.Options) == 0, Secret: q.IsSecret}
		for _, o := range q.Options {
			fq.Options = append(fq.Options, harnessproto.FormOption{Label: o.Label, Description: o.Description})
		}
		f.Questions = append(f.Questions, fq)
	}
	return f
}

// handleUserInput registers a request-user-input on our pinned thread and
// surfaces it (the queue reads OpenUserInputs; the notice and raw event keep
// the transcript whole). It never auto-answers: the native TUI attached to the
// same server may be the one to answer, and an empty answer would preempt it.
func (s *Supervisor) handleUserInput(id json.RawMessage, method string, params json.RawMessage) {
	var p userInputParams
	_ = json.Unmarshal(params, &p)
	qs := make([]string, 0, len(p.Questions))
	for _, q := range p.Questions {
		qs = append(qs, q.Question)
	}
	s.mu.Lock()
	pinned := s.threadID
	s.mu.Unlock()
	// The same ownership guard as approvals: only our pinned thread's request
	// may become answerable here.
	if p.ThreadID != "" && pinned != "" && p.ThreadID == pinned && len(p.Questions) > 0 {
		form := userInputForm(p)
		if !s.inputs.register(&pendingApproval{
			rawID: append(json.RawMessage(nil), id...), key: idKey(id), method: method,
			threadID: p.ThreadID, turnID: p.TurnID, itemID: p.ItemID, form: &form,
		}) {
			return // a replayed request: surfaced already
		}
	}
	s.emit(notice("info", "request-user-input awaiting a client: "+strings.Join(qs, " | ")))
	s.emit(rawEvent(method, params))
}

// OpenUserInputs lists the request-user-input requests no client has answered
// yet, oldest id first.
func (s *Supervisor) OpenUserInputs() []UserInput {
	s.inputs.mu.Lock()
	defer s.inputs.mu.Unlock()
	out := make([]UserInput, 0, len(s.inputs.live))
	for k, p := range s.inputs.live {
		if p.state == apPending && p.form != nil {
			out = append(out, UserInput{RequestID: k, TurnID: p.turnID, Form: *p.form})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RequestID < out[j].RequestID })
	return out
}

// AnswerUserInput answers an open request-user-input with a form answer: each
// question's selected labels plus its free text, keyed by question id; a
// dismissal answers every question with no answers (the App Server's own "no
// answer" shape). Stale and duplicate answers are refused, as for approvals.
func (s *Supervisor) AnswerUserInput(requestID string, a harnessproto.FormAnswer) error {
	s.inputs.mu.Lock()
	p, ok := s.inputs.live[requestID]
	var form harnessproto.Form
	if ok && p.form != nil {
		form = *p.form
	}
	s.inputs.mu.Unlock()
	if !ok {
		if s.inputs.isResolved(requestID) {
			return errDuplicateApproval
		}
		return errStaleApproval
	}
	if err := harnessproto.ValidateAnswer(form, a); err != nil {
		return err
	}
	answers := userInputAnswers(form, a)
	p, err := s.inputs.answerLocally(requestID)
	if err != nil {
		return err
	}
	return s.rpc.respond(p.rawID, map[string]any{"answers": answers})
}

// userInputAnswers renders a form answer as ToolRequestUserInputResponse's
// answers map: {<question id>: {"answers": [labels…, free text]}}.
func userInputAnswers(f harnessproto.Form, a harnessproto.FormAnswer) map[string]map[string][]string {
	out := make(map[string]map[string][]string, len(f.Questions))
	for _, q := range f.Questions {
		out[q.ID] = map[string][]string{"answers": {}}
	}
	if a.Action != harnessproto.FormActionAnswer {
		return out
	}
	for _, qa := range a.Answers {
		for _, q := range f.Questions {
			if q.ID != qa.ID {
				continue
			}
			list := []string{}
			for _, i := range qa.Selected {
				list = append(list, q.Options[i].Label)
			}
			if qa.Other != nil {
				list = append(list, strings.TrimSpace(*qa.Other))
			}
			out[q.ID]["answers"] = list
		}
	}
	return out
}

// isResolved reports whether requestID was resolved (answered and confirmed,
// or cleared).
func (a *approvalTracker) isResolved(requestID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.resolved[requestID]
}

// IsStale reports an answer to a request this supervisor never saw (or no
// longer tracks); IsDuplicate one already answered or resolved.
func IsStale(err error) bool     { return errors.Is(err, errStaleApproval) }
func IsDuplicate(err error) bool { return errors.Is(err, errDuplicateApproval) }
