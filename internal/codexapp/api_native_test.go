package codexapp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"amux/internal/codexauth"
	"amux/internal/codexcfg"
)

// Only initialize/login/account-read: no thread, turn, live account or model call.
func TestSmokeAPIKeyNativeOwnerAndMemoryDelivery(t *testing.T) {
	bin := os.Getenv("AMUX_TEST_CODEX_BIN")
	if bin == "" {
		t.Skip("set AMUX_TEST_CODEX_BIN to CI-pinned Codex 0.157.1")
	}
	version, err := exec.Command(bin, "--version").Output()
	if err != nil || strings.TrimSpace(string(version)) != "codex-cli 0.157.1" {
		t.Fatalf("expected pinned 0.157.1: %q %v", version, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	root := t.TempDir()
	env := []string{"HOME=" + root, "PATH=/usr/bin:/bin", "XDG_STATE_HOME=" + root, "XDG_RUNTIME_DIR=" + root}
	central := filepath.Join(root, "central")
	p, err := codexauth.New(codexauth.Config{Argv: []string{bin}, Home: central, Env: env})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	const key = "sk-synthetic-amux-API-SECRET-no-live-account"
	if err := p.LoginAPIKey(ctx, key); err != nil {
		t.Fatal(err)
	}
	acct, err := p.Status(ctx)
	if err != nil || acct.Type != "apiKey" || acct.Email != "" || acct.PlanType != "" {
		t.Fatalf("native account/read: %+v %v", acct, err)
	}
	home := filepath.Join(root, "session")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	endpoint := "unix://" + filepath.Join(root, "s.sock")
	argv := codexcfg.CentralChatGPTEndpoint(AppServerArgv(bin, endpoint))
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = append(env, "CODEX_HOME="+home)
	cmd.Dir = root
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	sup := New(Config{SessionID: "api-native", Endpoint: endpoint, EventLogPath: filepath.Join(root, "events.jsonl"), ChatGPTAuth: providerFunc(func(ctx context.Context, r TokenRequest) (ChatGPTToken, error) {
		tok, err := p.SessionToken(ctx, r.SessionID, r.Refresh, r.PreviousAccountID)
		return ChatGPTToken{AccessToken: tok.AccessToken, APIKey: tok.APIKey, AccountID: tok.AccountID, PlanType: tok.PlanType}, err
	})})
	defer sup.Close()
	conn, err := sup.dialWithRetry(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := sup.attachTransport(ctx, conn); err != nil {
		t.Fatal(err)
	}
	if err := sup.initialize(ctx); err != nil {
		t.Fatal(err)
	}
	raw, err := sup.rpc.call(ctx, "account/read", map[string]any{"refreshToken": false})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Account struct {
			Type     string `json:"type"`
			PlanType string `json:"planType"`
			Email    string `json:"email"`
		} `json:"account"`
	}
	if json.Unmarshal(raw, &got) != nil || got.Account.Type != "apiKey" || got.Account.PlanType != "" || got.Account.Email != "" {
		t.Fatalf("workload API account/read: %s", raw)
	}
	if _, err := sup.auth.refresh(ctx); !errors.Is(err, errNotInjected) {
		t.Fatal("API mode claimed ChatGPT refresh identity", err)
	}
	if _, err := os.Stat(filepath.Join(home, "auth.json")); !os.IsNotExist(err) {
		t.Fatal("workload persisted credential instead of memory-only delivery", err)
	}
	assertNoSecrets(t, sup)
	t.Log("Codex 0.157.1: native API login -> protected central store -> private workload login -> account/read apiKey; no account ID, plan, refresh or workload auth.json; no thread/turn RPC")
}
