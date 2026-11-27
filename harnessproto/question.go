package harnessproto

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Question prompts v2 (docs/question-prompts.md): an agent's structured
// question to the user — Claude Code's AskUserQuestion, Codex's
// request-user-input — carried as one envelope, the Form, so every amux
// surface can answer it with what the harness itself offers: 1–4 questions,
// header chips, options with descriptions, multi-select, "Other" free text,
// option previews, per-question notes, and cancel.
//
// Wire contract (question-prompts-v2): the envelope rides under a single word
// key, `form`, on a queue entry (amux queue JSON, direct QueueEntry) and in the
// question_request runtime event; every key nested under it is snake_case on
// every wire, so one decoder serves the camelCase queue JSON, the snake_case
// direct API and the cloud backend.
//
// Compatibility rules a consumer follows:
//   - FormVersion changes only for a breaking change; additive keys keep it.
//     A consumer that meets a version it does not know renders the legacy
//     fields of the entry (or "open in a newer client") and never answers the
//     form.
//   - Unknown keys are ignored; an unknown Delivery reads as DeliveryPane (do
//     not offer controls); an unknown Source is rendered generically.
//   - The feature QuestionPromptsFeature (cloud register capabilities) or the
//     direct capabilities' `queue.form` block says the daemon emits forms and
//     accepts FormAnswers; without it a consumer uses the legacy question path.

// QuestionPromptsFeature is the register.capabilities.features string a
// provider advertises when its daemon emits question forms and serves
// VerbAnswer.
const QuestionPromptsFeature = "question-prompts-v2"

// FormVersion is the question-form wire version.
const FormVersion = 2

// Form sources (Form.Source).
const (
	FormSourceClaudeAsk      = "claude_ask_user_question"
	FormSourceCodexUserInput = "codex_request_user_input"
	// FormSourceCodexElicit is reserved for MCP elicitation (not emitted yet).
	FormSourceCodexElicit = "codex_mcp_elicitation"
)

// Delivery modes (Form.Delivery): how amux delivers an answer right now.
const (
	// DeliveryStructured: through a structured channel — Claude's held
	// PreToolUse hook, or the Codex App Server's request/response.
	DeliveryStructured = "structured"
	// DeliveryKeys: by driving the dialog drawn in the pane, verifying every
	// step against the screen before submitting.
	DeliveryKeys = "keys"
	// DeliveryPane: amux cannot deliver this form; answer it in the agent's
	// pane. A consumer shows no answer controls.
	DeliveryPane = "pane"
)

// Answer actions (FormAnswer.Action).
const (
	FormActionAnswer = "answer"
	FormActionCancel = "cancel"
)

// Resolution outcomes (QuestionResolvedPayload.Outcome).
const (
	QuestionAnswered  = "answered"
	QuestionCancelled = "cancelled"
	QuestionTimedOut  = "timed_out"
	// QuestionCleared: the prompt is gone and how it went is unknown (the turn
	// ended, the runtime restarted, the session was archived).
	QuestionCleared = "cleared"
)

// Where a resolution came from (QuestionResolvedPayload.Via).
const (
	ViaAmux    = "amux"
	ViaPane    = "pane"
	ViaUnknown = "unknown"
)

// Bounds on a form and its answers.
const (
	MaxFormQuestions = 4
	MaxFormOptions   = 8 // Claude allows 4; Codex is not bounded by schema
	MaxNoteRunes     = 2000
	MaxOtherBytes    = 64 << 10
)

// Form is one structured question prompt.
type Form struct {
	Version   int            `json:"version"`
	Source    string         `json:"source"`
	Questions []FormQuestion `json:"questions"`
	// Cancel: the prompt may be dismissed without an answer. Reply: it may be
	// dismissed with a message to the agent instead (Claude's "Chat about
	// this", a typed reply).
	Cancel bool `json:"cancel"`
	Reply  bool `json:"reply"`
	// Delivery is the current delivery mode; it changes while the prompt is
	// open (a held Claude prompt released to its pane becomes keys or pane).
	Delivery string `json:"delivery"`
	// Deadline is when the prompt auto-continues or amux releases it to the
	// pane (Unix ms), when known.
	Deadline int64 `json:"deadline,omitempty"`
	// Claim is set while an accepted answer is being delivered.
	Claim *FormClaim `json:"claim,omitempty"`
}

// FormQuestion is one question of a form.
type FormQuestion struct {
	// ID is stable within the form: Codex's question id; "q<i>" by position
	// for Claude (which keys answers by question text on delivery).
	ID          string       `json:"id"`
	Header      string       `json:"header,omitempty"`
	Question    string       `json:"question"`
	Options     []FormOption `json:"options,omitempty"`
	MultiSelect bool         `json:"multi_select,omitempty"`
	// Other: a free-text answer is accepted.
	Other bool `json:"other"`
	// Secret: free text must be masked and never echoed or logged.
	Secret bool `json:"secret,omitempty"`
	// Notes: a per-question note is accepted.
	Notes bool `json:"notes,omitempty"`
}

// FormOption is one choice. Preview is untrusted markdown shown beside the
// options of a single-select question.
type FormOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
	Preview     string `json:"preview,omitempty"`
}

// FormClaim names who is delivering an accepted answer.
type FormClaim struct {
	By string `json:"by"`
	At int64  `json:"at"`
}

