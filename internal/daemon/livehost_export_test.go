package daemon

import (
	"context"
	"net"
	"testing"
	"time"

	"amux/internal/access"
	"amux/internal/amuxcfg"
	"amux/internal/codexapp"
	"amux/internal/engine"
	"amux/internal/panespec"
	"amux/internal/source"
	"amux/internal/store"
)

// LiveHost is a real daemon for cross-package protocol tests (package
// daemon_test): the production workspace source over an isolated store, the
// production action/steer/query dispatch, and host-authenticated clients over
// the real wire. Only the runtime engine is fake (it records keystrokes
// instead of owning a PTY), so no harness binary or model is needed.
type LiveHost struct {
	d          *Daemon
	eng        *fakeEngine
	credential access.Credential
}

// NewLiveHostForTest starts the daemon's poll loop on an isolated HOME.
func NewLiveHostForTest(t *testing.T) *LiveHost {
	t.Helper()
	d, eng := steerDaemon(t)
	d.sources = []source.Source{source.NewWorkspace()}
	// Tests call Poll explicitly; a fast background poll only adds SQLite
	// contention with the actions under test.
	d.interval = 250 * time.Millisecond
	var err error
	d.authority, err = access.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.authority.EnsureHost(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.authority.Close() })
	cred, err := access.LoadCredential(d.authority.CredentialDir(access.SubjectHost, access.LocalHostSubject))
	if err != nil {
		t.Fatal(err)
	}
	// Create and migrate the store once before any concurrent opener: two
	// first opens of a fresh database race the schema migration (a separate,
	// pre-existing store issue), which is not what these tests exercise.
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.pollLoop(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return &LiveHost{d: d, eng: eng, credential: cred}
}

// Dial opens one host-authenticated client connection to the daemon.
func (h *LiveHost) Dial(ctx context.Context) (*Client, error) {
	srv, cli := net.Pipe()
	go h.d.serve(context.Background(), srv)
	type res struct {
		c   *Client
		err error
	}
	ch := make(chan res, 1)
	go func() { c, err := authenticateClient(cli, h.credential); ch <- res{c, err} }()
	select {
	case r := <-ch:
		if r.err != nil {
			_ = cli.Close()
		}
		return r.c, r.err
	case <-ctx.Done():
		_ = cli.Close()
		return nil, ctx.Err()
	}
}

// Running reports whether the session's agent runtime is up.
func (h *LiveHost) Running(id string) bool {
	in, ok := h.eng.Lookup(engine.Key{AgentID: id, Tab: panespec.TabAgent})
	return ok && in.Alive()
}

// Typed is what the session's runtime received as keystrokes.
func (h *LiveHost) Typed(id string) string {
	in, ok := h.eng.Lookup(engine.Key{AgentID: id, Tab: panespec.TabAgent})
	if f, isFake := in.(*fakeInstance); ok && isFake {
		return f.written()
	}
	return ""
}

// Poll recomputes the inventory now.
func (h *LiveHost) Poll() { h.d.pollOnce(context.Background()) }

// AttachStructured switches the daemon to App Server control and attaches a
// real codexapp.Supervisor for session id, connected to the App Server at
// endpoint (a test fake). The supervisor's process is a placeholder; all
// protocol traffic goes to endpoint.
func (h *LiveHost) AttachStructured(t *testing.T, id, dir, endpoint string) {
	t.Helper()
	if h.d.codex == nil {
		ctx, cancel := context.WithCancel(context.Background())
		h.d.codex = codexapp.NewManager(ctx, "")
		t.Cleanup(func() { h.d.codex.Shutdown(); cancel() })
	}
	h.d.codexControl.Effective = amuxcfg.AppServer
	// Registered exactly as the daemon's own structured start does, so the
	// permission gate knows this runtime's generation.
	if _, _, err := h.d.publishPermissionRuntime(id, func() (any, error) {
		return h.d.codex.Ensure(t.Context(), id, dir, nil, []string{"sleep", "60"}, endpoint, "", "", "")
	}); err != nil {
		t.Fatal(err)
	}
}
