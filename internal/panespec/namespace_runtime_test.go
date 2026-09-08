package panespec

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"amux/internal/access"
	"amux/internal/core"
	"amux/internal/launchenv"
	"amux/internal/store"
)

// This is the acceptance fixture for relationships a pure argv assertion
// cannot prove: PID/proc aliases, inherited descriptors, ancestor renames, and
// objects created after namespace setup. It skips explicitly on unsupported
// hosts; it never falls back to a shared namespace.
func TestRuntimeNamespaceRejectsAliasesFDsAndFutureSiblings(t *testing.T) {
	requireRuntimeIsolation(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("AMUX_JAIL", "on")
	for _, name := range []string{
		"AMUX_MUX_TOKEN", "AMUX_PROVIDER_TOKEN", "AMUX_PROVIDER_PASSWORD",
		"AMUX_TLS_KEY", "AMUX_TLS_KEY_PASSWORD", "AMUX_HOST_PRIVATE_KEY",
		"AMUX_RPC_DIR", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY",
		"AZURE_CLIENT_SECRET", "GOOGLE_APPLICATION_CREDENTIALS",
		"GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN",
	} {
		t.Setenv(name, "planted-host-only")
	}
	t.Setenv("OPENAI_API_KEY", "authorized-codex-model-key")
	own := filepath.Join(core.SessionsDir(), "root", "own")
	peer := filepath.Join(core.SessionsDir(), "root", "peer")
	for _, dir := range []string{own, peer} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	peerCanary := filepath.Join(peer, "secret")
	if err := os.WriteFile(peerCanary, []byte("peer-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	peerFD, err := os.Open(peerCanary)
	if err != nil {
		t.Fatal(err)
	}
	defer peerFD.Close()

	s := store.Session{ID: "own", Agent: "codex", Dir: own}
	spec := testLaunchSpec(t, s)
	if err := os.WriteFile(filepath.Join(spec.Access.CredentialHostDir, access.ContextFileName), []byte(`{"protocol":1,"subjectId":"own","mailboxDir":"`+spec.Access.MailboxMountDir+`"}`), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(spec.Access.CredentialHostDir, "current"), []byte("credential"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(spec.Access.MailboxHostDir, "service.json"), []byte("service"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(spec.Access.MailboxHostDir, "responses", "response"), []byte("response"), 0o400); err != nil {
		t.Fatal(err)
	}
	future := filepath.Join(core.SessionsDir(), "root", "future", "secret")
	hostPIDNamespace, err := os.Readlink("/proc/self/ns/pid")
	if err != nil {
		t.Fatal(err)
	}
	script := `set -eux
test -z "${AMUX_MUX_TOKEN+x}"
test -z "${AMUX_PROVIDER_TOKEN+x}"
test -z "${AMUX_PROVIDER_PASSWORD+x}"
test -z "${AMUX_TLS_KEY+x}"
test -z "${AMUX_TLS_KEY_PASSWORD+x}"
test -z "${AMUX_HOST_PRIVATE_KEY+x}"
test -z "${AMUX_RPC_DIR+x}"
test -z "${AWS_ACCESS_KEY_ID+x}"
test -z "${AWS_SECRET_ACCESS_KEY+x}"
test -z "${AZURE_CLIENT_SECRET+x}"
test -z "${GOOGLE_APPLICATION_CREDENTIALS+x}"
test -z "${GH_ENTERPRISE_TOKEN+x}"
test -z "${GITHUB_ENTERPRISE_TOKEN+x}"
test "$OPENAI_API_KEY" = authorized-codex-model-key
! tr '\000' '\n' < /proc/1/environ | grep -F planted-host-only
tr '\000' '\n' < /proc/1/environ | grep -Fx 'OPENAI_API_KEY=authorized-codex-model-key'
test ! -e "$PANESPEC_TEST_SOURCE_PARENT"
test ! -e "$PANESPEC_TEST_CREDENTIAL_SOURCE"
test ! -e /mnt/c
test ! -e /var/run/docker.sock
test ! -e "$HOME/.zsh_history"
test ! -e "$HOME/.bash_history"
test "$(command -v amux)" = /amux-bin/amux
test -x /amux-bin/amux
if mv /amux-bin /amux-bin-hidden 2>/dev/null; then exit 25; fi
test "$(command -v amux)" = /amux-bin/amux
amux -test.run='^$' >/dev/null
"$PANESPEC_TEST_SELF" -test.run='^$' >/dev/null
test "$(cat /amux-session-access/current)" = credential
test "$(cat "$PANESPEC_TEST_MAILBOX/service.json")" = service
echo request > "$PANESPEC_TEST_REQUESTS/request"
if /bin/sh -c 'echo bad > "$1"' sh "$PANESPEC_TEST_MAILBOX/service.json" 2>/dev/null; then exit 21; fi
if /bin/sh -c 'echo bad > "$1"' sh "$PANESPEC_TEST_MAILBOX/responses/response" 2>/dev/null; then exit 23; fi
if /bin/sh -c 'echo bad > "$1"' sh /amux-session-access/current 2>/dev/null; then exit 22; fi
mv "$PANESPEC_TEST_OWN/.amux" "$PANESPEC_TEST_OWN/.amux-hidden" 2>/dev/null || true
test "$(cat /amux-session-access/current)" = credential
test ! -e "$PANESPEC_TEST_PEER"
test ! -e "/proc/$PANESPEC_TEST_HOST_PID/root$PANESPEC_TEST_PEER"
test ! -e "/proc/1/root$PANESPEC_TEST_PEER"
test "$(readlink /proc/self/ns/pid)" != "$PANESPEC_TEST_HOST_PID_NS"
test "$$" -ne 1
grep -Eq '^NSpid:[[:space:]]+1$' /proc/1/status
if [ "$(cat /proc/self/fd/3 2>/dev/null || true)" = peer-secret ]; then exit 24; fi
! grep -F " $PANESPEC_TEST_DATA " /proc/self/mountinfo
! grep -F " $PANESPEC_TEST_STATE " /proc/self/mountinfo
! grep -F " /run " /proc/self/mountinfo
grep -F " $PANESPEC_TEST_OWN " /proc/self/mountinfo
grep -F " /amux-session-access " /proc/self/mountinfo
echo SCOPE_READY
read -r signal
test ! -e "$PANESPEC_TEST_FUTURE"
test "$(cat /amux-session-access/current)" = rotated
`
	argv, err := scope(own, TabAgent, s, spec.Access, []string{"/bin/sh", "-c", script}, nil)
	if err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		t.Fatal(err)
	}
	payloadEnvironment := []string{
		"PANESPEC_TEST_MAILBOX=" + spec.Access.MailboxMountDir,
		"PANESPEC_TEST_REQUESTS=" + spec.Access.RequestsMountDir,
		"PANESPEC_TEST_OWN=" + own,
		"PANESPEC_TEST_PEER=" + peer,
		"PANESPEC_TEST_FUTURE=" + future,
		fmt.Sprintf("PANESPEC_TEST_HOST_PID=%d", os.Getpid()),
		"PANESPEC_TEST_HOST_PID_NS=" + hostPIDNamespace,
		"PANESPEC_TEST_DATA=" + core.DataDir(),
		"PANESPEC_TEST_STATE=" + core.StateDir(),
		"PANESPEC_TEST_SOURCE_PARENT=" + filepath.Dir(spec.Access.MailboxHostDir),
		"PANESPEC_TEST_CREDENTIAL_SOURCE=" + spec.Access.CredentialHostDir,
		"PANESPEC_TEST_SELF=" + self,
	}
	argv = withPayloadEnvironment(t, argv, payloadEnvironment)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.ExtraFiles = []*os.File{peerFD}
	cmd.Env, err = launchenv.Build(os.Environ(), nil, launchenv.ForRuntime(s.Agent))
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(stdout)
	for {
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			_ = cmd.Wait()
			t.Fatalf("namespace did not become ready: %v: %s", readErr, stderr.String())
		}
		if line == "SCOPE_READY\n" {
			break
		}
	}
	if err := os.MkdirAll(filepath.Dir(future), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(future, []byte("late-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	mountedCredential := spec.Access.CredentialHostDir + ".mounted"
	if err := os.Rename(spec.Access.CredentialHostDir, mountedCredential); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(spec.Access.CredentialHostDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(spec.Access.CredentialHostDir, "current"), []byte("attacker-replacement"), 0o400); err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(mountedCredential, "replacement")
	if err := os.WriteFile(replacement, []byte("rotated"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, filepath.Join(mountedCredential, "current")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(stdin, "continue\n"); err != nil {
		t.Fatal(err)
	}
	_ = stdin.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("runtime namespace fixture: %v: %s", err, stderr.String())
	}
	if got, err := os.ReadFile(filepath.Join(spec.Access.RequestsHostDir, "request")); err != nil || string(got) != "request\n" {
		t.Fatalf("requests overlay did not persist: %q, %v", got, err)
	}
	if got, err := os.ReadFile(peerCanary); err != nil || string(got) != "peer-secret" {
		t.Fatalf("peer canary changed: %q, %v", got, err)
	}
}

func withPayloadEnvironment(t *testing.T, argv, environment []string) []string {
	t.Helper()
	separator := -1
	for i, arg := range argv {
		if arg == "--" {
			separator = i
		}
	}
	if separator < 0 {
		t.Fatalf("bwrap argv lacks payload separator: %v", argv)
	}
	setenv := make([]string, 0, len(environment)*3)
	for _, entry := range environment {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || name == "" {
			t.Fatalf("invalid fixture environment %q", entry)
		}
		setenv = append(setenv, "--setenv", name, value)
	}
	out := append([]string(nil), argv[:separator]...)
	out = append(out, setenv...)
	return append(out, argv[separator:]...)
}
