package main

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

	"amux/internal/centralauth"
	"amux/internal/sessionrpc"
)

// centralRetry is how often a launcher without a usable credential asks again.
var centralRetry = 5 * time.Second

// centralExec is the protected Claude launcher for central mode. It runs inside
// the session, after the payload trampoline, so the token is fetched only into
// this process and handed to Claude through its documented environment input.
// It is never placed in argv, the sandbox launcher environment, or a log.
func centralExec(argv []string, stderr io.Writer) int {
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
	token, err := fetchCentralToken()
	for warned := false; err != nil; token, err = fetchCentralToken() {
		if !warned {
			fmt.Fprintf(stderr, "amux: the central Claude credential is unavailable (%v).\n"+
				"Run `amux auth login` in a host terminal; this pane starts Claude as soon as it can.\n", err)
			warned = true
		}
		time.Sleep(centralRetry)
	}
	if err := syscall.Exec(bin, argv, centralEnv(os.Environ(), token)); err != nil {
		fmt.Fprintf(stderr, "amux: exec Claude: %v\n", err)
		return 126
	}
	return 0
}

// centralEnv replaces inherited Claude credential inputs with the central
// token, so a stale or alternate value can never shadow it.
func centralEnv(environ []string, token string) []string {
	out := make([]string, 0, len(environ)+1)
	for _, kv := range environ {
		key, _, _ := strings.Cut(kv, "=")
		switch key {
		case centralauth.ClaudeTokenEnv, "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN",
			"CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR", "CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR":
			continue
		}
		out = append(out, kv)
	}
	return append(out, centralauth.ClaudeTokenEnv+"="+token)
}

// fetchCentralToken asks the daemon for this session's credential. Errors are
// fixed RPC codes, never values.
func fetchCentralToken() (string, error) {
	client, err := openRestrictedSessionRPC()
	if err != nil {
		return "", errors.New("session mailbox unavailable")
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := client.Query(ctx, sessionrpc.Query{
		Verb: centralauth.QueryVerb, Fields: map[string]string{centralauth.HarnessField: string(centralauth.Claude)},
	})
	if err != nil {
		var remote *sessionrpc.RemoteError
		if errors.As(err, &remote) && remote.Code != "" {
			return "", errors.New(remote.Code)
		}
		return "", errors.New("daemon did not answer")
	}
	var d centralauth.Delivery
	if json.Unmarshal(result.Body, &d) != nil {
		return "", errors.New("malformed response")
	}
	if _, err := centralauth.NewSecret(d.Token); err != nil {
		return "", errors.New("malformed response")
	}
	return d.Token, nil
}
