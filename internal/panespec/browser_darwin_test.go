//go:build darwin

package panespec

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"amux/internal/launchenv"
	"amux/internal/store"
)

func browserDiscoveryFixture(t *testing.T) (LaunchSpec, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("AMUX_JAIL", "on")
	s := store.Session{ID: "browser", Agent: "codex", Dir: filepath.Join(home, "own")}
	spec := testLaunchSpec(t, s)
	probe := filepath.Join(s.Dir, "browser-discovery-probe")
	build := exec.Command("/usr/bin/cc", "-x", "objective-c", "-", "-framework", "AppKit", "-Wno-deprecated-declarations", "-o", probe)
	build.Stdin = strings.NewReader(`#import <AppKit/AppKit.h>
int main(void) {
 @autoreleasepool {
  NSWorkspace *workspace = [NSWorkspace sharedWorkspace];
  NSURL *browser = [workspace URLForApplicationToOpenURL:[NSURL URLWithString:@"https://example.com"]];
  if (!browser) { fprintf(stderr, "default browser lookup denied\n"); return 20; }
  puts("browser discovered");
  return 0;
 }
}
`)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("compile browser discovery probe: %v: %s", err, out)
	}
	return spec, probe
}

// The control proves this Mac has a discoverable browser; the protected run
// then proves LaunchServices discovery is denied. The fixture never asks
// NSWorkspace or /usr/bin/open to launch an application.
func TestSeatbeltRuntimeDeniesHostApplicationDiscovery(t *testing.T) {
	spec, probe := browserDiscoveryFixture(t)
	if out, err := exec.Command(probe).CombinedOutput(); err != nil {
		t.Fatalf("unsandboxed browser discovery control: %v: %s", err, out)
	}

	argv, err := scope(spec.Session.Dir, TabAgent, spec.Session, spec.Access, nil, []string{probe})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = spec.Session.Dir
	cmd.Env, err = launchenv.Build([]string{"HOME=" + os.Getenv("HOME"), "PATH=/usr/bin:/bin"}, platformLaunchEnv(spec), launchenv.ModelCapability{})
	if err != nil {
		t.Fatal(err)
	}
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("protected session discovered a host browser through LaunchServices: %s", out)
	}
}

func TestSeatbeltProfileOmitsHostApplicationLaunchAuthority(t *testing.T) {
	spec, probe := browserDiscoveryFixture(t)
	argv, err := scope(spec.Session.Dir, TabAgent, spec.Session, spec.Access, nil, []string{probe})
	if err != nil {
		t.Fatal(err)
	}
	if len(argv) < 3 || argv[0] != "/usr/bin/sandbox-exec" || argv[1] != "-p" {
		t.Fatalf("unexpected Seatbelt launch argv: %v", argv)
	}
	for _, forbidden := range []string{
		"(allow lsopen)",
		"com.apple.coreservices.launchservicesd",
		"com.apple.CoreServices.coreservicesd",
		"com.apple.coreservices.quarantine-resolver",
		"com.apple.lsd.mapdb",
		"com.apple.lsd.open",
	} {
		if strings.Contains(argv[2], forbidden) {
			t.Errorf("Seatbelt profile grants host application authority %q", forbidden)
		}
	}
}
