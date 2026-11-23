package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"amux/internal/access"
	"amux/internal/core"
	"amux/internal/items"
	"amux/internal/sessionrpc"
)

// Upward signals (internal/items, docs/items.md): a session raises structured
// items — follow-ups, outstanding work, decisions it needs, risks, things left
// out of scope — at the end of a turn or of its work, instead of burying them
// in prose. A member's items go to its coordinator, everyone else's to the
// console; the coordinator triages and escalates. Every command acts as the
// caller (identity comes only from the fixed session context).

const agentRaiseUsage = `usage: amux agent raise --kind <kind> --title "<title>" [--detail "<text>" | --detail -]
                        [--pr <repo#n>]... [--link <url>]... [--suggest "<action>"] [--json]

  kinds: follow_up | outstanding | decision_needed | risk | fyi
  A member's item goes to its coordinator; a coordinator's (or any session
  outside a workgroup) to the console. Raising the same kind and title again
  while the item is still open updates it instead of adding a duplicate.`

const agentItemsUsage = `usage: amux agent items [--raised | --inbox] [--state <s>[,<s>...] | --all] [--limit n] [--json]
       amux agent items show <id> [--json]
       amux agent items ack|dismiss|done|reopen <id>... [--note "<text>"]

  Lists the open and acknowledged items you raised or receive (a coordinator
  also sees its workgroup's). --raised / --inbox narrow to one side; --all
  includes dismissed and done items.`

const agentEscalateUsage = `usage: amux agent escalate <id>... [--note "<why>"]

  A coordinator hands an item it received to the console (the user's inbox):
  for decisions that need the user, or work that crosses workgroups.`

