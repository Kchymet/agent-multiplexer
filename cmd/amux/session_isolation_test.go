package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"amux/internal/access"
	"amux/internal/core"
	"amux/internal/sessionreport"
)

// The test binary must never reach the fixed session authority. `go test` run
// from inside an amux worker inherits that worker's own signed context (the
// AMUX_SESSION_ACCESS locator on macOS, the fixed mount on Linux), so a test that
// invokes a real self-scoped command — `agent done`, `agent name`, any `do` verb
// once the context proves a restricted client — would open the worker's own
// mailbox and archive or rename the very session running the suite. HOME/XDG
// redirection does not help: the authority is deliberately outside their reach.
//
// The three seams below are the only production paths into that authority.
// TestMain replaces them with fail-closed fakes for the whole binary (including
// subprocess children that re-enter main()), and sandboxCLI re-installs counting
// fakes per test. Tests that want a specific fixed-context behaviour install
// their own fakes on top, exactly as before.

// productionSessionAuthority captures the real seams at package init, before
// TestMain isolates them, so the regressions below can prove they are gone.
var productionSessionAuthority = sessionAuthoritySeams{
	load: loadAgentSessionContext, open: openRestrictedSessionRPC, restricted: sessionContextRestricted,
}

type sessionAuthoritySeams struct {
	load       func() (access.SessionContext, error)
	open       func() (restrictedSessionRPC, error)
	restricted func() bool
}

var (
	errIsolatedSessionContext = fmt.Errorf("test isolation: fixed session context unavailable: %w", fs.ErrNotExist)
	errIsolatedSessionRPC     = errors.New("test isolation: fixed session RPC unavailable")
)

// isolatedSessionAuthority is the fail-closed stand-in: no fixed context (a
// proven ENOENT, so the client is a plain host shell) and no session RPC. It
// counts calls so a test can show a command was routed into it, not past it.
type isolatedSessionAuthority struct {
	loads, opens, restrictedChecks atomic.Int64
}

func (a *isolatedSessionAuthority) install() (restore func()) {
	previous := sessionAuthoritySeams{
		load: loadAgentSessionContext, open: openRestrictedSessionRPC, restricted: sessionContextRestricted,
	}
	loadAgentSessionContext = func() (access.SessionContext, error) {
		a.loads.Add(1)
		return access.SessionContext{}, errIsolatedSessionContext
	}
	openRestrictedSessionRPC = func() (restrictedSessionRPC, error) {
		a.opens.Add(1)
		return nil, errIsolatedSessionRPC
	}
	sessionContextRestricted = func() bool {
		a.restrictedChecks.Add(1)
		return false
	}
	return func() {
		loadAgentSessionContext, openRestrictedSessionRPC, sessionContextRestricted =
			previous.load, previous.open, previous.restricted
	}
}

// isolateSessionAuthority installs a fresh counting fake for one test and
// restores whatever was installed before it (normally TestMain's fake).
func isolateSessionAuthority(t *testing.T) *isolatedSessionAuthority {
	t.Helper()
	fake := &isolatedSessionAuthority{}
	t.Cleanup(fake.install())
	return fake
}

func TestMain(m *testing.M) {
	// Every test, and every subprocess child that re-enters main(), starts from
	// a fail-closed session authority. Nothing in this package opts back into
	// the real one; a test that needs fixed-context behaviour fakes it.
	restore := (&isolatedSessionAuthority{}).install()
	defer restore()
	// The inherited locator is only a macOS hint and the fake above already
	// ignores it, but a test must not even see the worker's own credential path.
	if err := os.Unsetenv(core.SessionAccessEnv); err != nil {
		panic(err)
	}
	if err := useCanonicalTestTempDir(); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

// liveShapedSessionContext writes a context.json the real loader would accept
// at a directory the macOS locator can point to. Nothing signs it and no
// mailbox exists, so even a leaked seam could not complete an RPC — but the
// assertions below are stricter: the real loader is never consulted at all.
func liveShapedSessionContext(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	body := fmt.Sprintf(`{"protocol":%d,"subjectId":"live-worker","mailboxDir":%q}`,
		access.ProtocolVersion, filepath.Join(dir, "mailbox"))
	if err := os.WriteFile(filepath.Join(dir, access.ContextFileName), []byte(body), 0o400); err != nil {
		t.Fatal(err)
	}
	return dir
}

func seamsAreProduction() (load, open, restricted bool) {
	same := func(a, b any) bool { return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer() }
	return same(loadAgentSessionContext, productionSessionAuthority.load),
		same(openRestrictedSessionRPC, productionSessionAuthority.open),
		same(sessionContextRestricted, productionSessionAuthority.restricted)
}

// TestSessionAuthorityIsolatedByDefault: a test that never calls sandboxCLI —
// or the subprocess child that re-enters main() — still cannot reach the real
// loader or the real mailbox client.
func TestSessionAuthorityIsolatedByDefault(t *testing.T) {
	if load, open, restricted := seamsAreProduction(); load || open || restricted {
		t.Fatalf("production session authority installed: load=%v open=%v restricted=%v", load, open, restricted)
	}
	if os.Getenv(core.SessionAccessEnv) != "" {
		t.Fatalf("%s leaked into the test binary", core.SessionAccessEnv)
	}
	if _, err := restrictedAction(core.Action{Action: core.ActionSetArchived, ID: "live-worker"}); !errors.Is(err, errIsolatedSessionRPC) {
		t.Fatalf("restrictedAction error = %v, want isolated RPC refusal", err)
	}
	if _, err := loadAgentSessionContext(); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("loadAgentSessionContext error = %v, want proven-absent context", err)
	}
	if sessionContextRestricted() {
		t.Fatal("isolated session context reports a restricted client")
	}
}

