package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"amux/internal/core"
	"amux/internal/queue"
	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// `amux question` answers agents' structured questions (docs/question-prompts.md
// §5.4) with every feature of the harness's own dialog: per-question
// selections, "Other" free text, notes, dismissal with or without a reply, and
// handing a held question back to the pane.

// Exit codes for a refused answer, so a script can tell them apart.
const (
	exitAnswered         = 3
	exitAskerGone        = 4
	exitDeliveryFailed   = 5
	exitNeedsNewerClient = 6
)

// codedError carries a CLI exit code.
type codedError struct {
	msg  string
	code int
}

func (e *codedError) Error() string { return e.msg }
func (e *codedError) ExitCode() int { return e.code }

func cmdQuestion(args []string) error {
	if len(args) == 0 {
		args = []string{"list"}
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list", "ls":
		return questionList(rest, os.Stdout)
	case "show":
		return questionShow(rest, os.Stdout)
	case "answer":
		if len(rest) < 1 {
			return usageErr("question answer")
		}
		e, err := findQuestion(rest[0])
		if err != nil {
			return err
		}
		a, err := parseQuestionAnswer(*e.Form, rest[1:], os.Stdin)
		if err != nil {
			return err
		}
		return sendQuestionAnswer(e.ID, a, "answered "+e.ID)
	case "cancel", "dismiss":
		if len(rest) < 1 {
			return usageErr("question cancel")
		}
		msg := ""
		for i := 1; i < len(rest); i++ {
			switch {
			case (rest[i] == "--message" || rest[i] == "-m") && i+1 < len(rest):
				i++
				msg = rest[i]
			case strings.HasPrefix(rest[i], "--message="):
				msg = strings.TrimPrefix(rest[i], "--message=")
			default:
				return usagef("unknown argument %q", rest[i])
			}
		}
		return sendQuestionAnswer(rest[0], harnessproto.FormAnswer{Version: harnessproto.FormVersion, Action: harnessproto.FormActionCancel, Message: msg}, "dismissed "+rest[0])
	case "release":
		if len(rest) != 1 {
			return usageErr("question release")
		}
		return questionHostResult(core.Action{Action: core.ActionQueueRelease, Fields: map[string]string{core.QueueEntryField: rest[0]}}, "released "+rest[0]+" to the agent's pane")
	case "help", "-h", "--help":
		return showHelp("question")
	}
	return usagef("unknown question subcommand %q", sub)
}

// questions lists the open question entries that carry a form.
func questions(all bool) ([]queue.Entry, error) {
	o, err := readQueue(all)
	if err != nil {
		return nil, err
	}
	var out []queue.Entry
	for _, e := range o.Entries {
		if e.Kind == queue.KindQuestion && e.Form != nil {
			out = append(out, e)
		}
	}
	return out, nil
}

func findQuestion(id string) (queue.Entry, error) {
	qs, err := questions(true)
	if err != nil {
		return queue.Entry{}, err
	}
	for _, e := range qs {
		if e.ID == id {
			return e, nil
		}
	}
	return queue.Entry{}, &codedError{msg: fmt.Sprintf("no open question %s (`amux question list` shows what is open)", id), code: exitAskerGone}
}

