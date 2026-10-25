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

type observedServerClose struct {
	net.Conn
	closed chan struct{}
	once   sync.Once
}

func (c *observedServerClose) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { close(c.closed) })
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
	pipeServer, raw := net.Pipe()
	srv := &observedServerClose{Conn: pipeServer, closed: make(chan struct{})}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); d.serve(ctx, srv) }()
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
	// cannot enter until we release effectMu. Disconnect before that admission.
	raw.Close()
	// Client close does not order the server reader goroutine. Observe the
	// server watcher closing its transport after EOF cancels connectionCtx,
	// while effect admission is still held. The old synchronous reader cannot
	// reach this observation and fails the bounded wait.
	select {
	case <-srv.closed:
	case <-ctx.Done():
		d.effectMu.Unlock()
		<-done
		t.Fatal("server did not observe disconnect while effect admission was held")
	}
	d.effectMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
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
