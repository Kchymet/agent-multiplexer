package daemon

import (
	"context"
	"errors"
	"sync"
	"testing"

	"amux/internal/accounts"
	"amux/internal/codexapp"
	"amux/internal/codexauth"
	"amux/internal/store"
)

type migrationAPIProvider struct {
	blockingAccountProvider
	mu  sync.Mutex
	key string
}

func (p *migrationAPIProvider) LoginAPIKey(ctx context.Context, key string, commit ...func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	previous := p.key
	p.key = key
	p.mu.Unlock()
	for _, admit := range commit {
		if err := admit(); err != nil {
			p.mu.Lock()
			p.key = previous
			p.mu.Unlock()
			return err
		}
	}
	return nil
}

// Before an API-key login the provider holds a synthetic subscription login.
func (p *migrationAPIProvider) SessionToken(context.Context, string, bool, string) (codexauth.Tokens, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.key == "" {
		return codexauth.Tokens{AccessToken: "synthetic-subscription-access", AccountID: "acct-old", Generation: 1}, nil
	}
	return codexauth.Tokens{AccessToken: p.key, APIKey: true, Generation: 1}, nil
}
func (p *migrationAPIProvider) Status(context.Context) (codexauth.Account, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.key == "" {
		return codexauth.Account{Type: "chatgpt"}, nil
	}
	return codexauth.Account{Type: "apiKey"}, nil
}

// A running subscription process with a resumable thread: after an API-key
// login the running process is refused as superseded (never given the key),
// and the next launch receives the key without any rebind, keeping the thread.
func TestCodexReplacementWithAPIKeyServesNextLaunch(t *testing.T) {
	row := store.Session{ID: "migration", Agent: "codex", Dir: t.TempDir()}
	d, _, _ := sessionRuntimeFixture(t, row)
	d.centralCodex = &centralCodex{provider: &migrationAPIProvider{}, lifetime: t.Context()}
	defer d.disconnectAccounts("", true)
	if err := accounts.Update(func(s *accounts.State) error {
		p := s.Profiles["codex:central"]
		p.Mode, p.State = "subscription", "ready"
		s.Profiles[p.ID] = p
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := codexapp.SaveIdentity(codexapp.Identity{SessionID: row.ID, ThreadID: "existing-thread", Resumable: true, ChatGPTAccount: "acct-old"}); err != nil {
		t.Fatal(err)
	}
	source := codexTokenSource{d: d}
	running, err := accounts.BeginLaunch(row.ID, "codex")
	if err != nil {
		t.Fatal(err)
	}
	// The running process's real first delivery and a same-account refresh.
	if token, err := source.CodexChatGPTToken(t.Context(), codexapp.TokenRequest{SessionID: row.ID, Launch: running}); err != nil || token.APIKey || token.AccountID != "acct-old" {
		t.Fatalf("initial subscription delivery = %+v, %v", token, err)
	}
	if token, err := source.CodexChatGPTToken(t.Context(), codexapp.TokenRequest{SessionID: row.ID, Refresh: true, PreviousAccountID: "acct-old", Launch: running}); err != nil || token.AccountID != "acct-old" {
		t.Fatalf("same-account refresh = %+v, %v", token, err)
	}

	loginAPIForTest(t, d, "codex", "synthetic-migration-key")
	for _, req := range []codexapp.TokenRequest{
		{SessionID: row.ID, Refresh: true, PreviousAccountID: "acct-old", Launch: running}, // 401 refresh
		{SessionID: row.ID, PreviousAccountID: "acct-old", Launch: running},                // reconnect
	} {
		token, err := source.CodexChatGPTToken(t.Context(), req)
		if !errors.Is(err, accounts.ErrSuperseded) || token.AccessToken != "" {
			t.Fatalf("running subscription process (refresh=%v) = %+v, %v; want superseded", req.Refresh, token, err)
		}
	}
	next, err := accounts.BeginLaunch(row.ID, "codex")
	if err != nil {
		t.Fatal(err)
	}
	token, err := source.CodexChatGPTToken(t.Context(), codexapp.TokenRequest{SessionID: row.ID, Launch: next})
	if err != nil || !token.APIKey || token.AccountID != "" {
		t.Fatal("next launch did not receive the API key", err)
	}
	if token, err := source.CodexChatGPTToken(t.Context(), codexapp.TokenRequest{SessionID: row.ID, Launch: running}); !errors.Is(err, accounts.ErrSuperseded) || token.AccessToken != "" {
		t.Fatalf("old process after the next launch = %+v, %v", token, err)
	}
	if id, ok := codexapp.LoadIdentity(row.ID); !ok || id.ThreadID != "existing-thread" {
		t.Fatalf("thread identity changed: %+v", id)
	}
}
