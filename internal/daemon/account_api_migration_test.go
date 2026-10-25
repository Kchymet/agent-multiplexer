package daemon

import (
	"context"
	"sync"
	"testing"

	"amux/internal/accounts"
	"amux/internal/codexapp"
	"amux/internal/codexauth"
	"amux/internal/store"
	hp "github.com/kchymet/agent-multiplexer/harnessproto"
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

// Reproduce the review finding through real mutation, migration and delivery,
// without pre-setting CredentialUnknown. The old schema has no mode/version.
func TestUnknownLegacyCodexCannotAdoptAPI(t *testing.T) {
	for _, state := range []string{"blocked", "logged-out", "ready", "unconfigured", "missing-binding", "missing-binding-updated", "fresh", "fresh-replaced-before-delivery"} {
		t.Run(state, func(t *testing.T) {
			row := store.Session{ID: "migration", Agent: "codex", Dir: t.TempDir()}
			d, _, _ := sessionRuntimeFixture(t, row)
			d.centralCodex = &centralCodex{provider: &migrationAPIProvider{}, lifetime: t.Context()}
			defer d.disconnectAccounts("", true)
			if state != "fresh" && state != "fresh-replaced-before-delivery" {
				if err := accounts.Update(func(s *accounts.State) error {
					p := s.Profiles["codex:central"]
					p.Mode = ""
					p.State = state
					if state == "missing-binding" || state == "missing-binding-updated" {
						p.State = "unconfigured"
					}
					s.Profiles[p.ID] = p
					s.Bindings[row.ID] = accounts.Binding{ProfileID: p.ID, Harness: "codex", Source: "central", Revision: 1, Origin: "machine-default"}
					if state == "missing-binding" || state == "missing-binding-updated" {
						delete(s.Bindings, row.ID)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if err := codexapp.SaveIdentity(codexapp.Identity{SessionID: row.ID, ThreadID: "existing-thread", Resumable: true}); err != nil {
					t.Fatal(err)
				}
			}
			if state == "missing-binding-updated" {
				db, err := store.Open()
				if err != nil {
					t.Fatal(err)
				}
				err = db.PutSession(row)
				_ = db.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			loginAPIForTest(t, d, "codex", "synthetic-migration-key")
			if state == "fresh-replaced-before-delivery" {
				loginAPIForTest(t, d, "codex", "synthetic-replacement-key")
			}
			source := codexTokenSource{d: d}
			token, err := source.CodexChatGPTToken(t.Context(), codexapp.TokenRequest{SessionID: row.ID})
			if state == "fresh" {
				if err != nil || !token.APIKey {
					t.Fatal("genuinely new workload lost first login", err)
				}
				return
			}
			if err == nil || token.AccessToken != "" {
				t.Fatal("existing workload adopted replacement API key without rebind")
			}
			// Discovery/restart cannot wash away the unknown-history fence.
			if err := accounts.BindExisting(row.ID, "codex"); err != nil {
				t.Fatal(err)
			}
			if _, err := source.CodexChatGPTToken(t.Context(), codexapp.TokenRequest{SessionID: row.ID}); err == nil {
				t.Fatal("discovery removed fence")
			}
			out := d.accountCall(t.Context(), hp.AccountRequest{Operation: hp.AccountRebind, ProfileID: "codex:central", Revision: profileRevision(t, "codex:central"), WorkloadID: row.ID}, false)
			if !out.OK {
				t.Fatal(out)
			}
			token, err = source.CodexChatGPTToken(t.Context(), codexapp.TokenRequest{SessionID: row.ID})
			if err != nil || !token.APIKey {
				t.Fatal("explicit rebind did not recover", err)
			}
		})
	}
}
