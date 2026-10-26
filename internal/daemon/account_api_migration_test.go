package daemon

import (
	"context"
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
func (p *migrationAPIProvider) SessionToken(context.Context, string, bool, string) (codexauth.Tokens, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return codexauth.Tokens{AccessToken: p.key, APIKey: true, Generation: 1}, nil
}
func (p *migrationAPIProvider) Status(context.Context) (codexauth.Account, error) {
	return codexauth.Account{Type: "apiKey"}, nil
}

// A v1 subscription binding with delivered evidence and a resumable thread:
// after an API-key login the running process is refused, and the next launch
// receives the key without any rebind, keeping the thread identity.
func TestCodexReplacementWithAPIKeyServesNextLaunch(t *testing.T) {
	row := store.Session{ID: "migration", Agent: "codex", Dir: t.TempDir()}
	d, _, _ := sessionRuntimeFixture(t, row)
	d.centralCodex = &centralCodex{provider: &migrationAPIProvider{}, lifetime: t.Context()}
	defer d.disconnectAccounts("", true)
	running, err := accounts.BeginLaunch(row.ID, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if err := accounts.Update(func(s *accounts.State) error {
		p := s.Profiles["codex:central"]
		p.Mode, p.State = "subscription", "ready"
		s.Profiles[p.ID] = p
		b := s.Bindings[row.ID]
		b.Launch, b.Pending = running.Seq, nil
		b.CredentialMode, b.ExpectedAccount, b.DeliveredEpoch = "subscription", "acct-old", p.DeliveryEpoch
		s.Bindings[row.ID] = b
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := codexapp.SaveIdentity(codexapp.Identity{SessionID: row.ID, ThreadID: "existing-thread", Resumable: true, ChatGPTAccount: "acct-old"}); err != nil {
		t.Fatal(err)
	}
	loginAPIForTest(t, d, "codex", "synthetic-migration-key")
	source := codexTokenSource{d: d}
	if token, err := source.CodexChatGPTToken(t.Context(), codexapp.TokenRequest{SessionID: row.ID, Refresh: true, PreviousAccountID: "acct-old", Launch: running}); err == nil || token.AccessToken != "" {
		t.Fatal("running subscription process adopted the API key")
	}
	next, err := accounts.BeginLaunch(row.ID, "codex")
	if err != nil {
		t.Fatal(err)
	}
	token, err := source.CodexChatGPTToken(t.Context(), codexapp.TokenRequest{SessionID: row.ID, Launch: next})
	if err != nil || !token.APIKey {
		t.Fatal("next launch did not receive the API key", err)
	}
	if token, err := source.CodexChatGPTToken(t.Context(), codexapp.TokenRequest{SessionID: row.ID, Launch: running}); err == nil || token.AccessToken != "" {
		t.Fatal("superseded process received a credential")
	}
	if id, ok := codexapp.LoadIdentity(row.ID); !ok || id.ThreadID != "existing-thread" {
		t.Fatalf("thread identity changed: %+v", id)
	}
}
