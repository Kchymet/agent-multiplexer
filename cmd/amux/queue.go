package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"amux/internal/core"
	"amux/internal/items"
	"amux/internal/queue"
	"amux/internal/sessionrpc"
	"github.com/kchymet/agent-multiplexer/harnessproto"
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
	if fields[core.SteerDecision] == "" && len(text) == 0 && len(choices) == 0 {
		return nil, errors.New("an answer needs text, --choice <label>, or --allow/--deny")
	}
	return fields, nil
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
		if isFormAnswer(args[1:]) {
			return inboxFormAnswer(args[0], args[1:])
		}
		fields, err := queueAnswerFields(args[0], args[1:])
		if err != nil {
			return err
		}
		return queueHostAction(core.Action{Action: core.ActionQueueAnswer, Fields: fields}, "answered "+args[0])
	case "questions":
		return questionList(args, os.Stdout)
	case "show":
		return questionShow(args, os.Stdout)
	case "cancel":
		return inboxCancel(args)
	case "release":
		return inboxRelease(args)
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
// printed as plain text, one line per field, never interpreted.
func renderQueue(w io.Writer, o queue.Overview) {
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
		fmt.Fprintf(w, "    %s\n", oneLine(e.Title, 160))
		switch e.Kind {
		case queue.KindQuestion:
			for _, q := range e.Questions {
				label := q.Question
				if q.Header != "" {
					label = q.Header + ": " + q.Question
				}
				fmt.Fprintf(w, "    ? %s\n", oneLine(label, 160))
				for _, opt := range q.Options {
					fmt.Fprintf(w, "      - %s", oneLine(opt.Label, 60))
					if opt.Description != "" {
						fmt.Fprintf(w, "  %s", oneLine(opt.Description, 100))
					}
					fmt.Fprintln(w)
				}
			}
			if e.Form != nil && e.Form.Version == harnessproto.FormVersion {
				fmt.Fprintf(w, "    answer: amux inbox answer %s --select <label|n> ...  (amux inbox show %s for the whole form)\n", e.ID, e.ID)
			} else {
				fmt.Fprintf(w, "    answer: amux inbox answer %s --choice <label>  (or free text)\n", e.ID)
			}
		case queue.KindPermission:
			if e.Body != "" {
				fmt.Fprintf(w, "    %s\n", oneLine(e.Body, 200))
			}
			fmt.Fprintf(w, "    answer: amux inbox answer %s --allow | --deny\n", e.ID)
		case queue.KindGoalInput:
			fmt.Fprintf(w, "    answer: amux inbox answer %s <text>\n", e.ID)
		case queue.KindDecision:
			if e.Suggest != "" {
				fmt.Fprintf(w, "    suggested: %s\n", oneLine(e.Suggest, 160))
			}
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
