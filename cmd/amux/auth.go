package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"
	"time"

	"amux/internal/agent"
	"amux/internal/amuxcfg"
	"amux/internal/centralauth"
	"amux/internal/cfghome"
	"amux/internal/claudecfg"
	"amux/internal/core"
)

func cmdAuth(args []string) error {
	if len(args) == 0 || (len(args) == 1 && isHelpFlag(args[0])) {
		authUsage()
		return nil
	}
	run, ok := authSubcommands[args[0]]
	if !ok {
		return fmt.Errorf("unknown auth subcommand %q; want login, status, or restart", args[0])
	}
	return run(args[1:])
}

var authSubcommands = map[string]func([]string) error{
	"login":   authLogin,
	"status":  authStatus,
	"restart": authRestart,
}

func authUsage() {
	fmt.Fprint(os.Stderr, `amux auth — central harness logins, delivered to sessions by the daemon

usage: amux auth <command>

  login [claude] [--token-stdin]  create or replace the central Claude token
  login codex                     sign the central Codex home in to ChatGPT
  status [claude|codex]           show the selected mode and central login
  restart [--force]               restart idle Claude agents with current credentials

login runs `+"`claude setup-token`"+` (browser sign-in) and then asks you to paste
the token it prints, with input hidden; --token-stdin reads it from stdin
instead. Codex sign-in prints a browser URL; the daemon owns that login.
restart --force also interrupts busy or unknown-state agents. Conversations
resume after a restart.

Run logins in a host terminal. Sessions never hold a refresh token: Claude
receives the setup-token through CLAUDE_CODE_OAUTH_TOKEN, and Codex receives
short-lived ChatGPT tokens from the daemon. A setup-token is valid for one
year and is not refreshed; run login again to reissue it before then.
Reissuing does not revoke the previous token. Status never prints a secret.

Other methods remain available as explicit selections:
  amux config set claude.auth host|shared|environment
  amux config set codex.auth host|environment
`)
}

func authRestart(args []string) error {
	if len(args) > 1 || (len(args) == 1 && args[0] != "--force") {
		return fmt.Errorf("usage: amux auth restart [--force]")
	}
	return reloadClaudeAuth(len(args) == 1)
}

// authHarness parses an optional leading harness word.
func authHarness(args []string) (string, []string) {
	if len(args) > 0 && (args[0] == "claude" || args[0] == "codex") {
		return args[0], args[1:]
	}
	return "claude", args
}

func authStatus(args []string) error {
	harness, rest := authHarness(args)
	if len(rest) != 0 {
		return fmt.Errorf("usage: amux auth status [claude|codex]")
	}
	if harness == "codex" {
		return codexAuthStatus()
	}
	return claudeAuthStatus()
}

func authLogin(args []string) error {
	harness, rest := authHarness(args)
	if sessionContextRestricted() {
		return fmt.Errorf("amux auth login must run in a host terminal, not inside a session")
	}
	if harness == "codex" {
		if len(rest) != 0 {
			return fmt.Errorf("usage: amux auth login codex")
		}
		return codexLogin()
	}
	stdin := false
	switch {
	case len(rest) == 1 && rest[0] == "--token-stdin":
		stdin = true
	case len(rest) != 0:
		return fmt.Errorf("usage: amux auth login [claude] [--token-stdin]")
	}
	return claudeLogin(stdin, os.Stdin, os.Stdout)
}

