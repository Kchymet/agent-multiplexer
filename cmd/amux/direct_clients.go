package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/user"
	"strings"
	"time"

	"amux/internal/direct"
)

func directScopeString(r direct.ResourceScope) string {
	if len(r.Workgroups) > 0 {
		return r.Kind + ":" + strings.Join(r.Workgroups, ",")
	}
	return r.Kind
}

// printDirectClients is the text form of clients list.
func printDirectClients(out io.Writer, serverID string, clients []direct.ClientInfo, stale bool) {
	fmt.Fprintf(out, "server_id %s\n", serverID)
	if stale && len(clients) == 0 {
		fmt.Fprintln(out, "(no stale clients)")
		return
	}
	for _, c := range clients {
		state := "active"
		if c.RevokedAt != 0 {
			state = "revoked (" + c.RevokeReason + ")"
		}
		if c.Origin == direct.OriginCloud {
			state += "  cloud:" + c.CloudRequester
		}
		if c.UpdatedAt != 0 {
			state += "  updated " + time.Unix(c.UpdatedAt, 0).Format(time.RFC3339)
			if n := len(c.Changes); n > 0 {
				last := c.Changes[n-1]
				by := last.Source
				if last.By != "" {
					by += ":" + last.By
				}
				state += " by " + by + " (was " + strings.Join(last.OldPerms, ",") + " " + directScopeString(last.OldResource) + ")"
			}
		}
		if c.Stale != "" {
			state += "  stale: " + c.Stale
		}
		fmt.Fprintf(out, "%s  %-20s %-8s %-10s %-24s %s\n", c.ClientID, c.Label, c.ClientKind,
			strings.Join(c.Permissions, ","), directScopeString(c.ResourceScope), state)
	}
	if stale {
		fmt.Fprintln(out, "Revoke the ones you no longer use: amux device revoke <client-id>")
	}
}

// directActor names the host-side caller in a grant's change record.
func directActor() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return "cli:" + u.Username
	}
	return "cli"
}

// runDirectClientsUpdate is `amux device ls update <client-id>
// [--permissions <list>] [--all-sessions | --workgroup <id>...] [--json]`.
func runDirectClientsUpdate(args []string, out io.Writer, admin func(direct.AdminRequest) (direct.AdminResponse, error)) error {
	const usage = "usage: amux device ls update <client-id> [--permissions <list>] [--all-sessions | --workgroup <root-id>...] [--json]"
	id := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		id, args = args[0], args[1:]
	}
	fs := newCmdFlagSet("device update")
	var groups multiFlag
	perms := fs.String("permissions", "", "")
	all := fs.Bool("all-sessions", false, "")
	asJSON := fs.Bool("json", false, "")
	fs.Var(&groups, "workgroup", "")
	if err := parseCmdFlags(fs, "device update", args); err != nil {
		return err
	}
	switch {
	case id == "" && fs.NArg() == 1:
		id = fs.Arg(0)
	case fs.NArg() != 0:
		return fmt.Errorf("unexpected argument %q\n%s", fs.Arg(0), usage)
	}
	if id == "" {
		return errors.New(usage)
	}
	u := direct.GrantUpdate{ClientID: id}
	if *perms != "" {
		for _, p := range strings.Split(*perms, ",") {
			u.Permissions = append(u.Permissions, strings.TrimSpace(p))
		}
	}
	switch {
	case *all && len(groups) > 0:
		return errors.New("choose --all-sessions or --workgroup, not both")
	case *all:
		u.Resource = &direct.ResourceScope{Kind: direct.ResourceAllSessions}
	case len(groups) > 0:
		u.Resource = &direct.ResourceScope{Kind: direct.ResourceWorkgroups, Workgroups: groups}
	}
	if u.Permissions == nil && u.Resource == nil {
		return fmt.Errorf("nothing to change: give --permissions, a scope, or both\n%s", usage)
	}
	resp, err := admin(direct.AdminRequest{Op: direct.OpUpdate, Update: &u, Actor: directActor()})
	if err != nil && resp.Update == nil {
		if strings.Contains(err.Error(), "unknown admin operation") {
			return errors.New("the running direct server predates grant updates; restart it (amux device disable && amux device enable) and retry")
		}
		return err
	}
	if resp.Update == nil {
		return errors.New("the direct server returned no update result")
	}
	r := *resp.Update
	if *asJSON {
		if jerr := json.NewEncoder(out).Encode(r); jerr != nil {
			return jerr
		}
		return err
	}
	c := r.Client
	if !r.Changed {
		fmt.Fprintf(out, "%s (%s) unchanged: %s %s\n", c.ClientID, c.Label, strings.Join(c.Permissions, ","), directScopeString(c.ResourceScope))
		return err
	}
	fmt.Fprintf(out, "%s (%s) updated:\n  was %s %s\n  now %s %s\n", c.ClientID, c.Label,
		strings.Join(r.OldPermissions, ","), directScopeString(r.OldResource),
		strings.Join(c.Permissions, ","), directScopeString(c.ResourceScope))
	if len(r.Dropped) > 0 {
		fmt.Fprintf(out, "  dropped %s: it needs --all-sessions\n", strings.Join(r.Dropped, ","))
	}
	if r.Narrowed {
		fmt.Fprintf(out, "Narrowing is in effect now; %d open stream(s) or call(s) were ended and reconnect under the new grant.\n", r.CutOperations)
	} else {
		fmt.Fprintln(out, "The new rights apply to the client's next request; nothing open was interrupted.")
	}
	return err
}
