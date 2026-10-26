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

// fetchAttempt bounds one credential request, including waiting for the daemon
// to publish a just-provisioned mailbox. publishPoll paces that wait.
var (
	fetchAttempt = 10 * time.Second
	publishPoll  = 50 * time.Millisecond
)

// Launch is the in-session Claude launcher for central mode. The token is
// fetched into this process only and handed to Claude through its documented
// CLAUDE_CODE_OAUTH_TOKEN input; it is never placed in argv, the sandbox
// launcher environment, or a log. Without a credential it explains what to do
// once and keeps asking, so the pane starts Claude as soon as a host login
// exists instead of exiting into a restart loop. It returns only on failure.
func Launch(argv []string, stderr io.Writer, fetch func() (Secret, error)) int {
	return LaunchCredential(argv, stderr, func() (Credential, error) {
		t, e := fetch()
		return Credential{Kind: KindClaudeOAuthToken, Token: t}, e
	})
}

// LaunchCredential retains classified delivery guidance for either auth mode.
func LaunchCredential(argv []string, stderr io.Writer, fetch func() (Credential, error)) int {
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
	for previous := ""; err != nil; token, err = fetch() {
		message := launchGuidance(err)
		if message != previous {
			fmt.Fprintln(stderr, "amux: "+message+"\nWaiting for authorized credential delivery; retrying automatically.")
			previous = message
		}
		time.Sleep(LaunchRetry)
	}
	if err := syscall.Exec(bin, argv, LaunchCredentialEnv(os.Environ(), token)); err != nil {
		fmt.Fprintf(stderr, "amux: exec Claude: %v\n", err)
	}
	return 126
}

// fetchFailure contains only a local classification, never native error text,
// response bodies, paths or arbitrary remote codes. A transport failure does
// not establish whether a credential exists.
type fetchFailure string

func (e fetchFailure) Error() string { return string(e) }

const (
	fetchMissing     fetchFailure = "central_login_required"
	fetchUnavailable fetchFailure = "central_credential_unavailable"
	fetchDenied      fetchFailure = "credential_access_denied"
	fetchBlocked     fetchFailure = "account_binding_blocked"
	fetchConflict    fetchFailure = "central_credential_conflict"
	fetchSuperseded  fetchFailure = "account_runtime_superseded"
	fetchChanged     fetchFailure = "account_revision_changed"
	fetchMailbox     fetchFailure = "session mailbox unavailable"
	fetchUnpublished fetchFailure = "session mailbox not published"
	fetchTimeout     fetchFailure = "session mailbox request timed out"
	fetchTransport   fetchFailure = "session mailbox request failed"
	fetchMalformed   fetchFailure = "malformed credential response"
)

func launchGuidance(err error) string {
	var failure fetchFailure
	if !errors.As(err, &failure) {
		failure = fetchTransport
	}
	switch failure {
	case fetchMissing:
		return "No central Claude login is configured. Run `amux auth login claude` in a trusted host terminal."
	case fetchDenied:
		return "Claude credential access was denied. Check this workload's authorization in amux; logging in does not restore revoked access."
	case fetchBlocked:
		return "Central Claude delivery is paused (login pending, logged out, or blocked). Check Accounts in amux; this workload uses the central login once it is ready."
	case fetchSuperseded:
		return "The central Claude login changed after this process was launched. A running process never switches logins; restart the workload to use the current login."
	case fetchChanged:
		return "Account metadata changed during delivery. Retrying with the current central login."
	case fetchConflict:
		return "Claude settings conflict with central credential delivery. Check provider and credential overrides for this workload."
	case fetchUnavailable:
		return "The daemon could not read the central Claude credential. Check Accounts in amux; this does not establish that login is missing."
	case fetchMailbox:
		return "The protected session mailbox is unavailable. Check the workload's session access and daemon status from a trusted host terminal."
	case fetchUnpublished:
		return "The daemon has not published this session's protected mailbox after 10 seconds. Credential status is unknown; check daemon status from a trusted host terminal."
	case fetchTimeout:
		return "The daemon did not answer the protected session mailbox request within 10 seconds. Credential status is unknown; check daemon status from a trusted host terminal."
	case fetchMalformed:
		return "The daemon returned an invalid credential response. Check that the daemon and amux launcher versions match."
	default:
		return "The protected session mailbox request failed. Credential status is unknown; check daemon status and workload access from a trusted host terminal."
	}
}

