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

	"amux/internal/core"
	"amux/internal/items"
	"amux/internal/queue"
	"amux/internal/sessionrpc"

	"github.com/charmbracelet/x/ansi"
)

// amux inbox — the user question queue (internal/queue, docs/direct-console.md
// §4): every pending question for the user, from any session, in one place,
// with answers routed back to the asking session. Reading it works from a host
// terminal and from the admin console and the overseer; answering, dismissing
// and acknowledging are the user's alone (host terminal).

// queueAnswerFields parses `amux inbox answer` arguments after the entry id.
func queueAnswerFields(entry string, args []string) (map[string]string, error) {
	fields := map[string]string{core.QueueEntryField: entry}
	var text, choices []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--allow":
			fields[core.SteerDecision] = core.SteerAllow
		case a == "--deny":
			fields[core.SteerDecision] = core.SteerDeny
		case a == "--choice" || a == "-c":
			if i+1 >= len(args) {
				return nil, fmt.Errorf("%s needs a label", a)
			}
			i++
			choices = append(choices, args[i])
		case strings.HasPrefix(a, "--choice="):
			choices = append(choices, strings.TrimPrefix(a, "--choice="))
		case a == "--answers":
			if i+1 >= len(args) {
				return nil, fmt.Errorf("%s needs a JSON array", a)
			}
			i++
			fields[core.QueueAnswersField] = args[i]
		case strings.HasPrefix(a, "--answers="):
			fields[core.QueueAnswersField] = strings.TrimPrefix(a, "--answers=")
		case a == "--text" || a == "-m":
			if i+1 >= len(args) {
				return nil, fmt.Errorf("%s needs text", a)
			}
			i++
			text = append(text, args[i])
		case a == "--":
			text = append(text, args[i+1:]...)
			i = len(args)
		case strings.HasPrefix(a, "-") && a != "-":
			return nil, usagef("unknown flag %q", a)
		default:
			text = append(text, a)
		}
	}
	if len(text) > 0 {
		fields[core.QueueTextField] = strings.Join(text, " ")
	}
	if len(choices) > 0 {
		fields[core.QueueChoiceField] = strings.Join(choices, "\n")
	}
	if a := fields[core.QueueAnswersField]; a != "" {
		var as []queue.Answer
		if err := json.Unmarshal([]byte(a), &as); err != nil {
			return nil, fmt.Errorf(`--answers must be a JSON array of {"choice":[labels],"other":"text"}: %v`, err)
		}
		if len(text) > 0 || len(choices) > 0 {
			return nil, errors.New("--answers replaces --choice and text: use one")
		}
	}
	if fields[core.SteerDecision] == "" && len(text) == 0 && len(choices) == 0 && fields[core.QueueAnswersField] == "" {
		return nil, errors.New("an answer needs text, --choice <label>, --answers <json>, or --allow/--deny (or run it in a terminal to be asked)")
	}
	return fields, nil
}