// itemCall performs one item verb as the caller; a variable so tests can fake
// the daemon. route is access.RouteAction or access.RouteQuery.
var itemCall = func(route access.Route, verb, id string, fields map[string]string) ([]byte, error) {
	client, err := openRestrictedSessionRPC()
	if err != nil {
		return nil, fmt.Errorf("open fixed session control context: %w", err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	var result sessionrpc.Result
	if route == access.RouteQuery {
		result, err = client.Query(ctx, sessionrpc.Query{Verb: verb, ID: id, Fields: fields})
	} else {
		result, err = client.Action(ctx, sessionrpc.Action{Verb: verb, ID: id, Fields: fields})
	}
	if err != nil {
		var res core.Result
		if len(result.Body) != 0 && json.Unmarshal(result.Body, &res) == nil && res.Error != "" {
			return nil, errors.New(res.Error)
		}
		return nil, err
	}
	if len(result.Body) == 0 {
		return nil, fmt.Errorf("item operation returned no result")
	}
	return result.Body, nil
}

// itemSelf is the caller's own session id: every item verb acts as it.
func itemSelf(cmd string) (string, error) {
	context, err := loadAgentSessionContext()
	if err != nil || strings.TrimSpace(context.SubjectID) == "" {
		return "", fmt.Errorf("%s", notInsideAgent(cmd, "amux do raise-item <session-id> -f kind=… -f title=…"))
	}
	return context.SubjectID, nil
}

// itemAction runs an item action and decodes the item it returns.
func itemAction(verb, self string, fields map[string]string) (items.Item, error) {
	body, err := itemCall(access.RouteAction, verb, self, fields)
	if err != nil {
		return items.Item{}, err
	}
	var res core.Result
	if err := json.Unmarshal(body, &res); err != nil {
		return items.Item{}, fmt.Errorf("decode item result: %w", err)
	}
	if !res.OK {
		if res.Error == "" {
			res.Error = "item operation failed"
		}
		return items.Item{}, errors.New(res.Error)
	}
	var it items.Item
	if err := json.Unmarshal(res.Item, &it); err != nil {
		return items.Item{}, fmt.Errorf("decode item: %w", err)
	}
	return it, nil
}

// flagValue reads "--name value" or "--name=value" at args[i].
func flagValue(args []string, i int, name string) (value string, consumed int, ok bool, err error) {
	a := args[i]
	if a == name {
		if i+1 >= len(args) {
			return "", 0, true, fmt.Errorf("%s needs a value", name)
		}
		return args[i+1], 2, true, nil
	}
	if strings.HasPrefix(a, name+"=") {
		return strings.TrimPrefix(a, name+"="), 1, true, nil
	}
	return "", 0, false, nil
}

// itemStdin is where `--detail -` reads from (tests replace it).
var itemStdin io.Reader = os.Stdin

func cmdAgentRaise(args []string) error {
	fields := map[string]string{}
	var prs, links []string
	asJSON := false
	for i := 0; i < len(args); {
		a := args[i]
		if a == "--json" {
			asJSON = true
			i++
			continue
		}
		if a == "-h" || a == "--help" {
			fmt.Fprintln(os.Stderr, agentRaiseUsage)
			return nil
		}
		matched := false
		for _, f := range []struct{ flag, field string }{
			{"--kind", core.ItemKindField}, {"--title", core.ItemTitleField}, {"--detail", core.ItemDetailField},
			{"--suggest", core.ItemSuggestField}, {"--pr", core.ItemPRsField}, {"--link", core.ItemLinksField},
		} {
			v, n, ok, err := flagValue(args, i, f.flag)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			matched = true
			i += n
			switch f.field {
			case core.ItemPRsField:
				prs = append(prs, strings.Split(v, ",")...)
			case core.ItemLinksField:
				links = append(links, v)
			case core.ItemDetailField:
				if v == "-" {
					b, err := io.ReadAll(io.LimitReader(itemStdin, 64<<10))
					if err != nil {
						return fmt.Errorf("read detail from stdin: %w", err)
					}
					v = string(b)
				}
				fields[f.field] = v
			default:
				fields[f.field] = v
			}
			break
		}
		if !matched {
			return fmt.Errorf("unknown raise argument %q\n%s", a, agentRaiseUsage)
		}
	}
	if fields[core.ItemKindField] == "" || strings.TrimSpace(fields[core.ItemTitleField]) == "" {
		return fmt.Errorf("--kind and --title are required\n%s", agentRaiseUsage)
	}
	if !items.KnownKind(fields[core.ItemKindField]) {
		return fmt.Errorf("unknown kind %q (want one of %s)", fields[core.ItemKindField], strings.Join(items.Kinds(), ", "))
	}
	for _, l := range links {
		if strings.ContainsAny(l, "\n\r") {
			return fmt.Errorf("invalid link %q", l)
		}
	}
	if len(prs) > 0 {
		fields[core.ItemPRsField] = strings.Join(prs, ",")
	}
	if len(links) > 0 {
		fields[core.ItemLinksField] = strings.Join(links, "\n")
	}
	self, err := itemSelf("amux agent raise")
	if err != nil {
		return err
	}
	it, err := itemAction(core.ActionRaiseItem, self, fields)
	if err != nil {
		return fmt.Errorf("amux agent raise: %w", err)
	}
	if asJSON {
		return printJSON(it)
	}
	verb := "raised"
	if it.Raised > 1 {
		verb = fmt.Sprintf("updated (raised %d times)", it.Raised)
	}
	fmt.Printf("%s %s [%s] → %s\n", verb, it.ID, it.Kind, recipientLabel(it.Recipient))
	return nil
}

func recipientLabel(r string) string {
	if r == items.ConsoleRecipient {
		return "the console"
	}
	return "coordinator " + r
}

func cmdAgentItems(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "show":
			return cmdAgentItemShow(args[1:])
		case items.VerbAck, items.VerbDismiss, items.VerbDone, items.VerbReopen:
			return cmdAgentItemState(args[0], args[1:])
		case "-h", "--help", "help":
			fmt.Fprintln(os.Stderr, agentItemsUsage)
			return nil
		}
	}
	fields := map[string]string{}
	asJSON := false
	for i := 0; i < len(args); {
		a := args[i]
		switch a {
		case "--json":
			asJSON = true
			i++
			continue
		case "--raised":
			fields[core.ItemBoxField] = core.ItemBoxRaised
			i++
			continue
		case "--inbox":
			fields[core.ItemBoxField] = core.ItemBoxInbox
			i++
			continue
		case "--all":
			fields[core.ItemStatesField] = "all"
			i++
			continue
		}
		if v, n, ok, err := flagValue(args, i, "--state"); ok {
			if err != nil {
				return err
			}
			fields[core.ItemStatesField] = v
			i += n
			continue
		}
		if v, n, ok, err := flagValue(args, i, "--limit"); ok {
			if err != nil {
				return err
			}
			if _, err := strconv.Atoi(v); err != nil {
				return fmt.Errorf("--limit must be a number")
			}
			fields[core.ItemLimitField] = v
			i += n
			continue
		}
		return fmt.Errorf("unknown items argument %q\n%s", a, agentItemsUsage)
	}
	self, err := itemSelf("amux agent items")
	if err != nil {
		return err
	}
	body, err := itemCall(access.RouteQuery, core.QueryItems, self, fields)
	if err != nil {
		return fmt.Errorf("amux agent items: %w", err)
	}
	var list items.List
	if err := json.Unmarshal(body, &list); err != nil {
		return fmt.Errorf("amux agent items: decode: %w", err)
	}
	if asJSON {
		return printJSON(list)
	}
	printItemTable(os.Stdout, self, list)
	return nil
}

