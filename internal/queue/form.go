package queue

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// Question prompts v2 (docs/question-prompts.md): the queue's side of the
// harnessproto Form — building a form from a runtime's question, projecting it
// onto the legacy entry fields an old client reads, and mapping a legacy answer
// (option labels, one free text) onto a FormAnswer without changing its
// meaning.

// Legacy answer errors.
var (
	// ErrNeedsNewerClient: the form cannot be answered with legacy fields.
	ErrNeedsNewerClient = errors.New(harnessproto.CodeNeedsNewerClient)
	// ErrLegacyShape: the legacy answer does not map onto the form without
	// guessing (mixed labels and text, an unanswered question).
	ErrLegacyShape = errors.New("legacy answer shape")
)

// LegacyOmittedTitle prefixes the title of an entry whose questions are kept
// from legacy clients.
const LegacyOmittedTitle = "[open in a newer amux client] "

// legacyPreviewNote is appended to a legacy entry's body when its options have
// previews a legacy client cannot show.
const legacyPreviewNote = "Option previews are hidden here; open the question in a newer amux client to compare them."

// claudeAskInput is AskUserQuestion's input, previews included.
type claudeAskInput struct {
	Questions []struct {
		Header      string `json:"header"`
		Question    string `json:"question"`
		MultiSelect bool   `json:"multiSelect"`
		Options     []struct {
			Label       string `json:"label"`
			Description string `json:"description"`
			Preview     string `json:"preview"`
		} `json:"options"`
	} `json:"questions"`
}

// ClaudeForm builds the form for an AskUserQuestion call's input. Question ids
// are positional ("q0"…); Claude always offers "Other", takes a note per
// question (annotations) and can be dismissed or answered with a reply. The
// caller sets Delivery. ok is false for input that is not that shape.
func ClaudeForm(raw json.RawMessage) (harnessproto.Form, bool) {
	var in claudeAskInput
	if len(raw) == 0 || json.Unmarshal(raw, &in) != nil {
		return harnessproto.Form{}, false
	}
	f := harnessproto.Form{Version: harnessproto.FormVersion, Source: harnessproto.FormSourceClaudeAsk, Cancel: true, Reply: true}
	for _, q := range in.Questions {
		if strings.TrimSpace(q.Question) == "" {
			continue
		}
		fq := harnessproto.FormQuestion{
			ID: fmt.Sprintf("q%d", len(f.Questions)), Header: q.Header, Question: q.Question,
			MultiSelect: q.MultiSelect, Other: true, Notes: true,
		}
		for _, o := range q.Options {
			opt := harnessproto.FormOption{Label: o.Label, Description: o.Description}
			if !q.MultiSelect {
				opt.Preview = o.Preview // previews are single-select only
			}
			fq.Options = append(fq.Options, opt)
		}
		f.Questions = append(f.Questions, fq)
		if len(f.Questions) == harnessproto.MaxFormQuestions {
			break
		}
	}
	return f, len(f.Questions) > 0
}

// HasPreview reports whether a question shows option previews.
func HasPreview(q harnessproto.FormQuestion) bool {
	for _, o := range q.Options {
		if o.Preview != "" {
			return true
		}
	}
	return false
}

// LegacyQuestions is the legacy view of a form's questions (no previews).
func LegacyQuestions(f harnessproto.Form) []Question {
	out := make([]Question, 0, len(f.Questions))
	for _, q := range f.Questions {
		lq := Question{Header: q.Header, Question: q.Question, MultiSelect: q.MultiSelect}
		for _, o := range q.Options {
			lq.Options = append(lq.Options, Option{Label: o.Label, Description: o.Description})
		}
		out = append(out, lq)
	}
	return out
}

// ProjectLegacy fills an entry's legacy fields from its form (§2.5): the
// questions an old client can answer correctly, a note when previews are
// hidden from it, and nothing — with a title saying so — when it cannot.
func ProjectLegacy(e *Entry) {
	if e.Form == nil {
		return
	}
	if !harnessproto.LegacyAnswerable(*e.Form) {
		e.Questions = nil
		if !strings.HasPrefix(e.Title, LegacyOmittedTitle) {
			e.Title = LegacyOmittedTitle + e.Title
		}
		return
	}
	e.Questions = LegacyQuestions(*e.Form)
	for _, q := range e.Form.Questions {
		if HasPreview(q) {
			if e.Body != "" {
				e.Body += "\n\n"
			}
			e.Body += legacyPreviewNote
			break
		}
	}
}

