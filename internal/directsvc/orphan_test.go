package directsvc

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The helper modes below run inside a re-executed test binary, standing in
// for a daemon (or a managed child) so orphan handling is tested against real
// process death without touching the host's daemon.
func TestMain(m *testing.M) {
	switch os.Getenv("AMUX_DIRECTSVC_HELPER") {
	case "daemon":
		helperDaemon()
	case "child":
		helperChild()
	}
	os.Exit(m.Run())
}

// helperDaemon starts the managed child through the production ExecStarter,
// reports the child's pid, and waits to be killed.
func helperDaemon() {
	p, err := ExecStarter(os.Getenv("AMUX_DIRECTSVC_CHILD"), os.Getenv("AMUX_DIRECTSVC_LOG"))(Settings{})
	if err != nil {
		os.Stdout.WriteString("error " + err.Error() + "\n")
		os.Exit(2)
	}
	os.Stdout.WriteString("child " + itoa(p.Pid()) + "\n")
	time.Sleep(time.Minute)
	os.Exit(3)
}

// helperChild is a managed child whose parent exits under it: it must notice
// the re-parenting and stop on its own.
func helperChild() {
	marker := os.Getenv("AMUX_DIRECTSVC_MARKER")
	go func() { time.Sleep(20 * time.Second); os.Exit(4) }()
	expected, err := SupervisorPID(os.Getenv)
	if err != nil {
		os.Exit(5)
	}
	WatchParent(expected, 10*time.Millisecond, nil, func() {
		_ = os.WriteFile(marker, []byte("stopped"), 0o600)
		os.Exit(0)
	})
}

func itoa(n int) string { return fmtInt(n) }

func fmtInt(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// A daemon that dies without stopping its child (SIGKILL, OOM) must not leave
// the listener running: on Linux the kernel delivers the parent-death signal.
func TestManagedChildStopsWhenDaemonIsKilled(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("parent-death signal is Linux-only; WatchParent covers other platforms")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "terminated")
	child := filepath.Join(dir, "amux-child")
	writeScript(t, child, "#!/bin/sh\ntrap 'echo term > "+marker+"; exit 0' TERM\nwhile :; do sleep 0.05; done\n")

	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "AMUX_DIRECTSVC_HELPER=daemon",
		"AMUX_DIRECTSVC_CHILD="+child, "AMUX_DIRECTSVC_LOG="+filepath.Join(dir, "direct.log"))
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "child ") {
		_ = cmd.Process.Kill()
		t.Fatalf("helper daemon said %q (%v)", line, err)
	}
	time.Sleep(100 * time.Millisecond) // let the child install its trap
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	waitFor(t, func() bool { _, err := os.Stat(marker); return err == nil })
}

// WatchParent stops a child whose daemon exited before the child even
// started watching: the expected pid is fixed by the parent before exec, so
// there is no window in which a re-parented child adopts its new parent as
// the baseline. The shell exits immediately after starting the helper.
func TestWatchParentStopsChildWhoseParentDiedBeforeWatching(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX re-parenting")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "stopped")
	cmd := exec.Command("/bin/sh", "-c", SupervisorPIDEnv+`=$$ "$0" -test.run='^$' >/dev/null 2>&1 &`, os.Args[0])
	cmd.Env = append(os.Environ(), "AMUX_DIRECTSVC_HELPER=child", "AMUX_DIRECTSVC_MARKER="+marker)
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { _, err := os.Stat(marker); return err == nil })
}

// The same, with the daemon dying after the child is already watching.
func TestWatchParentStopsChildWhoseParentDiesLater(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX re-parenting")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "stopped")
	cmd := exec.Command("/bin/sh", "-c", SupervisorPIDEnv+`=$$ "$0" -test.run='^$' >/dev/null 2>&1 & sleep 1`, os.Args[0])
	cmd.Env = append(os.Environ(), "AMUX_DIRECTSVC_HELPER=child", "AMUX_DIRECTSVC_MARKER="+marker)
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { _, err := os.Stat(marker); return err == nil })
}

func TestSupervisorPID(t *testing.T) {
	env := func(v string) func(string) string { return func(string) string { return v } }
	for _, bad := range []string{"", "x", "1", "0", "-5"} {
		if _, err := SupervisorPID(env(bad)); err == nil {
			t.Errorf("SupervisorPID(%q) accepted", bad)
		}
	}
	if pid, err := SupervisorPID(env("4242")); err != nil || pid != 4242 {
		t.Fatalf("SupervisorPID(4242) = %d, %v", pid, err)
	}
	if err := CheckParent(os.Getppid()); err != nil {
		t.Fatalf("CheckParent(real parent) = %v", err)
	}
	if err := CheckParent(os.Getppid() + 1); err == nil {
		t.Fatal("CheckParent accepted a pid that is not the parent")
	}
}