func questionList(args []string, w io.Writer) error {
	all, asJSON := false, false
	for _, a := range args {
		switch a {
		case "--all", "-a":
			all = true
		case "--json", "-j":
			asJSON = true
		default:
			return usagef("unknown argument %q", a)
		}
	}
	qs, err := questions(all)
	if err != nil {
		return err
	}
	if asJSON {
		if qs == nil {
			qs = []queue.Entry{}
		}
		b, err := json.MarshalIndent(qs, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(w, string(b))
		return nil
	}
	if len(qs) == 0 {
		fmt.Fprintln(w, "no open questions")
		return nil
	}
	for _, e := range qs {
		renderQuestion(w, e, false)
		fmt.Fprintln(w)
	}
	return nil
}

func questionShow(args []string, w io.Writer) error {
	if len(args) < 1 {
		return usageErr("question show")
	}
	e, err := findQuestion(args[0])
	if err != nil {
		return err
	}
	if len(args) > 1 && (args[1] == "--json" || args[1] == "-j") {
		b, err := json.MarshalIndent(e, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(w, string(b))
		return nil
	}
	renderQuestion(w, e, true)
	return nil
}

// deliveryWords says how amux will deliver an answer.
func deliveryWords(f harnessproto.Form) string {
	switch f.Delivery {
	case harnessproto.DeliveryStructured:
		s := "answers go straight to the agent"
		if f.Deadline > 0 {
			s += fmt.Sprintf(" (until %s, then it opens in the pane)", time.UnixMilli(f.Deadline).Format(time.Kitchen))
		}
		return s
	case harnessproto.DeliveryKeys:
		return "amux answers the dialog in the agent's pane"
	}
	return "answer this in the agent's pane"
}

// renderQuestion prints a form as plain text (all of it untrusted: printed,
// never interpreted). Previews are shown only with full.
func renderQuestion(w io.Writer, e queue.Entry, full bool) {
	f := *e.Form
	who := firstNonEmptyStr(e.SessionTitle, e.Session)
	fmt.Fprintf(w, "%s  %s  [%s]\n", e.ID, flatLine(who), deliveryWords(f))
	if f.Claim != nil {
		fmt.Fprintf(w, "  being answered by %s\n", f.Claim.By)
	}
	for qi, q := range f.Questions {
		head := ""
		if q.Header != "" {
			head = "[" + flatLine(q.Header) + "] "
		}
		kind := "pick one"
		if q.MultiSelect {
			kind = "pick any"
		}
		if len(q.Options) == 0 {
			kind = "free text"
		}
		fmt.Fprintf(w, "  %d. %s%s  (%s)\n", qi+1, head, flatLine(q.Question), kind)
		for oi, o := range q.Options {
			line := fmt.Sprintf("     %d) %s", oi+1, flatLine(o.Label))
			if o.Description != "" {
				line += " — " + flatLine(o.Description)
			}
			fmt.Fprintln(w, line)
			if full && o.Preview != "" {
				for _, pl := range strings.Split(o.Preview, "\n") {
					fmt.Fprintln(w, "        │ "+strings.Map(printable, pl))
				}
			}
		}
		var extras []string
		if q.Other {
			if q.Secret {
				extras = append(extras, "--other (secret)")
			} else {
				extras = append(extras, "--other <text>")
			}
		}
		if q.Notes {
			extras = append(extras, "--note <text>")
		}
		if len(extras) > 0 {
			fmt.Fprintf(w, "     also: %s\n", strings.Join(extras, ", "))
		}
	}
	if f.Delivery != harnessproto.DeliveryPane {
		fmt.Fprintf(w, "  answer: amux question answer %s --q 1 --select 1 ...", e.ID)
		if f.Cancel {
			fmt.Fprintf(w, "   dismiss: amux question cancel %s [--message <text>]", e.ID)
		}
		fmt.Fprintln(w)
	}
}

// printable drops control characters from untrusted text.
func printable(r rune) rune {
	if r == '\t' {
		return ' '
	}
	if r < 0x20 || r == 0x7f {
		return -1
	}
	return r
}

func flatLine(s string) string {
	return strings.Map(printable, strings.Join(strings.Fields(s), " "))
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// parseQuestionAnswer reads the answer flags against the form:
//
//	[--q <n|id|header>] --select <label|n>... [--other <text|->] [--note <text>] ...
//
// --q starts a question's group (optional for a one-question form); --select
// matches an exact label first, then a 1-based option number; "--other -"
// reads the text from stdin.
func parseQuestionAnswer(f harnessproto.Form, args []string, stdin io.Reader) (harnessproto.FormAnswer, error) {
	a := harnessproto.FormAnswer{Version: harnessproto.FormVersion, Action: harnessproto.FormActionAnswer}
	answers := map[string]*harnessproto.QuestionAnswer{}
	var order []string
	cur := -1
	if len(f.Questions) == 1 {
		cur = 0
	}
	group := func() (*harnessproto.QuestionAnswer, error) {
		if cur < 0 {
			return nil, usagef("this form has %d questions: start each one with --q <number|header>", len(f.Questions))
		}
		id := f.Questions[cur].ID
		if answers[id] == nil {
			answers[id] = &harnessproto.QuestionAnswer{ID: id}
			order = append(order, id)
		}
		return answers[id], nil
	}
	value := func(i *int, name string) (string, error) {
		a := args[*i]
		if v, ok := strings.CutPrefix(a, name+"="); ok {
			return v, nil
		}
		if *i+1 >= len(args) {
			return "", usagef("%s needs a value", name)
		}
		*i++
		return args[*i], nil
	}
	for i := 0; i < len(args); i++ {
		flag := args[i]
		name := flag
		if k, _, ok := strings.Cut(flag, "="); ok {
			name = k
		}
		switch name {
		case "--q", "--question":
			v, err := value(&i, name)
			if err != nil {
				return a, err
			}
			cur = questionIndex(f, v)
			if cur < 0 {
				return a, usagef("no question %q (use its number, id or header)", v)
			}
		case "--select", "-s":
			v, err := value(&i, name)
			if err != nil {
				return a, err
			}
			qa, err := group()
			if err != nil {
				return a, err
			}
			idx := optionIndexFor(f.Questions[cur], v)
			if idx < 0 {
				return a, usagef("question %d has no option %q (options: %s)", cur+1, v, optionList(f.Questions[cur]))
			}
			qa.Selected = append(qa.Selected, idx)
		case "--other", "-o":
			v, err := value(&i, name)
			if err != nil {
				return a, err
			}
			if v == "-" {
				line, err := bufio.NewReader(stdin).ReadString('\n')
				if err != nil && !errors.Is(err, io.EOF) {
					return a, err
				}
				v = strings.TrimRight(line, "\r\n")
			}
			qa, err := group()
			if err != nil {
				return a, err
			}
			qa.Other = &v
		case "--note", "-n":
			v, err := value(&i, name)
			if err != nil {
				return a, err
			}
			qa, err := group()
			if err != nil {
				return a, err
			}
			qa.Note = v
		default:
			return a, usagef("unknown argument %q", flag)
		}
	}
	for _, id := range order {
		a.Answers = append(a.Answers, *answers[id])
	}
	if err := harnessproto.ValidateAnswer(f, a); err != nil {
		return a, usagef("%v", err)
	}
	return a, nil
}

func questionIndex(f harnessproto.Form, v string) int {
	if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= len(f.Questions) {
		return n - 1
	}
	for i, q := range f.Questions {
		if q.ID == v || strings.EqualFold(q.Header, v) || q.Question == v {
			return i
		}
	}
	return -1
}

func optionIndexFor(q harnessproto.FormQuestion, v string) int {
	for i, o := range q.Options {
		if o.Label == v {
			return i
		}
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= len(q.Options) {
		return n - 1
	}
	return -1
}

func optionList(q harnessproto.FormQuestion) string {
	var l []string
	for i, o := range q.Options {
		l = append(l, fmt.Sprintf("%d) %s", i+1, o.Label))
	}
	if len(l) == 0 {
		return "none; use --other"
	}
	return strings.Join(l, ", ")
}

// sendQuestionAnswer delivers a form answer as the host.
func sendQuestionAnswer(id string, a harnessproto.FormAnswer, done string) error {
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	return questionHostResult(core.Action{Action: core.ActionQueueAnswer, Fields: map[string]string{
		core.QueueEntryField: id, core.QueueFormField: string(b),
	}}, done)
}

// questionHostResult runs a user-only queue action from a host terminal and
// maps a refusal's code to its exit code.
func questionHostResult(a core.Action, done string) error {
	if sessionContextRestricted() {
		return errors.New("only the user answers questions: run `amux question` in your own terminal (or press i in the TUI)")
	}
	res, err := sendHostResult(a)
	if err != nil {
		return err
	}
	if !res.OK {
		return &codedError{msg: res.Error, code: questionExitCode(res.Code)}
	}
	fmt.Println(done)
	return nil
}

func questionExitCode(code string) int {
	switch code {
	case "answered":
		return exitAnswered
	case "asker_gone":
		return exitAskerGone
	case harnessproto.CodeDeliveryFailed, harnessproto.CodeAnswerInPane:
		return exitDeliveryFailed
	case harnessproto.CodeNeedsNewerClient:
		return exitNeedsNewerClient
	}
	return 1
}

// sendHostResult sends an action on the host connection and returns the
// daemon's whole result (code included).
func sendHostResult(a core.Action) (core.Result, error) {
	c, err := dial()
	if err != nil {
		return core.Result{}, err
	}
	defer c.Close()
	if err := c.Send(a); err != nil {
		return core.Result{}, err
	}
	for {
		f, err := c.Next()
		if err != nil {
			return core.Result{}, err
		}
		if f.Result != nil {
			return *f.Result, nil
		}
	}
}
