package harnessproto

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func strp(s string) *string { return &s }

// sampleForm exercises every form feature: a preview question with notes, a
// multi-select with Other, and a free-text secret (Codex).
func sampleForm() Form {
	return Form{
		Version: FormVersion, Source: FormSourceClaudeAsk, Cancel: true, Reply: true, Delivery: DeliveryStructured,
		Questions: []FormQuestion{
			{ID: "q0", Header: "Layout", Question: "Which layout should the settings page use?", Other: true, Notes: true,
				Options: []FormOption{
					{Label: "Tabs (Recommended)", Description: "One tab per area", Preview: "┌Tabs┐\n│General│Account│"},
					{Label: "Single page", Description: "Long scroll", Preview: "General\nAccount"},
				}},
			{ID: "q1", Header: "Extras", Question: "Which extras do you want?", MultiSelect: true, Other: true, Notes: true,
				Options: []FormOption{{Label: "Search"}, {Label: "Dark mode"}, {Label: "Export"}}},
		},
	}
}

func init() {
	f := sampleForm()
	req, _ := json.Marshal(QuestionRequestPayload{RequestID: "q-k3v9x2m4pa", RuntimeGeneration: "gen-7", Form: f})
	res, _ := json.Marshal(QuestionResolvedPayload{RequestID: "q-k3v9x2m4pa", RuntimeGeneration: "gen-7", Outcome: QuestionAnswered, Via: ViaPane})
	ans, _ := json.Marshal(FormAnswer{Version: FormVersion, Action: FormActionAnswer, Answers: []QuestionAnswer{
		{ID: "q0", Selected: []int{0}, Note: "keep icons"},
		{ID: "q1", Selected: []int{0, 1}, Other: strp("keyboard map")},
	}})
	goldenFrames = append(goldenFrames,
		struct {
			name string
			msg  any
		}{"question_request", HarnessMsg{
			Type: HRuntimeEvents, SessionID: "s1", Runtime: RuntimeClaude, Seq: 9,
			Events: []RuntimeEvent{{Type: TypeQuestionRequest, ItemID: "q-k3v9x2m4pa", Direction: DirOut, Payload: req}},
		}},
		struct {
			name string
			msg  any
		}{"question_resolved", HarnessMsg{
			Type: HRuntimeEvents, SessionID: "s1", Runtime: RuntimeClaude, Seq: 10,
			Events: []RuntimeEvent{{Type: TypeQuestionResolved, ItemID: "q-k3v9x2m4pa", Direction: DirOut, Payload: res}},
		}},
		struct {
			name string
			msg  any
		}{"session_action_answer", MuxMsg{
			Type: MSessionAction, ReqID: "a9", Action: VerbAnswer, ID: "s1",
			Fields: map[string]string{FieldRequestID: "q-k3v9x2m4pa", FieldRuntimeGeneration: "gen-7", FieldAnswer: string(ans)},
		}},
		struct {
			name string
			msg  any
		}{"session_caps_question", SessionCaps{Prompt: true, Interject: true, Cancel: true, Question: true}},
	)
}

func TestQuestionVerbAccepted(t *testing.T) {
	if !SessionVerbs[VerbAnswer] || !SteeringVerbs[VerbAnswer] {
		t.Fatal("VerbAnswer must be a session and a steering verb")
	}
}