// claudeAuthStatus reports the selected Claude mode. Central mode is described
// from the store's metadata; legacy modes ask Claude about the store they use.
func claudeAuthStatus() error {
	mode, err := amuxcfg.AuthMode("claude")
	if err != nil {
		return err
	}
	saved, _ := amuxcfg.SavedAuthMode("claude")
	source := "default"
	if saved != "" {
		source = "saved"
	}
	fmt.Printf("claude.auth: %s (%s)\n", mode, source)
	switch mode {
	case amuxcfg.AuthCentral:
		st, err := centralauth.ClaudeStore().Status()
		if err != nil {
			return fmt.Errorf("central Claude credential unusable: %w", err)
		}
		if !st.Configured {
			fmt.Println("central login: none — run `amux auth login` to create one for all sessions")
			return nil
		}
		fmt.Printf("central login: setup-token %s, stored %s, generation %d\n",
			st.Fingerprint, st.StoredAt.Local().Format(time.DateTime), st.Generation)
		fmt.Println("lifetime: Claude documents setup-token tokens as valid for one year; amux does not refresh them")
		user := claudecfg.User()
		// A spec that names only the template: status has no session, so the
		// user's own settings are the layer every new session starts from.
		sp := cfghome.Spec{Template: user.Dir, Dir: user.Dir}
		if conflicts := claudecfg.CentralConflicts(sp, "", os.Environ()); len(conflicts) > 0 {
			fmt.Printf("warning: %s would override the central token in sessions (this shell's environment is shown; the daemon's may differ)\n",
				strings.Join(conflicts, ", "))
		}
		return nil
	case amuxcfg.AuthEnvironment:
		fmt.Println("credentials: from the inherited environment and Claude settings (API key, apiKeyHelper, cloud provider or gateway); amux provides none")
		return nil
	}
	return runClaudeAuth("status")
}

// claudeLogin acquires a setup-token on the host and stores it centrally.
func claudeLogin(fromStdin bool, in io.Reader, out io.Writer) error {
	store := centralauth.ClaudeStore()
	release, err := store.LockLogin()
	if err != nil {
		return err
	}
	defer release()
	var raw string
	if fromStdin {
		raw, err = readTokenLine(in)
	} else {
		if !isTerminal(os.Stdin) {
			return fmt.Errorf("amux auth login needs a terminal; use --token-stdin to provide a token from `claude setup-token`")
		}
		if err := runClaudeSetupToken(); err != nil {
			return err
		}
		fmt.Fprint(out, "\nPaste the token printed above (input is hidden): ")
		raw, err = readHiddenLine(os.Stdin)
		fmt.Fprintln(out)
	}
	if err != nil {
		return err
	}
	token, err := centralauth.NewSecret(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("that is not a Claude OAuth token (empty, too long, or contains spaces); nothing was saved")
	}
	cred, err := store.Save(token, time.Now())
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Central Claude login saved (%s, generation %d).\n", token.Fingerprint(), cred.Generation)
	if cred.Generation > 1 {
		fmt.Fprintln(out, "The previous token was replaced but not revoked; it stays valid until it expires or is revoked in your Claude account.")
	}
	if mode, err := amuxcfg.AuthMode("claude"); err == nil && mode != amuxcfg.AuthCentral {
		fmt.Fprintf(out, "claude.auth is %s, so sessions keep using it; run `amux config unset claude.auth` to use the central login.\n", mode)
		return nil
	}
	if err := reloadClaudeAuth(false); err != nil {
		return fmt.Errorf("login saved, but running sessions could not be queued: %w; run amux auth restart after updating the daemon", err)
	}
	return nil
}

func readTokenLine(in io.Reader) (string, error) {
	line, err := bufio.NewReader(io.LimitReader(in, 16<<10)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return line, nil
}

// runClaudeSetupToken runs the host Claude's documented token flow with the
// terminal attached. Credential overrides are removed so the command cannot
// report or reuse an unrelated API key or inherited token.
func runClaudeSetupToken() error {
	argv, err := agent.Argv("claude", "")
	if err != nil {
		return err
	}
	cmd := exec.Command(argv[0], "setup-token")
	cmd.Env = withoutCredentialOverrides(os.Environ())
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("claude setup-token failed: %w", err)
	}
	return nil
}

func withoutCredentialOverrides(env []string) []string {
	out := env[:0:0]
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		if slices.Contains([]string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", centralauth.ClaudeTokenEnv,
			"CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR", "CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR", "CLAUDECODE"}, key) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// runClaudeAuth runs `claude auth <verb>` against the store a legacy mode