func printItemTable(w io.Writer, self string, list items.List) {
	if len(list.Items) == 0 {
		fmt.Fprintln(w, "no items")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATE\tKIND\tFROM → TO\tTITLE")
	for _, it := range list.Items {
		from, to := it.Source, it.Recipient
		if from == self {
			from = "you"
		}
		if to == self {
			to = "you"
		}
		title := it.Title
		if r := []rune(title); len(r) > 72 {
			title = string(r[:72]) + "…"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s → %s\t%s\n", it.ID, it.State, it.Kind, from, to, title)
	}
	tw.Flush()
	if list.Truncated {
		fmt.Fprintln(w, "(more items; narrow with --raised, --inbox, --state or --limit)")
	}
}

func cmdAgentItemShow(args []string) error {
	asJSON := false
	var id string
	for _, a := range args {
		switch {
		case a == "--json":
			asJSON = true
		case strings.HasPrefix(a, "-"):
			return fmt.Errorf("unknown show option %q\n%s", a, agentItemsUsage)
		case id == "":
			id = a
		default:
			return fmt.Errorf("%s", agentItemsUsage)
		}
	}
	if id == "" {
		return fmt.Errorf("%s", agentItemsUsage)
	}
	self, err := itemSelf("amux agent items show")
	if err != nil {
		return err
	}
	body, err := itemCall(access.RouteQuery, core.QueryItems, self, map[string]string{core.ItemIDField: id})
	if err != nil {
		return fmt.Errorf("amux agent items show: %w", err)
	}
	var list items.List
	if err := json.Unmarshal(body, &list); err != nil || len(list.Items) != 1 {
		return fmt.Errorf("amux agent items show: unexpected response")
	}
	it := list.Items[0]
	if asJSON {
		return printJSON(it)
	}
	printItem(os.Stdout, it)
	return nil
}

func printItem(w io.Writer, it items.Item) {
	fmt.Fprintf(w, "%s  [%s]  %s\n", it.ID, it.Kind, it.State)
	fmt.Fprintf(w, "title:   %s\n", it.Title)
	fmt.Fprintf(w, "from:    %s", it.Source)
	if it.Workgroup != "" && it.Workgroup != it.Source {
		fmt.Fprintf(w, " (workgroup %s)", it.Workgroup)
	}
	fmt.Fprintf(w, "\nto:      %s\n", recipientLabel(it.Recipient))
	if it.Raised > 1 {
		fmt.Fprintf(w, "raised:  %d times\n", it.Raised)
	}
	for _, pr := range it.PRs {
		fmt.Fprintf(w, "pr:      %s\n", pr)
	}
	for _, l := range it.Links {
		fmt.Fprintf(w, "link:    %s\n", l)
	}
	if it.Suggest != "" {
		fmt.Fprintf(w, "suggest: %s\n", it.Suggest)
	}
	if it.Detail != "" {
		fmt.Fprintf(w, "\n%s\n", it.Detail)
	}
	if len(it.History) > 0 {
		fmt.Fprintln(w, "\nhistory:")
		for _, ev := range it.History {
			line := time.UnixMilli(ev.At).Format("2006-01-02 15:04") + "  " + ev.Action
			if ev.By != "" {
				line += " by " + ev.By
			}
			if ev.From != "" || ev.To != "" {
				line += fmt.Sprintf(" (%s → %s)", orDash(ev.From), orDash(ev.To))
			}
			if ev.Note != "" {
				line += ": " + ev.Note
			}
			fmt.Fprintln(w, "  "+line)
		}
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// itemTargets parses "<id>... [--note text]".
func itemTargets(args []string, usage string) (ids []string, note string, err error) {
	for i := 0; i < len(args); {
		if v, n, ok, ferr := flagValue(args, i, "--note"); ok {
			if ferr != nil {
				return nil, "", ferr
			}
			note = v
			i += n
			continue
		}
		if strings.HasPrefix(args[i], "-") {
			return nil, "", fmt.Errorf("unknown option %q\n%s", args[i], usage)
		}
		ids = append(ids, args[i])
		i++
	}
	if len(ids) == 0 {
		return nil, "", fmt.Errorf("%s", usage)
	}
	return ids, note, nil
}

func cmdAgentItemState(verb string, args []string) error {
	ids, note, err := itemTargets(args, agentItemsUsage)
	if err != nil {
		return err
	}
	self, err := itemSelf("amux agent items " + verb)
	if err != nil {
		return err
	}
	var failed []string
	for _, id := range ids {
		fields := map[string]string{core.ItemIDField: id, core.ItemStateField: verb}
		if note != "" {
			fields[core.ItemNoteField] = note
		}
		it, err := itemAction(core.ActionItemState, self, fields)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", id, err)
			failed = append(failed, id)
			continue
		}
		fmt.Printf("%s: %s\n", it.ID, it.State)
	}
	if len(failed) > 0 {
		return fmt.Errorf("amux agent items %s: failed for %s", verb, strings.Join(failed, ", "))
	}
	return nil
}

func cmdAgentEscalate(args []string) error {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprintln(os.Stderr, agentEscalateUsage)
		return nil
	}
	ids, note, err := itemTargets(args, agentEscalateUsage)
	if err != nil {
		return err
	}
	self, err := itemSelf("amux agent escalate")
	if err != nil {
		return err
	}
	var failed []string
	for _, id := range ids {
		fields := map[string]string{core.ItemIDField: id}
		if note != "" {
			fields[core.ItemNoteField] = note
		}
		it, err := itemAction(core.ActionEscalateItem, self, fields)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", id, err)
			failed = append(failed, id)
			continue
		}
		fmt.Printf("%s: escalated → %s\n", it.ID, recipientLabel(it.Recipient))
	}
	if len(failed) > 0 {
		return fmt.Errorf("amux agent escalate: failed for %s", strings.Join(failed, ", "))
	}
	return nil
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