func TestValidateAnswer(t *testing.T) {
	f := sampleForm()
	ok := func(qs ...QuestionAnswer) FormAnswer {
		return FormAnswer{Version: FormVersion, Action: FormActionAnswer, Answers: qs}
	}
	cases := []struct {
		name string
		f    Form
		a    FormAnswer
		err  string
	}{
		{"single and multi", f, ok(QuestionAnswer{ID: "q0", Selected: []int{1}}, QuestionAnswer{ID: "q1", Selected: []int{0, 2}}), ""},
		{"other alone", f, ok(QuestionAnswer{ID: "q0", Other: strp("a grid")}, QuestionAnswer{ID: "q1", Other: strp("none")}), ""},
		{"multi plus other, notes", f, ok(QuestionAnswer{ID: "q0", Selected: []int{0}, Note: "n"}, QuestionAnswer{ID: "q1", Selected: []int{1}, Other: strp("x"), Note: "m"}), ""},
		{"order free", f, ok(QuestionAnswer{ID: "q1", Selected: []int{0}}, QuestionAnswer{ID: "q0", Selected: []int{0}}), ""},
		{"cancel", f, FormAnswer{Version: FormVersion, Action: FormActionCancel}, ""},
		{"cancel with reply", f, FormAnswer{Version: FormVersion, Action: FormActionCancel, Message: "let's talk"}, ""},
		{"bad version", f, FormAnswer{Version: 1, Action: FormActionCancel}, "version"},
		{"unknown action", f, FormAnswer{Version: FormVersion, Action: "skip"}, "unknown action"},
		{"missing question", f, ok(QuestionAnswer{ID: "q0", Selected: []int{0}}), "1 answers for 2"},
		{"wrong id", f, ok(QuestionAnswer{ID: "q0", Selected: []int{0}}, QuestionAnswer{ID: "qx", Selected: []int{0}}), "not answered"},
		{"twice", f, ok(QuestionAnswer{ID: "q0", Selected: []int{0}}, QuestionAnswer{ID: "q0", Selected: []int{1}}), "twice"},
		{"out of range", f, ok(QuestionAnswer{ID: "q0", Selected: []int{2}}, QuestionAnswer{ID: "q1", Selected: []int{0}}), "no option 2"},
		{"negative", f, ok(QuestionAnswer{ID: "q0", Selected: []int{-1}}, QuestionAnswer{ID: "q1", Selected: []int{0}}), "no option -1"},
		{"dup index", f, ok(QuestionAnswer{ID: "q0", Selected: []int{0}}, QuestionAnswer{ID: "q1", Selected: []int{1, 1}}), "twice"},
		{"two on single", f, ok(QuestionAnswer{ID: "q0", Selected: []int{0, 1}}, QuestionAnswer{ID: "q1", Selected: []int{0}}), "takes one answer"},
		{"select plus other on single", f, ok(QuestionAnswer{ID: "q0", Selected: []int{0}, Other: strp("x")}, QuestionAnswer{ID: "q1", Selected: []int{0}}), "takes one answer"},
		{"empty", f, ok(QuestionAnswer{ID: "q0"}, QuestionAnswer{ID: "q1", Selected: []int{0}}), "no answer"},
		{"blank other", f, ok(QuestionAnswer{ID: "q0", Other: strp("  ")}, QuestionAnswer{ID: "q1", Selected: []int{0}}), "empty free text"},
		{"answer with message", f, FormAnswer{Version: FormVersion, Action: FormActionAnswer, Message: "x", Answers: ok(QuestionAnswer{ID: "q0", Selected: []int{0}}, QuestionAnswer{ID: "q1", Selected: []int{0}}).Answers}, "no message"},
		{"cancel with answers", f, FormAnswer{Version: FormVersion, Action: FormActionCancel, Answers: []QuestionAnswer{{ID: "q0"}}}, "no answers"},
	}
	noOther := sampleForm()
	noOther.Questions[0].Other, noOther.Questions[0].Notes = false, false
	noOther.Cancel, noOther.Reply = false, false
	cases = append(cases,
		struct {
			name string
			f    Form
			a    FormAnswer
			err  string
		}{"other not offered", noOther, ok(QuestionAnswer{ID: "q0", Other: strp("x")}, QuestionAnswer{ID: "q1", Selected: []int{0}}), "no free text"},
		struct {
			name string
			f    Form
			a    FormAnswer
			err  string
		}{"note not offered", noOther, ok(QuestionAnswer{ID: "q0", Selected: []int{0}, Note: "x"}, QuestionAnswer{ID: "q1", Selected: []int{0}}), "no note"},
		struct {
			name string
			f    Form
			a    FormAnswer
			err  string
		}{"cancel not offered", noOther, FormAnswer{Version: FormVersion, Action: FormActionCancel}, "cannot be dismissed"},
	)
	replyless := sampleForm()
	replyless.Reply = false
	cases = append(cases, struct {
		name string
		f    Form
		a    FormAnswer
		err  string
	}{"reply not offered", replyless, FormAnswer{Version: FormVersion, Action: FormActionCancel, Message: "x"}, "reply message"})

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateAnswer(c.f, c.a)
			if c.err == "" {
				if err != nil {
					t.Fatalf("want ok, got %v", err)
				}
				return
			}
			if err == nil || !errors.Is(err, ErrInvalidAnswer) || !strings.Contains(err.Error(), c.err) {
				t.Fatalf("want error containing %q, got %v", c.err, err)
			}
		})
	}
}

func TestLegacyAnswerable(t *testing.T) {
	f := sampleForm()
	if !LegacyAnswerable(f) {
		t.Fatal("a plain Claude form is legacy answerable")
	}
	s := sampleForm()
	s.Questions[1].Secret = true
	if LegacyAnswerable(s) {
		t.Fatal("a secret question must not reach a legacy client")
	}
	d := sampleForm()
	d.Questions[1].Question = d.Questions[0].Question
	if LegacyAnswerable(d) {
		t.Fatal("duplicate question texts are not legacy answerable")
	}
	e := sampleForm()
	e.Source = FormSourceCodexElicit
	if LegacyAnswerable(e) {
		t.Fatal("elicitation is not legacy answerable")
	}
}

// TestFormUnknownKeysIgnored pins the forward-compat rule: a newer daemon's
// additive keys decode cleanly into this version's types.
func TestFormUnknownKeysIgnored(t *testing.T) {
	raw := `{"version":2,"source":"claude_ask_user_question","cancel":true,"reply":false,"delivery":"keys","future":1,
		"questions":[{"id":"q0","question":"Q?","other":true,"rank":3,"options":[{"label":"A","icon":"x"}]}]}`
	var f Form
	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		t.Fatal(err)
	}
	if f.Questions[0].Options[0].Label != "A" || f.Delivery != DeliveryKeys {
		t.Fatalf("decoded %+v", f)
	}
}
