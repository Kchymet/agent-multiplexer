package daemon

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"amux/internal/access"
	"amux/internal/accounts"
	"amux/internal/centralauth"
	"amux/internal/console"
	"amux/internal/core"
	"amux/internal/store"
	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

func consoleAccountFixture(t *testing.T) (*Daemon, access.SessionAccess, access.Credential) {
	t.Helper()
	isolatedAccounts(t)
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	d := New("", nil, time.Hour)
	d.authority, err = access.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = console.Ensure(); err != nil {
		t.Fatal(err)
	}
	grant, err := d.authority.EnsureSession(t.Context(), console.ID, console.Dir())
	if err != nil {
		t.Fatal(err)
	}
	credential, err := access.LoadCredential(grant.CredentialHostDir)
	if err != nil {
		t.Fatal(err)
	}
	closeEndpoint, err := d.listenConsoleAccounts(t.Context(), grant.MailboxHostDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeEndpoint(); d.drainServingConnections(); d.authority.Close() })
	return d, grant, credential
}
func consoleAccountClient(t *testing.T, g access.SessionAccess, c access.Credential) *Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	client, err := dialConsoleAccounts(ctx, g.MailboxHostDir, c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}
func TestConsoleAccountChannelConfiguresLocallyWithoutMailboxRecords(t *testing.T) {
	_, g, credential := consoleAccountFixture(t)
	c := consoleAccountClient(t, g, credential)
	before, err := os.ReadDir(g.MailboxHostDir)
	if err != nil {
		t.Fatal(err)
	}
	status, err := c.Accounts(hp.AccountRequest{Operation: hp.AccountStatus}, false)
	if err != nil || !status.OK || len(status.Snapshot.Profiles) != 2 {
		t.Fatalf("status %+v %v", status, err)
	}
	login, err := c.Accounts(hp.AccountRequest{Operation: hp.AccountLogin, ProfileID: "claude:central", Revision: profileRevision(t, "claude:central"), Initiator: "console-cli"}, true)
	if err != nil || !login.OK {
		t.Fatalf("login %+v %v", login, err)
	}
	complete, err := c.Accounts(hp.AccountRequest{Operation: hp.AccountComplete, ProfileID: "claude:central", AttemptID: login.Ceremony.AttemptID, Initiator: "console-cli", Secret: "SYNTHETIC-CONSOLE-TOKEN"}, true)
	if err != nil || !complete.OK {
		t.Fatalf("complete %+v %v", complete, err)
	}
	st, err := centralauth.ClaudeStore().Status()
	if err != nil || !st.Configured {
		t.Fatalf("credential not persisted: %+v %v", st, err)
	}
	after, err := os.ReadDir(g.MailboxHostDir)
	if err != nil || len(after) != len(before) {
		t.Fatal("account call wrote mailbox records", err)
	}
	for _, dir := range []string{"requests", "responses"} {
		items, err := os.ReadDir(filepath.Join(g.MailboxHostDir, dir))
		if err != nil || len(items) != 0 {
			t.Fatalf("ceremony entered %s: %v %v", dir, items, err)
		}
	}
}
func TestConsoleAccountChannelRejectsOtherPrincipalsAndGenericQueries(t *testing.T) {
	d, g, credential := consoleAccountFixture(t)
	for _, kind := range []access.SubjectKind{access.SubjectSession, access.SubjectProvider, access.SubjectHost} {
		id := "ordinary"
		if kind == access.SubjectHost {
			id = access.LocalHostSubject
		}
		dir, err := d.authority.Ensure(t.Context(), kind, id)
		if err != nil {
			t.Fatal(err)
		}
		other, err := access.LoadCredential(dir)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		client, err := dialConsoleAccounts(ctx, g.MailboxHostDir, other)
		cancel()
		if err == nil {
			client.Close()
			t.Fatalf("accepted %s principal", kind)
		}
	}
	for _, a := range []core.Action{{Action: core.ActionQuery, Query: core.QuerySessions}, {Action: core.ActionNewWorkgroup}, {Action: core.ActionPaneOpen, ID: "console"}} {
		c := consoleAccountClient(t, g, credential)
		if err := c.Send(a); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { _, err := c.Next(); done <- err }()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("generic operation received protected frame")
			}
		case <-time.After(time.Second):
			t.Fatal("generic operation not closed")
		}
	}
}
func TestConsoleQueuedCompletionFencedByDisconnectAndRevocation(t *testing.T) {
	for _, retire := range []string{"disconnect", "revoke"} {
		t.Run(retire, func(t *testing.T) {
			d, _, credential := consoleAccountFixture(t)
			rawServer, rawClient := net.Pipe()
			server := &observedServerClose{Conn: rawServer, closed: make(chan struct{})}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			done := make(chan struct{})
			go func() { defer close(done); d.serveAuthenticated(ctx, server, true) }()
			c, err := authenticateClientContext(ctx, rawClient, credential)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			login, err := c.Accounts(hp.AccountRequest{Operation: hp.AccountLogin, ProfileID: "claude:central", Revision: profileRevision(t, "claude:central"), Initiator: "console"}, true)
			if err != nil || !login.OK {
				t.Fatalf("%+v %v", login, err)
			}
			req := hp.AccountRequest{Operation: hp.AccountComplete, ProfileID: "claude:central", AttemptID: login.Ceremony.AttemptID, Initiator: "console", Secret: "SYNTHETIC-LATE-CONSOLE-TOKEN"}
			body, _ := json.Marshal(req)
			d.effectMu.Lock()
			err = json.NewEncoder(c.conn).Encode(core.Action{Action: core.ActionQuery, Query: core.QueryAccountCeremony, Fields: map[string]string{"request": string(body)}})
			if err != nil {
				d.effectMu.Unlock()
				t.Fatal(err)
			}
			if retire == "disconnect" {
				rawClient.Close()
			} else if err = d.authority.Revoke(ctx, access.SubjectSession, console.ID); err != nil {
				d.effectMu.Unlock()
				t.Fatal(err)
			}
			select {
			case <-server.closed:
				if ctx.Err() != nil {
					d.effectMu.Unlock()
					<-done
					t.Fatal("fixture deadline retired server")
				}
			case <-time.After(time.Second):
				d.effectMu.Unlock()
				cancel()
				<-done
				t.Fatal("connection not retired before admission")
			}
			d.effectMu.Unlock()
			<-done
			status, err := centralauth.ClaudeStore().Status()
			if err != nil || status.Configured {
				t.Fatal("retired completion persisted token", err)
			}
			state, err := accounts.Read()
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(state.Profiles["claude:central"].Error, "SYNTHETIC") {
				t.Fatal("secret leaked")
			}
		})
	}
}
