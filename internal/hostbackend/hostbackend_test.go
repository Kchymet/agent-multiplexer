package hostbackend_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"amux/internal/core"
	"amux/internal/daemon"
	"amux/internal/hostbackend"
	"amux/internal/hostbackend/hostbackendtest"
	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

// Every one-shot operation authenticates its own connection and closes it, so
// a rotated or revoked host credential governs the very next call: nothing
// reuses an earlier authorization.
func TestOperationsAuthenticateFreshConnectionPerCall(t *testing.T) {
	first, second, third := hostbackendtest.New("a"), hostbackendtest.New("b"), hostbackendtest.New("c")
	first.SnapshotFn = func() ([]core.Session, error) { return []core.Session{{ID: "s1"}}, nil }
	second.DispatchFn = func(core.Action) (string, error) { return "new-id", nil }
	third.RuntimeRecordFn = func(id string) (core.RuntimeRecord, error) { return core.RuntimeRecord{Path: "/t/" + id}, nil }
	d := hostbackendtest.NewDialer(first, second, third)
	h := d.Backend()
	ctx := context.Background()

	sessions, err := h.Snapshot(ctx)
	if err != nil || len(sessions) != 1 || sessions[0].ID != "s1" {
		t.Fatalf("Snapshot = %v, %v", sessions, err)
	}
	id, err := h.Apply(ctx, core.Action{Action: core.ActionStart, ID: "s1"})
	if err != nil || id != "new-id" {
		t.Fatalf("Apply = %q, %v", id, err)
	}
	rec, err := h.RuntimeRecord(ctx, "s1")
	if err != nil || rec.Path != "/t/s1" {
		t.Fatalf("RuntimeRecord = %+v, %v", rec, err)
	}
	if d.Dials() != 3 {
		t.Fatalf("dials = %d, want one per call", d.Dials())
	}
	for _, c := range []*hostbackendtest.Conn{first, second, third} {
		if !c.IsClosed() {
			t.Fatalf("connection %s left open after its call", c.Label)
		}
	}
	if got := second.Calls(); !reflect.DeepEqual(got, []string{"dispatch:" + core.ActionStart, "close"}) {
		t.Fatalf("apply calls = %v", got)
	}

	// Revocation: the next dial fails authentication. No earlier connection is
	// reused, and the relay-visible text is unchanged.
	d.SetFail(errors.New("host proof rejected"))
	_, err = h.Snapshot(ctx)
	if !errors.Is(err, hostbackend.ErrUnavailable) || !strings.HasPrefix(err.Error(), "daemon unreachable: ") {
		t.Fatalf("revoked Snapshot error = %v", err)
	}
	if got := first.Calls(); len(got) != 2 {
		t.Fatalf("revoked call reached a closed earlier connection: %v", got)
	}
}

