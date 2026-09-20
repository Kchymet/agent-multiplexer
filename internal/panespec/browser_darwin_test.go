//go:build darwin

package panespec

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"amux/internal/launchenv"
	"amux/internal/store"
)

func browserFixture(t *testing.T) (LaunchSpec, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("AMUX_JAIL", "on")
	s := store.Session{ID: "browser", Agent: "codex", Dir: filepath.Join(home, "own")}
	spec := testLaunchSpec(t, s)
	probe := filepath.Join(s.Dir, "browser-probe")
	build := exec.Command("/usr/bin/cc", "-x", "objective-c", "-", "-framework", "AppKit", "-Wno-deprecated-declarations", "-o", probe)
	build.Stdin = strings.NewReader(`#import <AppKit/AppKit.h>
int main(int argc, const char **argv) {
 @autoreleasepool {
  NSWorkspace *workspace = [NSWorkspace sharedWorkspace];
  NSURL *browser = [workspace URLForApplicationToOpenURL:[NSURL URLWithString:@"https://example.com"]];
  if (!browser) { fprintf(stderr, "default browser lookup failed\n"); return 1; }
  if (argc == 1) { puts("browser discovered"); return 0; }
  NSURL *url = [NSURL URLWithString:[NSString stringWithUTF8String:argv[1]]];
  NSError *error = nil;
  NSRunningApplication *app = [workspace openURLs:@[url] withApplicationAtURL:browser options:NSWorkspaceLaunchWithoutActivation configuration:@{} error:&error];
  if (!app) { fprintf(stderr, "browser open failed: %s\n", [[error description] UTF8String]); return 1; }
  return 0;
 }
}
`)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("compile browser probe: %v: %s", err, out)
	}
	return spec, probe
}

func runBrowserProbe(t *testing.T, spec LaunchSpec, payload []string) {
	t.Helper()
	argv, err := scope(spec.Session.Dir, TabAgent, spec.Session, spec.Access, nil, payload)
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
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sandbox browser: %v: %s", err, out)
	}
}

// Default discovery exercises LaunchServices without launching a UI.
func TestSeatbeltRuntimeBrowserDiscovery(t *testing.T) {
	spec, probe := browserFixture(t)
	runBrowserProbe(t, spec, []string{probe})
}

// A successful open exit status alone is insufficient: the real browser must
// fetch the local callback. Opt-in because this opens two harmless browser tabs.
func TestSeatbeltRuntimeBrowserCallback(t *testing.T) {
	if os.Getenv("AMUX_TEST_BROWSER") != "1" {
		t.Skip("set AMUX_TEST_BROWSER=1 to open local browser callback tabs")
	}
	spec, probe := browserFixture(t)
	for _, kind := range []string{"open", "native"} {
		t.Run(kind, func(t *testing.T) {
			seen := make(chan struct{}, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/amux-browser-check" {
					http.NotFound(w, r)
					return
				}
				select {
				case seen <- struct{}{}:
				default:
				}
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				fmt.Fprint(w, "<title>amux browser check</title><p>amux browser authentication handoff works. You can close this tab.</p>")
			}))
			defer server.Close()
			url := server.URL + "/amux-browser-check"
			payload := []string{"/usr/bin/open", "-g", url}
			if kind == "native" {
				payload = []string{probe, url}
			}
			runBrowserProbe(t, spec, payload)
			select {
			case <-seen:
			case <-time.After(15 * time.Second):
				t.Fatal("browser never fetched the callback")
			}
		})
	}
}