// queueAnswerInteractive asks each of an entry's questions on a terminal,
// with the whole question and every option shown, and returns the answer
// fields. A single_select takes an option number or your own text; a
// multi_select takes option numbers (space or comma separated), and any
// other words are your own text; a text question takes a line. An empty line
// skips a question.
func queueAnswerInteractive(e queue.Entry, in io.Reader, out io.Writer, width int) (map[string]string, error) {
	r := bufio.NewReader(in)
	readLine := func(prompt string) (string, error) {
		fmt.Fprint(out, prompt)
		line, err := r.ReadString('\n')
		if err != nil && (err != io.EOF || line == "") {
			return "", err
		}
		return strings.TrimSpace(line), nil
	}
	fields := map[string]string{core.QueueEntryField: e.ID}
	switch e.Kind {
	case queue.KindPermission:
		writeWrapped(out, "", firstNonEmptyLine(e.Title), width)
		writeWrapped(out, "    ", e.Body, width)
		for {
			ans, err := readLine("allow or deny? [a/d] ")
			if err != nil {
				return nil, err
			}
			switch strings.ToLower(ans) {
			case "a", "allow", "y", "yes":
				fields[core.SteerDecision] = core.SteerAllow
				return fields, nil
			case "d", "deny", "n", "no":
				fields[core.SteerDecision] = core.SteerDeny
				return fields, nil
			}
		}
	case queue.KindQuestion:
	default:
		writeWrapped(out, "", e.Title, width)
		if e.Body != e.Title {
			writeWrapped(out, "    ", e.Body, width)
		}
		ans, err := readLine("answer: ")
		if err != nil {
			return nil, err
		}
		if ans == "" {
			return nil, errors.New("no answer given")
		}
		fields[core.QueueTextField] = ans
		return fields, nil
	}
	qs := make([]queue.Question, len(e.Questions))
	as := make([]queue.Answer, len(e.Questions))
	for i, q := range e.Questions {
		qs[i] = q.Normalize()
	}
	for i := 0; i < len(qs); i++ {
		q := qs[i]
		fmt.Fprintln(out)
		writeQuestion(out, q, width)
		prompt := map[string]string{
			queue.InputSingleSelect: "choose a number, or type your own answer: ",
			queue.InputMultiSelect:  "choose numbers (e.g. 1 3), plus any words of your own: ",
			queue.InputText:         "answer: ",
		}[q.Input]
		ans, err := readLine(prompt)
		if err != nil {
			return nil, err
		}
		a, err := parseInteractiveAnswer(q, ans)
		if err != nil {
			fmt.Fprintln(out, err)
			i-- // ask again
			continue
		}
		as[i] = a
	}
	if err := queue.ValidateAnswers(qs, as); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(as)
	if err != nil {
		return nil, err
	}
	fields[core.QueueAnswersField] = string(raw)
	return fields, nil
}

// parseInteractiveAnswer reads one typed line as a question's answer.
func parseInteractiveAnswer(q queue.Question, line string) (queue.Answer, error) {
	if line == "" || q.Input == queue.InputText {
		return queue.Answer{Other: line}, nil
	}
	pick := func(tok string) (string, bool) {
		n, err := strconv.Atoi(tok)
		if err != nil || n < 1 || n > len(q.Options) {
			return "", false
		}
		return q.Options[n-1].Label, true
	}
	if q.Input == queue.InputSingleSelect {
		if l, ok := pick(line); ok {
			return queue.Answer{Choice: []string{l}}, nil
		}
		return queue.Answer{Other: line}, nil
	}
	var a queue.Answer
	var words []string
	seen := map[string]bool{}
	for _, tok := range strings.FieldsFunc(line, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
		if l, ok := pick(tok); ok && len(words) == 0 {
			if !seen[l] {
				seen[l] = true
				a.Choice = append(a.Choice, l)
			}
			continue
		}
		words = append(words, tok)
	}
	a.Other = strings.Join(words, " ")
	return a, nil
}

// writeQuestion prints one question whole: header, text, how to answer it,
// and every option with its description and preview.
func writeQuestion(w io.Writer, q queue.Question, width int) {
	text := q.Question
	if q.Header != "" {
		text = "[" + q.Header + "] " + text
	}
	writeWrapped(w, "    ? ", text, width)
	hint := map[string]string{queue.InputSingleSelect: "choose one", queue.InputMultiSelect: "choose any", queue.InputText: "type an answer"}[q.Input]
	if q.AllowOther {
		hint += ", or type your own"
	}
	fmt.Fprintf(w, "      (%s)\n", hint)
	for i, o := range q.Options {
		writeWrapped(w, fmt.Sprintf("      %d. ", i+1), o.Label, width)
		if o.Description != "" {
			writeWrapped(w, "         ", o.Description, width)
		}
		if o.Preview != "" {
			writeWrapped(w, "         | ", o.Preview, width)
		}
	}
}

