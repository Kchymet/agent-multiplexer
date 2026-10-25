package main

import (
	"amux/internal/centralauth"
	"amux/internal/daemon"
	"context"
	"encoding/json"
	"fmt"
	hp "github.com/kchymet/agent-multiplexer/harnessproto"
	"os"
	"os/signal"
	"strings"
	"time"
)

func accountClient() (*daemon.Client, error) {
	if sessionContextRestricted() {
		return daemon.DialConsoleAccounts()
	}
	return dial()
}

type accountService interface {
	Accounts(hp.AccountRequest, bool) (hp.AccountResult, error)
}

func checkedAccount(c accountService, r hp.AccountRequest, ceremony bool) (hp.AccountResult, error) {
	out, err := c.Accounts(r, ceremony)
	if err == nil && !out.OK {
		err = fmt.Errorf("%s", out.Error)
	}
	return out, err
}
func accountSnapshotClient(c *daemon.Client) (hp.AccountSnapshot, error) {
	out, err := checkedAccount(c, hp.AccountRequest{Operation: hp.AccountStatus}, false)
	if err != nil {
		return hp.AccountSnapshot{}, err
	}
	if out.Snapshot == nil {
		return hp.AccountSnapshot{}, fmt.Errorf("account status unavailable")
	}
	return *out.Snapshot, nil
}
func authAccounts(args []string) error {
	if len(args) > 1 || (len(args) == 1 && args[0] != "--json") {
		return fmt.Errorf("usage: amux auth accounts [--json]")
	}
	c, err := accountClient()
	if err != nil {
		return err
	}
	defer c.Close()
	s, err := accountSnapshotClient(c)
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	fmt.Println(string(b))
	return nil
}
func authAccountMutation(op string, args []string) error {
	if len(args) < 1 || len(args) > 2 || (op == hp.AccountRebind && len(args) != 2) || (op != hp.AccountRebind && len(args) != 1) {
		return fmt.Errorf("usage: amux auth %s <claude|codex> [workload-id]", op)
	}
	id := args[0]
	if id == "claude" || id == "codex" {
		id += ":central"
	}
	if id != "claude:central" && id != "codex:central" {
		return fmt.Errorf("choose claude or codex; legacy source bindings are preserved but are not selectable account configurations")
	}
	c, err := accountClient()
	if err != nil {
		return err
	}
	defer c.Close()
	s, err := accountSnapshotClient(c)
	if err != nil {
		return err
	}
	for _, p := range s.Profiles {
		if p.ID == id {
			r := hp.AccountRequest{Operation: op, ProfileID: id, Revision: p.Revision}
			if op == hp.AccountDefault {
				r.DefaultRevision = s.DefaultRevisions[p.Harness]
			}
			if len(args) == 2 {
				r.WorkloadID = args[1]
			}
			_, err = checkedAccount(c, r, false)
			if err == nil {
				fmt.Printf("%s: %s applied\n", id, op)
			}
			return err
		}
	}
	return fmt.Errorf("unknown account profile")
}
func authMachineLogin(harness string, fromStdin bool) error {
	c, err := accountClient()
	if err != nil {
		return err
	}
	defer c.Close()
	s, err := accountSnapshotClient(c)
	if err != nil {
		return err
	}
	var profile hp.AccountProfile
	for _, p := range s.Profiles {
		if p.ID == harness+":central" {
			profile = p
		}
	}
	initiator := "host-cli"
	out, err := checkedAccount(c, hp.AccountRequest{Operation: hp.AccountLogin, ProfileID: profile.ID, Revision: profile.Revision, Initiator: initiator}, true)
	if err != nil {
		return err
	}
	if out.Ceremony == nil {
		return fmt.Errorf("missing login ceremony")
	}
	attempt := out.Ceremony.AttemptID
	req := hp.AccountRequest{ProfileID: profile.ID, AttemptID: attempt, Initiator: initiator}
	if harness == "claude" {
		var raw string
		if fromStdin {
			raw, err = readTokenLine(os.Stdin)
		} else {
			if !isTerminal(os.Stdin) {
				return fmt.Errorf("login requires terminal or --token-stdin")
			}
			// Native setup-token prints the credential. In the console its
			// stdout may be captured as a session event, so only the host CLI
			// may run issuance. Console completion uses hidden input instead.
			if sessionContextRestricted() {
				fmt.Println("Obtain a setup-token in a trusted host terminal using claude setup-token.")
			} else if err = runClaudeSetupToken(); err != nil {
				return err
			}
			fmt.Print("\nPaste setup-token (input hidden): ")
			raw, err = readHiddenLine(os.Stdin)
			fmt.Println()
		}
		if err != nil {
			return err
		}
		err = completeClaudeToken(c, req, raw)
		if err == nil {
			fmt.Println("Central Claude login saved. Existing bound workloads keep their account; use explicit rebind after replacing an opaque token.")
		}
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	shown := ""
	for {
		req.Operation = hp.AccountPoll
		out, err = checkedAccount(c, req, true)
		if err != nil {
			return err
		}
		cer := out.Ceremony
		if cer == nil {
			return fmt.Errorf("missing login ceremony")
		}
		if cer.AuthURL != "" && shown != cer.AuthURL {
			fmt.Printf("Open this URL on a browser with access to the machine callback:\n%s\n", cer.AuthURL)
			shown = cer.AuthURL
		}
		switch cer.State {
		case "succeeded":
			fmt.Println("Central Codex login saved.")
			return nil
		case "failed", "cancelled":
			return fmt.Errorf("login %s", cer.State)
		}
		select {
		case <-ctx.Done():
			req.Operation = hp.AccountCancel
			_, _ = checkedAccount(c, req, true)
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
}

func authMachineStatus(harness string) error {
	c, err := accountClient()
	if err != nil {
		return err
	}
	defer c.Close()
	s, err := accountSnapshotClient(c)
	if err != nil {
		return err
	}
	for _, p := range s.Profiles {
		if p.ID != harness+":central" {
			continue
		}
		fmt.Printf("%s configuration: %s (%s)\nidentity: %s; plan: %s\nlimits: %s (%s)\n", harness, p.ID, p.State, p.Identity.State, p.Identity.Plan, p.Limits.State, p.Limits.Source)
		if s.Defaults[harness] != p.ID {
			fmt.Printf("New workloads still use a preserved legacy source; run amux auth default %s to select this configuration.\n", harness)
		}
		for _, w := range p.Limits.Windows {
			fresh := "current"
			if time.Now().UnixMilli()-p.Limits.ObservedAt > hp.AccountLimitsTTLMillis {
				fresh = "stale"
			}
			fmt.Printf("  %s: %.1f%% used; resets %d (%s)\n", w.Name, w.UsedPercent, w.ResetsAt, fresh)
		}
		if p.Error != "" {
			fmt.Println(p.Error)
		}
	}
	return nil
}

func completeClaudeToken(c accountService, r hp.AccountRequest, raw string) error {
	secret, err := centralauth.NewSecret(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("account_secret_invalid")
	}
	r.Operation = hp.AccountComplete
	r.Secret = secret.Reveal()
	_, err = checkedAccount(c, r, true)
	return err
}