// selects: the older amux shared directory, or the host account otherwise.
func runClaudeAuth(verb string) error {
	// Resolve the installed Claude/wrapper exactly as agent launches do, but do
	// not pass model or permission flags to a CLI auth subcommand.
	argv, err := agent.Argv("claude", "")
	if err != nil {
		return err
	}
	cmd := exec.Command(argv[0], "auth", verb)
	if verb == "login" || claudecfg.AuthMode() == amuxcfg.AuthShared && claudecfg.SharedAuthEnabled() {
		cmd.Dir = claudecfg.SharedAuthDir()
		cmd.Env = claudecfg.AuthCommandEnv(os.Environ())
	} else {
		// Inspect the host account that host-mode sessions inherit, including
		// its environment credential overrides.
		cmd.Env = os.Environ()
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("Claude auth %s failed: %w", verb, err)
	}
	return nil
}

func reloadClaudeAuth(force bool) error {
	fields := map[string]string{}
	if force {
		fields["force"] = "true"
	}
	if err := sendAction(core.Action{Action: core.ActionAuthReload, Fields: fields}); err != nil {
		return err
	}
	fmt.Println("Claude agents queued to restart with the active login (conversations resume); busy and unknown-state agents wait unless --force is set.")
	return nil
}

// codexLogin drives the daemon-owned central Codex login: the daemon holds the
// central home exclusively, so the CLI only starts the flow and shows its URL.
func codexLogin() error {
	if err := sendAction(core.Action{Action: core.ActionCodexLogin}); err != nil {
		return err
	}
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	defer signal.Stop(interrupt)
	shown := ""
	for {
		var st core.CodexAuthStatus
		if err := queryRows(core.QueryCodexAuth, &st); err != nil {
			return err
		}
		if st.Login.URL != "" && st.Login.URL != shown {
			fmt.Printf("Open this URL in a browser to sign the central Codex home in to ChatGPT:\n\n  %s\n\nWaiting for the sign-in to finish (Ctrl-C cancels)...\n", st.Login.URL)
			shown = st.Login.URL
		}
		switch st.Login.State {
		case "succeeded":
			fmt.Println("Central Codex login saved. New Codex sessions use it; restart running Codex sessions (amux do restart <id>) to switch them.")
			return nil
		case "failed":
			return fmt.Errorf("central Codex login failed: %s", st.Login.Error)
		case "cancelled":
			return fmt.Errorf("central Codex login cancelled")
		case "idle":
			if st.Error != "" {
				return fmt.Errorf("central Codex login unavailable: %s", st.Error)
			}
		}
		select {
		case <-interrupt:
			_ = sendAction(core.Action{Action: core.ActionCodexLogin, Fields: map[string]string{"cancel": "true"}})
			return fmt.Errorf("central Codex login cancelled")
		case <-time.After(time.Second):
		}
	}
}

func codexAuthStatus() error {
	var st core.CodexAuthStatus
	if err := queryRows(core.QueryCodexAuth, &st); err != nil {
		return err
	}
	saved, _ := amuxcfg.SavedAuthMode("codex")
	source := "default"
	if saved != "" {
		source = "saved"
	}
	fmt.Printf("codex.auth: %s (%s)\n", st.Mode, source)
	if st.Mode != amuxcfg.AuthCentral {
		return nil
	}
	switch {
	case st.Login.State == "pending":
		fmt.Println("central login: sign-in in progress")
	case st.Error != "":
		fmt.Printf("central login: unavailable — %s\n", st.Error)
	case st.Account.Type == "chatgpt":
		plan := ""
		if st.Account.PlanType != "" {
			plan = " (" + st.Account.PlanType + ")"
		}
		fmt.Printf("central login: ChatGPT %s%s\n", st.Account.Email, plan)
	case st.Account.Type == "":
		fmt.Println("central login: none — run `amux auth login codex`")
	default:
		fmt.Printf("central login: %s (central mode needs a ChatGPT login; run `amux auth login codex`)\n", st.Account.Type)
	}
	return nil
}