// writeWrapped prints agent text whole, never cut: control characters
// dropped, word-wrapped to width with continuation lines indented under the
// prefix. Line breaks in the text are kept (each line hangs the same way).
func writeWrapped(w io.Writer, prefix, text string, width int) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	indent := strings.Repeat(" ", ansi.StringWidth(prefix))
	avail := max(20, width-len(indent))
	first := true
	for _, para := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		para = strings.TrimSpace(oneLine(para, len(para)+1))
		for _, l := range strings.Split(ansi.Wrap(para, avail, ""), "\n") {
			if first {
				fmt.Fprintln(w, prefix+l)
				first = false
			} else {
				fmt.Fprintln(w, indent+l)
			}
		}
	}
}

func firstNonEmptyLine(s ...string) string {
	for _, v := range s {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// queueTextWidth is the width `amux inbox` wraps to.
func queueTextWidth() int {
	if w := terminalWidth(os.Stdout); w >= 40 {
		return min(w, 120)
	}
	return 100
}

func cmdQueue(args []string) error {
	sub := "list"
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		return showHelp("inbox")
	}
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "list", "ls":
		return queueList(args, os.Stdout)
	case "answer", "reply":
		if len(args) < 1 {
			return usageErr("inbox answer")
		}
		if len(args) == 1 && isTerminal(os.Stdin) && !sessionContextRestricted() {
			e, err := findQueueEntry(args[0])
			if err != nil {
				return err
			}
			fields, err := queueAnswerInteractive(e, os.Stdin, os.Stdout, queueTextWidth())
			if err != nil {
				return err
			}
			return queueHostAction(core.Action{Action: core.ActionQueueAnswer, Fields: fields}, "answered "+args[0])
		}
		fields, err := queueAnswerFields(args[0], args[1:])
		if err != nil {
			return err
		}
		return queueHostAction(core.Action{Action: core.ActionQueueAnswer, Fields: fields}, "answered "+args[0])
	case "dismiss":
		if len(args) != 1 {
			return usageErr("inbox dismiss")
		}
		return queueHostAction(core.Action{Action: core.ActionQueueDismiss, Fields: map[string]string{core.QueueEntryField: args[0]}}, "dismissed "+args[0])
	case "ack", "acknowledge":
		if len(args) != 1 {
			return usageErr("inbox ack")
		}
		return queueHostAction(core.Action{Action: core.ActionItemState, ID: items.GlobalInbox(), Fields: map[string]string{
			core.ItemIDField: args[0], core.ItemStateField: items.VerbAck,
		}}, "acknowledged "+args[0])
	case "help", "-h", "--help":
		return showHelp("inbox")
	}
	return usagef("unknown inbox subcommand %q", sub)
}

// queueHostAction runs a user-only queue action from a host terminal.
func queueHostAction(a core.Action, done string) error {
	if sessionContextRestricted() {
		return errors.New("only the user answers the question queue: run `amux inbox` in your own terminal (or press i in the TUI)")
	}
	if err := sendAction(a); err != nil {
		return err
	}
	fmt.Println(done)
	return nil
}

// findQueueEntry reads one open entry by id.
func findQueueEntry(id string) (queue.Entry, error) {
	o, err := readQueue(true)
	if err != nil {
		return queue.Entry{}, err
	}
	for _, e := range o.Entries {
		if e.ID == id {
			return e, nil
		}
	}
	return queue.Entry{}, fmt.Errorf("queue entry %s is not open (`amux inbox` lists what is)", id)
}

// readQueue fetches the queue overview from the daemon.
func readQueue(all bool) (queue.Overview, error) {
	var o queue.Overview
	var fields map[string]string
	if all {
		fields = map[string]string{core.QueueAllField: "1"}
	}
	if sessionContextRestricted() {
		err := restrictedQueryRequest(sessionrpc.Query{Verb: core.QueryQueue, Fields: fields}, &o)
		return o, err
	}
	c, err := dial()
	if err != nil {
		return o, err
	}
	defer c.Close()
	raw, err := c.QueryFields(core.QueryQueue, fields)
	if err != nil {
		if strings.Contains(err.Error(), "unknown query") {
			return o, errors.New("the running daemon predates the question queue; it is available after `amux daemon restart`")
		}
		return o, err
	}
	if len(raw) > 0 {
		err = json.Unmarshal(raw, &o)
	}
	return o, err
}