// FormAnswer answers (or dismisses) a form.
type FormAnswer struct {
	Version int    `json:"version"`
	Action  string `json:"action"`
	// Message, on cancel, is delivered to the agent as the user's reply.
	// Requires Form.Reply.
	Message string           `json:"message,omitempty"`
	Answers []QuestionAnswer `json:"answers,omitempty"`
}

// QuestionAnswer is the answer to one question: option indices, free text,
// or both (multi-select), plus an optional note.
type QuestionAnswer struct {
	ID       string  `json:"id"`
	Selected []int   `json:"selected,omitempty"`
	Other    *string `json:"other,omitempty"`
	Note     string  `json:"note,omitempty"`
}

// QuestionRequestPayload is the TypeQuestionRequest event payload. RequestID
// is the queue entry id (deterministic over session, runtime generation and
// the call), so a transcript card and a queue row are one object.
type QuestionRequestPayload struct {
	RequestID         string `json:"request_id"`
	RuntimeGeneration string `json:"runtime_generation,omitempty"`
	Form              Form   `json:"form"`
}

// QuestionResolvedPayload is the TypeQuestionResolved event payload. It never
// carries the answer's content.
type QuestionResolvedPayload struct {
	RequestID         string `json:"request_id"`
	RuntimeGeneration string `json:"runtime_generation,omitempty"`
	Outcome           string `json:"outcome"`
	Via               string `json:"via,omitempty"`
}

// Question answer error codes (direct API codes, core.Result codes).
const (
	// CodeNeedsNewerClient: the entry cannot be answered with the legacy
	// answer fields (a secret, a shape they cannot express).
	CodeNeedsNewerClient = "needs_newer_client"
	// CodeDeliveryFailed: amux could not deliver the answer; nothing reached
	// the agent and the entry is open again.
	CodeDeliveryFailed = "delivery_failed"
	// CodeAnswerInPane: the form's delivery is DeliveryPane.
	CodeAnswerInPane = "answer_in_pane"
)

// ErrInvalidAnswer wraps every ValidateAnswer failure.
var ErrInvalidAnswer = errors.New("invalid answer")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidAnswer, fmt.Sprintf(format, args...))
}

// ValidateAnswer checks a against f: one answer per question (ids matching),
// a single-select question takes exactly one index or non-empty free text, a
// multi-select one at least one of either, free text and notes only where the
// question accepts them, cancel and a reply message only where the form does.
func ValidateAnswer(f Form, a FormAnswer) error {
	if a.Version != FormVersion {
		return invalid("version %d (want %d)", a.Version, FormVersion)
	}
	switch a.Action {
	case FormActionCancel:
		if !f.Cancel {
			return invalid("this prompt cannot be dismissed")
		}
		if strings.TrimSpace(a.Message) != "" && !f.Reply {
			return invalid("this prompt does not take a reply message")
		}
		if len(a.Message) > MaxOtherBytes {
			return invalid("message too long")
		}
		if len(a.Answers) > 0 {
			return invalid("a cancel carries no answers")
		}
		return nil
	case FormActionAnswer:
	default:
		return invalid("unknown action %q", a.Action)
	}
	if a.Message != "" {
		return invalid("an answer carries no message")
	}
	if len(a.Answers) != len(f.Questions) {
		return invalid("%d answers for %d questions", len(a.Answers), len(f.Questions))
	}
	byID := map[string]QuestionAnswer{}
	for _, qa := range a.Answers {
		if _, dup := byID[qa.ID]; dup {
			return invalid("question %q answered twice", qa.ID)
		}
		byID[qa.ID] = qa
	}
	for _, q := range f.Questions {
		qa, ok := byID[q.ID]
		if !ok {
			return invalid("question %q is not answered", q.ID)
		}
		seen := map[int]bool{}
		for _, i := range qa.Selected {
			if i < 0 || i >= len(q.Options) {
				return invalid("question %q has no option %d", q.ID, i)
			}
			if seen[i] {
				return invalid("question %q selects option %d twice", q.ID, i)
			}
			seen[i] = true
		}
		other := ""
		if qa.Other != nil {
			other = strings.TrimSpace(*qa.Other)
			if !q.Other {
				return invalid("question %q takes no free text", q.ID)
			}
			if other == "" {
				return invalid("question %q has empty free text", q.ID)
			}
			if len(*qa.Other) > MaxOtherBytes {
				return invalid("question %q free text too long", q.ID)
			}
		}
		if qa.Note != "" {
			if !q.Notes {
				return invalid("question %q takes no note", q.ID)
			}
			if utf8.RuneCountInString(qa.Note) > MaxNoteRunes {
				return invalid("question %q note too long", q.ID)
			}
		}
		n := len(qa.Selected)
		if other != "" {
			n++
		}
		switch {
		case n == 0:
			return invalid("question %q has no answer", q.ID)
		case !q.MultiSelect && n > 1:
			return invalid("question %q takes one answer", q.ID)
		}
	}
	return nil
}

// LegacyAnswerable reports whether the pre-v2 answer fields (option labels per
// question, one free-text answer) can answer f without loss of meaning, and
// whether its questions may be shown to a pre-v2 client at all. A secret, or
// two questions with the same text (Claude keys answers by text), are not.
func LegacyAnswerable(f Form) bool {
	seen := map[string]bool{}
	for _, q := range f.Questions {
		if q.Secret {
			return false
		}
		k := strings.TrimSpace(q.Question)
		if seen[k] {
			return false
		}
		seen[k] = true
	}
	return f.Source == FormSourceClaudeAsk || f.Source == FormSourceCodexUserInput
}
