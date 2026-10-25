package daemon

import (
	"amux/internal/access"
	"amux/internal/centralauth"
	"amux/internal/core"
	"context"
	"encoding/json"
	hp "github.com/kchymet/agent-multiplexer/harnessproto"
	"net"
	"sync"
	"testing"
	"time"
)

type accountRetirementObservedConn struct {
	net.Conn
	retired chan struct{}
	once    sync.Once
}

func (c *accountRetirementObservedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { close(c.retired) })
	return err
}

// This regression fails on affcf94: synchronous dispatch prevents the read loop
// from observing disconnect while a completion waits for effect admission.
// The fixture uses an authenticated transport and a synthetic token only.
func TestAccountCompletionQueuedBehindEffectLockRejectsDisconnectedInitiator(t *testing.T) {
	isolatedAccounts(t)
	d := New("", nil, time.Hour)
	var err error
	d.authority, err = access.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer d.authority.Close()
	dir, err := d.authority.EnsureHost(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	credential, err := access.LoadCredential(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv, raw := net.Pipe()
	observed := &accountRetirementObservedConn{Conn: srv, retired: make(chan struct{})}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); d.serve(ctx, observed) }()
	c, err := authenticateClientContext(ctx, raw, credential)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	out, err := c.Accounts(hp.AccountRequest{Operation: hp.AccountLogin, ProfileID: "claude:central", Revision: profileRevision(t, "claude:central"), Initiator: "review"}, true)
	if err != nil || !out.OK {
		t.Fatalf("login: %+v %v", out, err)
	}
	req := hp.AccountRequest{Operation: hp.AccountComplete, ProfileID: "claude:central", AttemptID: out.Ceremony.AttemptID, Initiator: "review", Secret: "SYNTHETIC-REVIEW-TOKEN"}
	b, _ := json.Marshal(req)
	d.effectMu.Lock()
	err = json.NewEncoder(c.conn).Encode(core.Action{Action: core.ActionQuery, Query: core.QueryAccountCeremony, Fields: map[string]string{"request": string(b)}})
	// The full authenticated completion frame has been read, but its mutation
	// cannot enter until we release effectMu. Peer Close alone does not prove
	// server observation. The server's connection watcher closes its transport
	// after connection-context cancellation; wait for that retirement barrier.
	raw.Close()
	if err != nil {
		d.effectMu.Unlock()
		t.Fatal(err)
	}
	select {
	case <-observed.retired:
		if ctx.Err() != nil {
			d.effectMu.Unlock()
			t.Fatal("server retired only after fixture lifetime cancellation")
		}
	case <-time.After(time.Second):
		d.effectMu.Unlock()
		cancel()
		<-done
		t.Fatal("server did not observe disconnect while account admission was blocked")
	}
	d.effectMu.Unlock()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("serve did not terminate")
	}
	st, err := centralauth.ClaudeStore().Status()
	if err != nil {
		t.Fatal(err)
	}
	if st.Configured {
		t.Fatal("completion persisted token after initiator disconnected before effect admission")
	}
}