func queueList(args []string, w io.Writer) error {
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
	o, err := readQueue(all)
	if err != nil {
		return err
	}
	if asJSON {
		b, err := json.MarshalIndent(o, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(w, string(b))
		return nil
	}
	renderQueue(w, o)
	return nil
}

// renderQueue prints the overview as text. Entry text comes from agents: it is
// printed as plain text, wrapped whole (never cut), never interpreted.
func renderQueue(w io.Writer, o queue.Overview) {
	width := queueTextWidth()
	s := o.Summary
	fmt.Fprintf(w, "workgroups: %d need you · %d running · %d waiting for review/merge · %d done\n", s.NeedsYou, s.Running, s.Review, s.Done)
	fmt.Fprintf(w, "global inbox: %d open · %d acknowledged\n", o.Inbox.Open, o.Inbox.Acknowledged)
	if len(o.Entries) == 0 {
		fmt.Fprintln(w, "\nno pending questions")
	} else {
		fmt.Fprintf(w, "\n%d pending question(s):\n", o.Counts.Total)
	}
	for _, e := range o.Entries {
		who := e.Session
		if e.SessionTitle != "" && e.SessionTitle != e.Session {
			who += " (" + oneLine(e.SessionTitle, 40) + ")"
		}
		if e.Workgroup != "" && e.Workgroup != e.Session {
			who = e.Workgroup + "/" + who
		}
		mark := ""
		if e.Dismissed {
			mark = " [dismissed]"
		}
		fmt.Fprintf(w, "\n%s  %s  %s%s\n", e.ID, e.Kind, who, mark)
		if e.Kind != queue.KindQuestion || len(e.Questions) == 0 {
			writeWrapped(w, "    ", e.Title, width)
		}
		switch e.Kind {
		case queue.KindQuestion:
			for _, q := range e.Questions {
				writeQuestion(w, q.Normalize(), width)
			}
			if len(e.Questions) == 1 {
				fmt.Fprintf(w, "    answer: amux inbox answer %s  (asks in a terminal) · --choice <label> · <text>\n", e.ID)
			} else {
				fmt.Fprintf(w, "    answer: amux inbox answer %s  (asks each question in a terminal) · --answers '[{\"choice\":[...]},{\"other\":\"...\"}]'\n", e.ID)
			}
		case queue.KindPermission:
			writeWrapped(w, "    ", e.Body, width)
			fmt.Fprintf(w, "    answer: amux inbox answer %s --allow | --deny\n", e.ID)
		case queue.KindGoalInput:
			if e.Body != e.Title {
				writeWrapped(w, "    ", e.Body, width)
			}
			fmt.Fprintf(w, "    answer: amux inbox answer %s <text>\n", e.ID)
		case queue.KindDecision:
			writeWrapped(w, "    ", e.Body, width)
			writeWrapped(w, "    suggested: ", e.Suggest, width)
			for _, pr := range e.PRs {
				fmt.Fprintf(w, "    pr: %s\n", oneLine(pr, 80))
			}
			fmt.Fprintf(w, "    item %s · answer: amux inbox answer %s <text> · amux inbox ack %s\n", e.Item, e.ID, e.Item)
		}
	}
	if len(s.Workgroups) > 0 {
		fmt.Fprintln(w, "\nworkgroups:")
		for _, g := range s.Workgroups {
			title := g.ID
			if g.Title != "" && g.Title != g.ID {
				title += " " + oneLine(g.Title, 40)
			}
			extra := ""
			if len(g.OpenPRs) > 0 {
				extra = " · open PRs " + strings.Join(g.OpenPRs, ", ")
			}
			if len(g.NeedsYou) > 0 {
				extra += " · needs you: " + strings.Join(g.NeedsYou, ", ")
			}
			fmt.Fprintf(w, "  %-9s %s%s\n", g.Bucket, title, extra)
		}
	}
	for _, e := range o.Errors {
		fmt.Fprintf(w, "\n(partial: %s)\n", e)
	}
}

// oneLine flattens control characters and newlines and bounds the length.
func oneLine(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