// LegacyAnswer maps a legacy answer — option labels per question (choices) or
// one free text — onto a FormAnswer with the same meaning (§3.5):
//
//   - labels for every question: those selections;
//   - text for a one-question form: that question's "Other" text;
//   - text alone for a several-question form: a dismissal with the text as the
//     user's reply (what the legacy typed path always did);
//   - anything else (labels and text mixed, a question left without labels):
//     ErrLegacyShape rather than a guess.
func LegacyAnswer(f harnessproto.Form, choices [][]string, text string) (harnessproto.FormAnswer, error) {
	if !harnessproto.LegacyAnswerable(f) {
		return harnessproto.FormAnswer{}, ErrNeedsNewerClient
	}
	text = strings.TrimSpace(text)
	a := harnessproto.FormAnswer{Version: harnessproto.FormVersion, Action: harnessproto.FormActionAnswer}
	hasChoice := false
	for _, c := range choices {
		if len(c) > 0 {
			hasChoice = true
		}
	}
	switch {
	case hasChoice && text != "":
		return a, fmt.Errorf("%w: answer with labels or with text, not both", ErrLegacyShape)
	case hasChoice:
		if len(choices) != len(f.Questions) {
			return a, fmt.Errorf("%w: give labels for every question (%d)", ErrLegacyShape, len(f.Questions))
		}
		for i, q := range f.Questions {
			if len(choices[i]) == 0 {
				return a, fmt.Errorf("%w: question %d has no label", ErrLegacyShape, i+1)
			}
			qa := harnessproto.QuestionAnswer{ID: q.ID}
			for _, label := range choices[i] {
				idx := optionIndex(q, label)
				if idx < 0 {
					return a, fmt.Errorf("%w: %q is not an option of question %d", ErrLegacyShape, label, i+1)
				}
				qa.Selected = append(qa.Selected, idx)
			}
			a.Answers = append(a.Answers, qa)
		}
	case text != "" && len(f.Questions) == 1:
		q := f.Questions[0]
		if !q.Other {
			return a, fmt.Errorf("%w: pick one of the options", ErrLegacyShape)
		}
		a.Answers = []harnessproto.QuestionAnswer{{ID: q.ID, Other: &text}}
	case text != "":
		if !f.Reply {
			return a, fmt.Errorf("%w: answer every question with a label", ErrLegacyShape)
		}
		a = harnessproto.FormAnswer{Version: harnessproto.FormVersion, Action: harnessproto.FormActionCancel, Message: text}
	default:
		return a, fmt.Errorf("%w: nothing to answer with", ErrLegacyShape)
	}
	return a, harnessproto.ValidateAnswer(f, a)
}

func optionIndex(q harnessproto.FormQuestion, label string) int {
	for i, o := range q.Options {
		if o.Label == label {
			return i
		}
	}
	return -1
}

// MatchReply recognizes a typed reply that is exactly the legacy rendering of
// a valid answer (FormatAnswer, harness-android#73 questionReply): a lone
// question's chosen label(s) joined with ", ", or one "header: labels" line
// per question. Only an exact match counts; anything else is free text.
func MatchReply(f harnessproto.Form, text string) (harnessproto.FormAnswer, bool) {
	text = strings.TrimSpace(text)
	if text == "" || !harnessproto.LegacyAnswerable(f) {
		return harnessproto.FormAnswer{}, false
	}
	var choices [][]string
	if len(f.Questions) == 1 {
		labels, ok := matchLabels(f.Questions[0], text)
		if !ok {
			return harnessproto.FormAnswer{}, false
		}
		choices = [][]string{labels}
	} else {
		lines := strings.Split(text, "\n")
		if len(lines) != len(f.Questions) {
			return harnessproto.FormAnswer{}, false
		}
		for i, q := range f.Questions {
			name := strings.TrimSpace(q.Header)
			if name == "" {
				name = q.Question
			}
			rest, ok := strings.CutPrefix(strings.TrimSpace(lines[i]), name+": ")
			if !ok {
				return harnessproto.FormAnswer{}, false
			}
			labels, ok := matchLabels(q, rest)
			if !ok {
				return harnessproto.FormAnswer{}, false
			}
			choices = append(choices, labels)
		}
	}
	a, err := LegacyAnswer(f, choices, "")
	return a, err == nil
}

// matchLabels reads s as one label of q, or (multi-select) labels joined with
// ", ". A whole-label match wins, so a label that itself holds ", " still
// matches.
func matchLabels(q harnessproto.FormQuestion, s string) ([]string, bool) {
	s = strings.TrimSpace(s)
	if optionIndex(q, s) >= 0 {
		return []string{s}, true
	}
	if !q.MultiSelect {
		return nil, false
	}
	// Labels may themselves hold ", ": accept exactly one way of reading the
	// parts as labels (an ambiguous reading is free text, not a guess).
	parts := strings.Split(s, ", ")
	var found [][]string
	var walk func(i int, acc []string)
	walk = func(i int, acc []string) {
		if len(found) > 1 {
			return
		}
		if i == len(parts) {
			found = append(found, append([]string(nil), acc...))
			return
		}
		for j := i + 1; j <= len(parts); j++ {
			if l := strings.Join(parts[i:j], ", "); optionIndex(q, l) >= 0 {
				walk(j, append(acc, l))
			}
		}
	}
	walk(0, nil)
	if len(found) != 1 {
		return nil, false
	}
	return found[0], true
}