func TestCancelledContextDoesNotDial(t *testing.T) {
	d := hostbackendtest.NewDialer(hostbackendtest.New("unused"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h := d.Backend()
	if _, err := h.Apply(ctx, core.Action{Action: core.ActionStart}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Apply = %v", err)
	}
	if _, err := h.OpenPane(ctx, hostbackend.PaneRequest{Agent: "a"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("OpenPane = %v", err)
	}
	if _, err := h.OpenAccounts(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("OpenAccounts = %v", err)
	}
	if d.Dials() != 0 {
		t.Fatalf("cancelled operations dialed %d times", d.Dials())
	}
}

// Cancellation (for example a relay connection revoked mid-verb) closes the
// authenticated transport, waits for the in-flight callback to observe that,
// and discards its late result rather than reporting it.
func TestCancelClosesTransportAndJoinsCallback(t *testing.T) {
	c := hostbackendtest.New("verb")
	entered := make(chan struct{})
	c.DispatchFn = func(core.Action) (string, error) {
		close(entered)
		<-c.Closed()
		return "late-id", nil
	}
	d := hostbackendtest.NewDialer(c)
	ctx, cancel := context.WithCancel(context.Background())
	type out struct {
		id  string
		err error
	}
	done := make(chan out, 1)
	go func() {
		id, err := d.Backend().Apply(ctx, core.Action{Action: core.ActionStart})
		done <- out{id, err}
	}()
	<-entered
	cancel()
	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) || got.id != "" {
			t.Fatalf("cancelled Apply = %q, %v; want no late id", got.id, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled Apply did not return")
	}
	if !c.IsClosed() {
		t.Fatal("cancellation did not close the transport")
	}
}

func TestJoinWaitsForCallbackBeforeReturning(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	finished := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		_, err := hostbackend.Join(ctx, func() {}, func() (int, error) {
			<-release
			close(finished)
			return 1, nil
		})
		result <- err
	}()
	cancel()
	select {
	case err := <-result:
		t.Fatalf("Join returned before callback exit: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Join error = %v", err)
	}
	select {
	case <-finished:
	default:
		t.Fatal("Join returned while its callback was still running")
	}
}

func TestAccountsConnectionLifetime(t *testing.T) {
	c := hostbackendtest.New("accounts")
	c.AccountsFn = func(r hp.AccountRequest, ceremony bool) (hp.AccountResult, error) {
		if ceremony {
			t.Errorf("status call marked as ceremony")
		}
		return hp.AccountResult{OK: true}, nil
	}
	d := hostbackendtest.NewDialer(c)
	ctx, cancel := context.WithCancel(context.Background())
	a, err := d.Backend().OpenAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res, err := a.Call(context.Background(), hp.AccountRequest{Operation: hp.AccountStatus}, false); err != nil || !res.OK {
		t.Fatalf("Call = %+v, %v", res, err)
	}
	dead, stop := context.WithCancel(context.Background())
	stop()
	if _, err := a.Call(dead, hp.AccountRequest{Operation: hp.AccountStatus}, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Call = %v", err)
	}
	cancel() // the owning connection's lifetime ends
	select {
	case <-c.Closed():
	case <-time.After(2 * time.Second):
		t.Fatal("account connection outlived its context")
	}
	if got := c.Calls(); !reflect.DeepEqual(got[:1], []string{"accounts:" + hp.AccountStatus}) {
		t.Fatalf("account calls = %v", got)
	}

	// Explicit Close works without a cancelled context.
	c2 := hostbackendtest.New("accounts2")
	a2, err := hostbackendtest.NewDialer(c2).Backend().OpenAccounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_ = a2.Close()
	if !c2.IsClosed() {
		t.Fatal("Close did not close the account connection")
	}
}

func TestPaneStreamsOnlyItsFramesAndClosesIdempotently(t *testing.T) {
	c := hostbackendtest.New("pane")
	d := hostbackendtest.NewDialer(c)
	p, err := d.Backend().OpenPane(context.Background(), hostbackend.PaneRequest{Agent: "agent-1", Tab: 0, Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	opened := c.Calls()
	if len(opened) != 1 || !strings.HasPrefix(opened[0], "pane-open:") || !strings.HasSuffix(opened[0], ":agent-1") {
		t.Fatalf("open calls = %v", opened)
	}
	paneID := strings.Split(opened[0], ":")[1]
	c.Frames <- daemon.Frame{Snapshot: &core.Snapshot{}}
	c.Frames <- daemon.Frame{Pane: &core.PaneFrame{PaneID: "other", Data: []byte("x")}}
	c.Frames <- daemon.Frame{Pane: &core.PaneFrame{PaneID: paneID, Data: []byte("mine")}}
	f, err := p.Next(context.Background())
	if err != nil || string(f.Data) != "mine" {
		t.Fatalf("Next = %+v, %v", f, err)
	}
	if err := p.Input([]byte("k")); err != nil {
		t.Fatal(err)
	}
	if err := p.Resize(100, 30); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
	if _, err := p.Next(context.Background()); err == nil {
		t.Fatal("Next succeeded after detach")
	}
	if err := p.Input([]byte("late")); err == nil {
		t.Fatal("input accepted after detach")
	}
}

func TestOpenPaneFailureClosesConnection(t *testing.T) {
	d := hostbackendtest.NewDialer()
	if _, err := d.Backend().OpenPane(context.Background(), hostbackend.PaneRequest{Agent: "a"}); err == nil ||
		!strings.Contains(err.Error(), "authenticate primary daemon pane relay") {
		t.Fatalf("OpenPane without credential = %v", err)
	}
}