// TestSandboxCLICannotReachSessionAuthority reproduces the worker leak with
// fakes: the locator points at a live-shaped context, the legacy env hint names
// a subject, the self-scoped commands run for real, and every one of them is
// answered by the isolated authority. The real loader and mailbox client are
// never called, so a worker cannot be archived or renamed by its own suite.
func TestSandboxCLICannotReachSessionAuthority(t *testing.T) {
	// Sentinels stand in for the inherited authority a worker would have had
	// installed before sandboxCLI ran. They are harmless but must stay silent:
	// only sandboxCLI's own replacement may answer the commands below.
	sentinel := &isolatedSessionAuthority{}
	t.Cleanup(sentinel.install())
	fake := sandboxCLI(t)
	t.Setenv(core.SessionAccessEnv, liveShapedSessionContext(t))
	t.Setenv("AMUX_WORKGROUP", "live-worker")

	if load, open, restricted := seamsAreProduction(); load || open || restricted {
		t.Fatalf("sandboxCLI left production session authority installed: load=%v open=%v restricted=%v", load, open, restricted)
	}

	if err := cmdAgentDone(nil); err == nil || !strings.Contains(err.Error(), "not inside") {
		t.Fatalf("agent done error = %v, want fixed-context refusal", err)
	}
	if err := cmdName([]string{"renamed", "by", "tests"}); err == nil || !strings.Contains(err.Error(), "not inside") {
		t.Fatalf("agent name error = %v, want fixed-context refusal", err)
	}
	if got := fake.loads.Load(); got != 2 {
		t.Fatalf("isolated loader calls = %d, want 2 (one per self command)", got)
	}
	if got := fake.opens.Load(); got != 0 {
		t.Fatalf("self commands opened session RPC %d times without an identity", got)
	}

	// A host-side mutation goes to the stubbed daemon dial, never the mailbox.
	if _, err := sendActionID(core.Action{Action: core.ActionSetArchived, ID: "live-worker"}); err == nil || err.Error() != "daemon offline (test)" {
		t.Fatalf("sendActionID error = %v, want stubbed dial", err)
	}
	if got := fake.restrictedChecks.Load(); got == 0 {
		t.Fatal("sendActionID never consulted the isolated session context")
	}
	// A mailbox-only path (agent hook reports) hits the isolated client.
	if err := restrictedReport(sessionreport.Activity, nil); err == nil || !errors.Is(err, errIsolatedSessionRPC) {
		t.Fatalf("restrictedReport error = %v, want isolated RPC refusal", err)
	}
	if got := fake.opens.Load(); got != 1 {
		t.Fatalf("isolated RPC opens = %d, want 1", got)
	}
	if loads, opens, checks := sentinel.loads.Load(), sentinel.opens.Load(), sentinel.restrictedChecks.Load(); loads != 0 || opens != 0 || checks != 0 {
		t.Fatalf("sandboxCLI let the inherited authority answer: loads=%d opens=%d restricted checks=%d", loads, opens, checks)
	}
}

// TestSandboxCLIRestoresIsolatedAuthority: a sandboxCLI test cannot leave the
// package-wide fake weakened or replaced for the tests that run after it.
func TestSandboxCLIRestoresIsolatedAuthority(t *testing.T) {
	before := sessionAuthoritySeams{
		load: loadAgentSessionContext, open: openRestrictedSessionRPC, restricted: sessionContextRestricted,
	}
	workgroupBefore := os.Getenv("AMUX_WORKGROUP")
	t.Run("inner", func(t *testing.T) {
		sandboxCLI(t)
		t.Setenv("AMUX_WORKGROUP", "live-worker")
	})
	same := func(a, b any) bool { return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer() }
	if !same(loadAgentSessionContext, before.load) || !same(openRestrictedSessionRPC, before.open) || !same(sessionContextRestricted, before.restricted) {
		t.Fatal("sandboxCLI did not restore the previously installed session authority")
	}
	if os.Getenv(core.SessionAccessEnv) != "" || os.Getenv("AMUX_WORKGROUP") != workgroupBefore {
		t.Fatal("sandboxCLI leaked session environment past its test")
	}
}
