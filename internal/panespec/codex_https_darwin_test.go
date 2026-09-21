//go:build darwin

package panespec

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"amux/internal/cfghome"
	"amux/internal/codexcfg"
	"amux/internal/launchenv"
	"amux/internal/store"
	"amux/internal/wsops"
)

// login status only reads local credentials. This asks the real Codex HTTPS
// client for account limits, proving TLS and host auth without a model turn.
func TestSeatbeltRuntimeCodexHTTPS(t *testing.T) {
	if os.Getenv("AMUX_TEST_NETWORK") != "1" || os.Getenv("AMUX_TEST_HOST_AUTH") != "1" {
		t.Skip("set AMUX_TEST_NETWORK=1 AMUX_TEST_HOST_AUTH=1 for real Codex HTTPS")
	}
	bin, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	bin, err = filepath.EvalSymlinks(bin)
	if err != nil {
		t.Fatal(err)
	}
	host := codexcfg.UserHome().Dir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("CODEX_HOME", host)
	t.Setenv("AMUX_JAIL", "on")
	s := store.Session{ID: "codex-https", Agent: "codex", Dir: filepath.Join(home, "own")}
	spec := testLaunchSpec(t, s)
	if _, err := cfghome.Seed(codexcfg.Template(s.ID, s.Dir)); err != nil {
		t.Fatal(err)
	}
	argv, err := scope(s.Dir, TabAgent, s, spec.Access, nil, []string{bin, "app-server", "--listen", "stdio://"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = s.Dir
	cmd.Env, err = launchenv.Build(os.Environ(), append(wsops.AgentEnv(s), platformLaunchEnv(spec)...), launchenv.ForRuntime(s.Agent))
	if err != nil {
		t.Fatal(err)
	}
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = input.Close(); cancel(); _ = cmd.Wait() }()
	fmt.Fprintln(input, `{"id":1,"method":"initialize","params":{"clientInfo":{"name":"amux_https_test","version":"1"}}}`)
	scan := bufio.NewScanner(output)
	scan.Buffer(make([]byte, 4096), 1<<20)
	for scan.Scan() {
		var msg struct {
			ID    int `json:"id"`
			Error *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
			Result json.RawMessage `json:"result"`
		}
		if json.Unmarshal(scan.Bytes(), &msg) != nil {
			continue
		}
		if msg.ID != 1 && msg.ID != 2 {
			continue
		}
		if msg.Error != nil {
			t.Fatalf("Codex request %d failed: code=%d message=%s", msg.ID, msg.Error.Code, msg.Error.Message)
		}
		if msg.ID == 1 {
			fmt.Fprintln(input, `{"method":"initialized"}`)
			fmt.Fprintln(input, `{"id":2,"method":"account/rateLimits/read"}`)
		} else if len(msg.Result) > 0 {
			t.Log("sandbox Codex authenticated HTTPS account request succeeded")
			return
		}
	}
	t.Fatalf("Codex exited before HTTPS response: context=%v scan=%v", ctx.Err(), scan.Err())
}
