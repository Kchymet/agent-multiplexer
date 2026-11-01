package procstat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func relink(t *testing.T, root string, pid, target string) {
	t.Helper()
	link := filepath.Join(root, pid, "cwd")
	_ = os.Remove(link)
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func selfRoles(snap Snapshot) map[int]string {
	roles := map[int]string{}
	for _, p := range snap.Self.Procs {
		roles[p.PID] = p.Role
	}
	return roles
}

// A daemon or TUI started from inside an agent's worktree inherits that cwd;
// it (and its non-sandbox children) must stay amux's own, while an `amux
// agent` hook in the same place is still the agent's.
func TestLongLivedAmuxIgnoresInheritedCwd(t *testing.T) {
	root := fixture(t)
	relink(t, root, "100", fixSessions+"/771951/374179/app") // daemon
	relink(t, root, "501", fixSessions+"/771951/374179/app") // tui
	relink(t, root, "500", fixWorkspaces+"/555555")          // provide
	snap, err := newTestSampler(root, &clock{t: time.Unix(1000, 0)}).Sample()
	if err != nil {
		t.Fatal(err)
	}
	roles := selfRoles(snap)
	if roles[100] != RoleDaemon || roles[501] != RoleTUI || roles[500] != RoleProvide {
		t.Fatalf("self roles = %v", roles)
	}
	if roles[101] != RoleChild {
		t.Fatalf("daemon's git child left Self: %v", roles)
	}
	// The hook (204) is still charged to the agent, and the agent did not
	// absorb the daemon.
	if a := session(snap, "374179"); a == nil || a.Processes != 5 {
		t.Fatalf("agent = %+v", a)
	}
	if ws := session(snap, "555555"); ws == nil || ws.Processes != 1 {
		t.Fatalf("workspace session absorbed provide: %+v", ws)
	}
}

// Stored ownership beats the path shape: a member moved to another workgroup
// keeps its old sandbox, and a legacy sub-session's directory is named after
// repo+branch, not its id.
func TestStoredOwnershipBeatsPathShape(t *testing.T) {
	root := fixture(t)
	relink(t, root, "300", fixSessions+"/771951/agent-multiplexer-main/src") // legacy sub-session
	owners := []Owner{
		{Dir: fixSessions + "/771951/374179", Session: "374179", Workgroup: "888888"}, // moved
		{Dir: fixSessions + "/771951/agent-multiplexer-main/", Session: "legacy1", Workgroup: "771951"},
		{Dir: fixSessions + "/771951", Session: "771951"}, // the container: its own group
		{Dir: "/", Session: "bogus"},                      // ignored
	}
	c := &clock{t: time.Unix(1000, 0)}
	s := New(Config{
		Root: root, Interval: 3 * time.Second, TopN: 3, SessionsDir: fixSessions, WorkspacesDir: fixWorkspaces,
		PageSize: 4096, CPUs: 32, Now: c.now, Owners: func() []Owner { return owners },
	})
	snap, err := s.Sample()
	if err != nil {
		t.Fatal(err)
	}
	agent := session(snap, "374179")
	// bwrap is claimed through its --bind argument, claude/gradle/hook by cwd,
	// qemu by inheritance: all roll up into the NEW workgroup.
	if agent == nil || agent.WorkgroupID != "888888" || agent.Processes != 5 {
		t.Fatalf("moved member = %+v", agent)
	}
	if wg := workgroup(snap, "888888"); wg == nil || wg.Processes != 5 {
		t.Fatalf("new workgroup = %+v", wg)
	}
	if l := session(snap, "legacy1"); l == nil || l.WorkgroupID != "771951" {
		t.Fatalf("legacy sub-session = %+v (sessions %+v)", l, snap.Sessions)
	}
	if session(snap, "agent-multiplexer-main") != nil {
		t.Fatal("path-shape id leaked for a stored session")
	}
	if session(snap, "bogus") != nil {
		t.Fatal("root dir owner matched everything")
	}
	// Path parsing still covers directories the daemon has not reported.
	if ws := session(snap, "555555"); ws == nil {
		t.Fatal("pattern fallback lost")
	}

	// Ownership changes are picked up on the owners cadence and re-applied to
	// cached processes without re-reading them.
	owners[0].Workgroup = "777777"
	c.t = c.t.Add(time.Second)
	snap, _ = s.Sample()
	if a := session(snap, "374179"); a.WorkgroupID != "888888" {
		t.Fatalf("owners refreshed before ownersTTL: %+v", a)
	}
	c.t = c.t.Add(ownersTTL)
	snap, _ = s.Sample()
	if a := session(snap, "374179"); a.WorkgroupID != "777777" {
		t.Fatalf("owners not refreshed: %+v", a)
	}
}

func TestRedactArgv(t *testing.T) {
	cases := map[string]string{
		"curl -H Authorization: Bearer ghp_abcdefghijklmnop https://x": "curl -H Authorization: <redacted> https://x",
		"gh --token ghp_abcdefghijklmnop pr view":                      "gh --token <redacted> pr view",
		"tool --password hunter2 --verbose":                            "tool --password <redacted> --verbose",
		"tool --api-key=abc123 --name=x":                               "tool --api-key=<redacted> --name=x",
		"env GITHUB_TOKEN=abc123 make":                                 "env GITHUB_TOKEN=<redacted> make",
		"git clone https://bob:s3cr3t@github.com/o/r":                  "git clone https://bob:<redacted>@github.com/o/r",
		"echo see sk-abcdefghijklmnopqrstu end":                        "echo see <redacted> end",
		"x --client-secret s":                                          "x --client-secret <redacted>",
		"qemu-system-x86_64 -avd Pixel -no-snapshot":                   "qemu-system-x86_64 -avd Pixel -no-snapshot",
		"bwrap --bind /data/amux/sessions/w/a /data/amux/sessions/w/a": "bwrap --bind /data/amux/sessions/w/a /data/amux/sessions/w/a",
	}
	for in, want := range cases {
		// The header case is one argv element with spaces.
		argv := strings.Fields(in)
		if strings.HasPrefix(in, "curl -H") {
			argv = []string{"curl", "-H", "Authorization: Bearer ghp_abcdefghijklmnop", "https://x"}
		}
		if got := strings.Join(redactArgv(argv), " "); got != want {
			t.Errorf("redact(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}

// Secrets in a published command line never reach the snapshot.
func TestSnapshotCmdRedacted(t *testing.T) {
	root := fixture(t)
	if err := os.WriteFile(filepath.Join(root, "202", "cmdline"), []byte("gh\x00api\x00-H\x00Authorization: token ghp_abcdefghijklmnop\x00--password\x00hunter2\x00"), 0o644); err != nil {
		t.Fatal(err)
	}
	snap, err := newTestSampler(root, &clock{t: time.Unix(1000, 0)}).Sample()
	if err != nil {
		t.Fatal(err)
	}
	a := session(snap, "374179")
	for _, p := range a.Top {
		if p.PID == 202 {
			if strings.Contains(p.Cmd, "ghp_") || strings.Contains(p.Cmd, "hunter2") {
				t.Fatalf("secret published: %q", p.Cmd)
			}
			return
		}
	}
	t.Fatalf("pid 202 not listed: %+v", a.Top)
}
