package centralauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"amux/internal/sessionrpc"
)

const launchChildEnv = "AMUX_CENTRALAUTH_LAUNCH_CHILD"

// TestMain lets a test re-execute this binary as a launcher child: Launch ends
// in exec, so it can only be observed from another process.
func TestMain(m *testing.M) {
	if os.Getenv(launchChildEnv) == "1" {
		os.Unsetenv(launchChildEnv)
		LaunchRetry = 10 * time.Millisecond
		failures := 2
		fetch := func() (Secret, error) {
			if failures > 0 {
				failures--
				return Secret{}, errors.New("central_credential_unavailable")
			}
			return NewSecret(mockToken)
		}
		os.Exit(Launch(os.Args[1:], os.Stderr, fetch))
	}
	os.Exit(m.Run())
}

func TestLaunchDeliversTokenOnlyThroughClaudeEnvironment(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "observed")
	fake := filepath.Join(dir, "claude")
	script := "#!/bin/sh\n{ printf 'token=%s\\n' \"$CLAUDE_CODE_OAUTH_TOKEN\"; printf 'api=%s\\n' \"${ANTHROPIC_API_KEY-unset}\"; " +
		"printf 'fd=%s\\n' \"${CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR-unset}\"; printf 'args=%s\\n' \"$*\"; } > " + out + "\n"
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, fake, "--resume", "abc")
	cmd.Env = append(os.Environ(), launchChildEnv+"=1",
		"CLAUDE_CODE_OAUTH_TOKEN=stale-inherited", "ANTHROPIC_API_KEY=inherited", "CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR=9")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("launcher child: %v: %s", err, stderr.String())
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	want := "token=" + mockToken + "\napi=unset\nfd=unset\nargs=--resume abc\n"
	if string(b) != want {
		t.Fatalf("fake Claude observed:\n%s\nwant:\n%s", b, want)
	}
	// Two unavailable answers produce one explanation, never the value.
	msg := stderr.String()
	if strings.Count(msg, "amux auth login") != 1 || strings.Contains(msg, mockToken) {
		t.Fatalf("launcher stderr = %q", msg)
	}
}

func TestLaunchEnvReplacesEveryAlternateCredentialInput(t *testing.T) {
	tok, _ := NewSecret(mockToken)
	env := LaunchEnv([]string{
		"PATH=/usr/bin", "CLAUDE_CODE_OAUTH_TOKEN=old", "ANTHROPIC_AUTH_TOKEN=x", "ANTHROPIC_API_KEY=",
		"CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR=3", "CLAUDE_CONFIG_DIR=/private",
	}, tok)
	want := []string{"PATH=/usr/bin", "CLAUDE_CONFIG_DIR=/private", ClaudeTokenEnv + "=" + mockToken}
	if !slices.Equal(env, want) {
		t.Fatalf("LaunchEnv = %v", env)
	}
}

type fakeQuerier struct {
	result sessionrpc.Result
	err    error
	query  sessionrpc.Query
}

func (f *fakeQuerier) Query(_ context.Context, q sessionrpc.Query) (sessionrpc.Result, error) {
	f.query = q
	return f.result, f.err
}
func (f *fakeQuerier) Close() error { return nil }

func TestFetchFromSessionRequestsOnlyItsHarnessAndHidesDetail(t *testing.T) {
	previous := openSession
	t.Cleanup(func() { openSession = previous })
	body, _ := json.Marshal(Delivery{Token: mockToken, Generation: 4})
	fake := &fakeQuerier{result: sessionrpc.Result{Status: sessionrpc.StatusOK, Body: body}}
	openSession = func() (sessionQuerier, error) { return fake, nil }
	tok, err := FetchFromSession()
	if err != nil || tok.Reveal() != mockToken {
		t.Fatalf("FetchFromSession = %v, %v", tok, err)
	}
	if fake.query.Verb != QueryVerb || len(fake.query.Fields) != 1 || fake.query.Fields[HarnessField] != "claude" ||
		fake.query.ID != "" || fake.query.Target != "" {
		t.Fatalf("query = %+v", fake.query)
	}

	for name, tc := range map[string]struct {
		open func() (sessionQuerier, error)
		want string
	}{
		"no mailbox": {func() (sessionQuerier, error) { return nil, errors.New("/secret/path missing") }, "session mailbox unavailable"},
		"denied": {func() (sessionQuerier, error) {
			return &fakeQuerier{err: &sessionrpc.RemoteError{Status: sessionrpc.StatusDenied, Code: "credential_access_denied"}}, nil
		}, "credential_access_denied"},
		"timeout": {func() (sessionQuerier, error) { return &fakeQuerier{err: context.DeadlineExceeded}, nil }, "daemon did not answer"},
		"malformed": {func() (sessionQuerier, error) {
			return &fakeQuerier{result: sessionrpc.Result{Body: []byte(`{"token":"has space ` + mockToken + `"}`)}}, nil
		}, "malformed response"},
	} {
		t.Run(name, func(t *testing.T) {
			openSession = tc.open
			_, err := FetchFromSession()
			if err == nil || err.Error() != tc.want {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}