// LaunchEnv replaces inherited Claude credential inputs with the central
// token, so a stale or alternate value can never shadow it.
func LaunchEnv(environ []string, token Secret) []string {
	return LaunchCredentialEnv(environ, Credential{Kind: KindClaudeOAuthToken, Token: token})
}

func LaunchCredentialEnv(environ []string, credential Credential) []string {
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
	key := ClaudeTokenEnv
	if credential.Kind == KindClaudeAPIKey {
		key = "ANTHROPIC_API_KEY"
	}
	return append(out, key+"="+credential.Token.Reveal())
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
	c, e := fetchCredentialFromSession(false, "")
	return c.Token, e
}

func FetchCredentialFromSession() (Credential, error) { return fetchCredentialFromSession(true, "") }

// FetchLaunchCredentialFromSession requests the credential for one process
// launch. Repeated requests for it receive the same credential version.
func FetchLaunchCredentialFromSession(launch string) func() (Credential, error) {
	return func() (Credential, error) { return fetchCredentialFromSession(true, launch) }
}

func fetchCredentialFromSession(typed bool, launch string) (Credential, error) {
	client, err := openSession()
	if err != nil {
		return Credential{}, fetchMailbox
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), fetchAttempt)
	defer cancel()
	fields := map[string]string{HarnessField: string(Claude)}
	if typed {
		fields[FormatField] = TypedFormat
	}
	if launch != "" {
		fields[LaunchField] = launch
	}
	result, err := client.Query(ctx, sessionrpc.Query{Verb: QueryVerb, Fields: fields})
	// The launcher starts while the action that provisioned this session still
	// holds the daemon's admission locks, so its mailbox can be published a few
	// milliseconds later. Nothing was sent; wait for publication within this
	// attempt instead of failing it and sleeping a full LaunchRetry.
	for errors.Is(err, sessionrpc.ErrUnpublished) {
		select {
		case <-ctx.Done():
			return Credential{}, fetchUnpublished
		case <-time.After(publishPoll):
		}
		result, err = client.Query(ctx, sessionrpc.Query{Verb: QueryVerb, Fields: fields})
	}
	// Pre-API daemons reject the extra field. Subscription-only fallback is safe:
	// new daemons independently refuse API keys to an untyped request.
	var rejected *sessionrpc.RemoteError
	if typed && errors.As(err, &rejected) && (rejected.Code == "access_denied" || rejected.Code == "credential_access_denied" || rejected.Code == "invalid_operation") {
		typed = false
		result, err = client.Query(ctx, sessionrpc.Query{Verb: QueryVerb, Fields: map[string]string{HarnessField: string(Claude)}})
	}
	if err != nil {
		var remote *sessionrpc.RemoteError
		if errors.As(err, &remote) {
			switch remote.Code {
			case string(fetchMissing), string(fetchUnavailable), string(fetchBlocked), string(fetchConflict), string(fetchSuperseded), string(fetchChanged):
				return Credential{}, fetchFailure(remote.Code)
			case "credential_access_denied", "credential_revoked", "credential_invalid", "access_denied", "authentication_failed", "expired":
				return Credential{}, fetchDenied
			}
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return Credential{}, fetchTimeout
		}
		return Credential{}, fetchTransport
	}
	var d Delivery
	if json.Unmarshal(result.Body, &d) != nil {
		return Credential{}, fetchMalformed
	}
	token, err := NewSecret(d.Token)
	if err != nil {
		return Credential{}, fetchMalformed
	}
	if d.Kind == "" {
		d.Kind = KindClaudeOAuthToken
	}
	if (d.Kind != KindClaudeOAuthToken && d.Kind != KindClaudeAPIKey) || (d.Kind == KindClaudeAPIKey && !typed) {
		return Credential{}, fetchMalformed
	}
	return Credential{Kind: d.Kind, Token: token, Generation: d.Generation}, nil
}
