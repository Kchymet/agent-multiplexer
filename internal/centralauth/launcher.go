package centralauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"amux/internal/sessionrpc"
)

// LaunchRetry is how often a launcher without a usable credential asks again.
var LaunchRetry = 5 * time.Second

// Launch is the in-session Claude launcher for central mode. The token is
// fetched into this process only and handed to Claude through its documented
// CLAUDE_CODE_OAUTH_TOKEN input; it is never placed in argv, the sandbox
// launcher environment, or a log. Without a credential it explains what to do
// once and keeps asking, so the pane starts Claude as soon as a host login
// exists instead of exiting into a restart loop. It returns only on failure.
func Launch(argv []string, stderr io.Writer, fetch func() (Secret, error)) int {
	if len(argv) == 0 {
		fmt.Fprintln(stderr, "amux: central Claude launcher needs a command")
		return 126
	}
	bin := argv[0]
	if !strings.ContainsRune(bin, os.PathSeparator) {
		var err error
		if bin, err = exec.LookPath(bin); err != nil {
			fmt.Fprintf(stderr, "amux: resolve Claude: %v\n", err)
			return 126
		}
	}
	token, err := fetch()
	for warned := false; err != nil; token, err = fetch() {
		if !warned {
			fmt.Fprintf(stderr, "amux: the central Claude credential is unavailable (%v).\n"+
				"Run `amux auth login` in a host terminal; this pane starts Claude as soon as it can.\n", err)
			warned = true
		}
		time.Sleep(LaunchRetry)
	}
	if err := syscall.Exec(bin, argv, LaunchEnv(os.Environ(), token)); err != nil {
		fmt.Fprintf(stderr, "amux: exec Claude: %v\n", err)
	}
	return 126
}

// LaunchEnv replaces inherited Claude credential inputs with the central
// token, so a stale or alternate value can never shadow it.
func LaunchEnv(environ []string, token Secret) []string {
	out := make([]string, 0, len(environ)+1)
	for _, kv := range environ {
		key, _, _ := strings.Cut(kv, "=")
		switch key {
		case ClaudeTokenEnv, "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN",
			"CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR", "CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR":
			continue
		}
		out = append(out, kv)
	}
	return append(out, ClaudeTokenEnv+"="+token.Reveal())
}

// sessionQuerier is the slice of the session RPC client Fetch uses.
type sessionQuerier interface {
	Query(context.Context, sessionrpc.Query) (sessionrpc.Result, error)
	Close() error
}

// openSession opens this session's fixed signed mailbox context.
var openSession = func() (sessionQuerier, error) { return sessionrpc.OpenClient() }

// FetchFromSession asks the daemon for this session's Claude credential over
// the signed mailbox. Errors are fixed RPC codes, never values or paths.
func FetchFromSession() (Secret, error) {
	client, err := openSession()
	if err != nil {
		return Secret{}, errors.New("session mailbox unavailable")
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := client.Query(ctx, sessionrpc.Query{
		Verb: QueryVerb, Fields: map[string]string{HarnessField: string(Claude)},
	})
	if err != nil {
		var remote *sessionrpc.RemoteError
		if errors.As(err, &remote) && remote.Code != "" {
			return Secret{}, errors.New(remote.Code)
		}
		return Secret{}, errors.New("daemon did not answer")
	}
	var d Delivery
	if json.Unmarshal(result.Body, &d) != nil {
		return Secret{}, errors.New("malformed response")
	}
	token, err := NewSecret(d.Token)
	if err != nil {
		return Secret{}, errors.New("malformed response")
	}
	return token, nil
}
