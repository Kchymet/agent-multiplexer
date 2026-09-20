//go:build darwin

package panespec

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"amux/internal/launchenv"
	"amux/internal/store"
)

// Exercise the native DNS API without depending on Internet availability or a
// cached hostname. A TCP-only loopback fixture never opens the system resolver.
func TestSeatbeltRuntimeDNSService(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("AMUX_JAIL", "on")
	s := store.Session{ID: "dns", Agent: "codex", Dir: filepath.Join(home, "own")}
	spec := testLaunchSpec(t, s)
	probe := filepath.Join(s.Dir, "dns-probe")
	build := exec.Command("/usr/bin/cc", "-x", "c", "-", "-o", probe)
	build.Stdin = strings.NewReader(`#include <dns_sd.h>
#include <stdio.h>
int main(void) {
 DNSServiceRef service = NULL;
 DNSServiceErrorType error = DNSServiceCreateConnection(&service);
 if (error != kDNSServiceErr_NoError) {
  fprintf(stderr, "DNSServiceCreateConnection: %d\n", error);
  return 1;
 }
 DNSServiceRefDeallocate(service);
 return 0;
}
`)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("compile native DNS probe: %v: %s", err, out)
	}
	argv, err := scope(s.Dir, TabAgent, s, spec.Access, nil, []string{probe})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = s.Dir
	cmd.Env, err = launchenv.Build([]string{"HOME=" + home, "PATH=/usr/bin:/bin"}, platformLaunchEnv(spec), launchenv.ModelCapability{})
	if err != nil {
		t.Fatal(err)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sandbox native DNS service: %v: %s", err, out)
	}
}

// Opt-in public connectivity check. No credentials or model calls are sent;
// HTTP errors still prove DNS, TLS and the remote HTTP connection succeeded.
func TestSeatbeltRuntimePublicHTTPS(t *testing.T) {
	if os.Getenv("AMUX_TEST_NETWORK") != "1" {
		t.Skip("set AMUX_TEST_NETWORK=1 to check public model and MCP endpoints")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("AMUX_JAIL", "on")
	s := store.Session{ID: "network", Agent: "codex", Dir: filepath.Join(home, "own")}
	spec := testLaunchSpec(t, s)
	for _, endpoint := range []string{"https://api.anthropic.com/v1/models", "https://api.openai.com/v1/models", "https://chatgpt.com/backend-api/codex/models", "https://mcp.linear.app/mcp"} {
		t.Run(endpoint, func(t *testing.T) {
			payload := []string{"/usr/bin/curl", "-q", "--noproxy", "*", "--silent", "--show-error", "--max-time", "15", "--output", "/dev/null", "--write-out", "%{http_code}", endpoint}
			argv, err := scope(s.Dir, TabAgent, s, spec.Access, nil, payload)
			if err != nil {
				t.Fatal(err)
			}
			env, err := launchenv.Build([]string{"HOME=" + home, "PATH=/usr/bin:/bin"}, platformLaunchEnv(spec), launchenv.ModelCapability{})
			if err != nil {
				t.Fatal(err)
			}
			for _, mode := range []string{"host", "sandbox"} {
				command := payload
				if mode == "sandbox" {
					command = argv
				}
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, command[0], command[1:]...)
				cmd.Dir, cmd.Env = s.Dir, env
				out, err := cmd.CombinedOutput()
				status, parseErr := strconv.Atoi(strings.TrimSpace(string(out)))
				if err != nil || parseErr != nil || status < 100 || status > 599 {
					t.Errorf("%s HTTPS: %v: %s", mode, err, out)
				} else {
					t.Logf("%s HTTPS status: %s", mode, out)
				}
			}
		})
	}
}
