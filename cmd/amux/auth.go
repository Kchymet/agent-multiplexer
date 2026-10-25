package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
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
		return fmt.Errorf("unknown auth subcommand %q; want login, status, accounts, default, logout, rebind, or restart", args[0])
	}
	return run(args[1:])
}

var authSubcommands = map[string]func([]string) error{
	"login":    authLogin,
	"accounts": authAccounts,
	"logout":   func(a []string) error { return authAccountMutation("logout", a) },
	"default":  func(a []string) error { return authAccountMutation("default", a) },
	"rebind":   func(a []string) error { return authAccountMutation("rebind", a) },
	"status":   authStatus,
	"restart":  authRestart,
}

func authUsage() {
	fmt.Fprint(os.Stderr, `amux auth — central harness logins, delivered to sessions by the daemon

usage: amux auth <command>

  login [claude] [--token-stdin]  create or replace the central Claude token
  login codex                     sign the central Codex home in to ChatGPT
  status [claude|codex]           show the amux-owned configuration and limits
  accounts [--json]             view Claude/Codex configuration and limits
  logout <claude|codex>          fence delivery and clear the central login
  default <claude|codex>         use the amux configuration for new workloads
  rebind <claude|codex> <id>      explicitly bind and stop one workload
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

One amux-owned configuration is available for each harness. Legacy source
bindings remain internal for compatibility. Configuration uses amux CLI/TUI;
Harness web account views are read-only.

Defaults affect newly created workloads. Existing workloads keep their binding;
use explicit rebind after replacing an opaque Claude credential. Login pauses
new credential delivery until completion or cancellation.
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
	if sessionContextRestricted() {
		return fmt.Errorf("machine account status requires a host terminal")
	}
	harness, rest := authHarness(args)
	if len(rest) != 0 {
		return fmt.Errorf("usage: amux auth status [claude|codex]")
	}
	if c, err := startupDial(); err == nil {
		c.Close()
		return authMachineStatus(harness)
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
		return authMachineLogin("codex", false)
	}
	stdin := false
	switch {
	case len(rest) == 1 && rest[0] == "--token-stdin":
		stdin = true
	case len(rest) != 0:
		return fmt.Errorf("usage: amux auth login [claude] [--token-stdin]")
	}
	return authMachineLogin("claude", stdin)
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
