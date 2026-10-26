package sessionrpc

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"amux/internal/access"
	"golang.org/x/sys/unix"
)

// pumpUntil serves until operation returns. Unlike pumpClient it tolerates
// ErrResponseCapacity, which is the condition these tests exercise.
func pumpUntil(t *testing.T, servers []*Server, operation func(context.Context) (Result, error)) (Result, error, time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type outcome struct {
		result Result
		err    error
	}
	done := make(chan outcome, 1)
	started := time.Now()
	go func() {
		result, err := operation(ctx)
		done <- outcome{result, err}
	}()
	for {
		for _, server := range servers {
			if _, err := server.ServeOnce(ctx); err != nil && !errors.Is(err, ErrQueueFull) && !errors.Is(err, ErrResponseCapacity) && ctx.Err() == nil {
				t.Fatal(err)
			}
		}
		select {
		case got := <-done:
			return got.result, got.err, time.Since(started)
		case <-time.After(time.Millisecond):
		}
	}
}

func responseCount(t *testing.T, mailboxDir string) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(mailboxDir, ResponsesDirName))
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

// earlierSignedCall signs call as issued at issued. Request IDs begin with the
// issue millisecond, so a strictly earlier millisecond sorts ahead in the
// server's FIFO scan.
func earlierSignedCall(t *testing.T, f *fixture, call Call, issued time.Time) (access.SignedRequest, []byte) {
	t.Helper()
	credential, err := access.LoadCredential(f.grant.CredentialHostDir)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(call)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := access.SignRequest(credential, f.auth.BootID(), body, issued, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return envelope, encoded
}

// The production client releases each consumed response, so one response slot
// sustains any number of sequential calls instead of one per MaxResponseAge.
// An old client that never releases still exhausts the slot (negative control)
// and falls back to unchanged age cleanup.
func TestClientReleaseRetiresConsumedResponses(t *testing.T) {
	budget := &testResponseBudget{limit: 1}
	f := newFixture(t, Callbacks{}, ServerOptions{ResponseBudget: budget})
	client := f.client(clientOptions{})
	for i := 0; i < 20; i++ {
		result, err, elapsed := pumpUntil(t, []*Server{f.server}, func(ctx context.Context) (Result, error) {
			return client.Query(ctx, Query{Verb: "snapshot", Fields: map[string]string{"value": fmt.Sprint(i)}})
		})
		if err != nil || string(result.Body) != fmt.Sprintf("ok:%d", i) {
			t.Fatalf("call %d = %+v, %v", i, result, err)
		}
		if elapsed > time.Second {
			t.Fatalf("call %d waited %v for response capacity", i, elapsed)
		}
	}
	if _, err := f.server.ServeOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if active, _, _ := budget.counts(); active != 0 || responseCount(t, f.grant.MailboxHostDir) != 0 {
		t.Fatalf("consumed responses retained: active=%d files=%d", active, responseCount(t, f.grant.MailboxHostDir))
	}

	// Negative control: requests whose responses are never released.
	for i := 0; i < 2; i++ {
		envelope, encoded := signedCall(t, f.auth, f.grant.CredentialHostDir, Call{Kind: CallOperation, Route: access.RouteQuery, Verb: "snapshot"})
		publishEnvelope(t, f.grant.RequestsHostDir, envelope, encoded)
		_, err := f.server.ServeOnce(context.Background())
		if i == 0 && err != nil {
			t.Fatal(err)
		}
		if i == 1 && !errors.Is(err, ErrResponseCapacity) {
			t.Fatalf("unreleased response did not hold capacity: %v", err)
		}
	}
}

// A release must be processed even when an ordinary request ahead of it in
// the pass is blocked on the very capacity the release frees. Both places a
// request can be ahead are covered: already claimed into processing (drained
// before the queue), and queued with an earlier issue time (the FIFO order).
func TestReleaseProcessedBehindCapacityBlockedRequest(t *testing.T) {
	for _, stage := range []string{processingDirName, RequestsDirName} {
		t.Run(stage, func(t *testing.T) { testReleaseBehindBlocked(t, stage) })
	}
}

func testReleaseBehindBlocked(t *testing.T, stage string) {
	budget := &testResponseBudget{limit: 1}
	f := newFixture(t, Callbacks{}, ServerOptions{ResponseBudget: budget})
	client := f.client(clientOptions{})
	// Fixed before the client signs anything: the client's release is issued
	// at or after this instant, so a request issued a millisecond earlier
	// precedes it in FIFO order regardless of how long the call takes.
	ahead := time.Now().Add(-time.Millisecond)
	// Serve the call, but do not process the client's release yet.
	done := make(chan error, 1)
	go func() {
		_, err := client.Query(context.Background(), Query{Verb: "snapshot"})
		done <- err
	}()
	// Stop serving once the response exists so no pass can reach the release.
	for responseCount(t, f.grant.MailboxHostDir) == 0 {
		if _, err := f.server.ServeOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("client did not read its response")
	}
	if responseCount(t, f.grant.MailboxHostDir) != 1 {
		t.Fatal("expected the consumed response before its release is served")
	}
	blocked, encoded := earlierSignedCall(t, f, Call{Kind: CallOperation, Route: access.RouteQuery, Verb: "snapshot", Fields: map[string]string{"value": "blocked"}}, ahead)
	publishEnvelope(t, filepath.Join(f.grant.MailboxHostDir, stage), blocked, encoded)
	state := func() string {
		active, commits, releases := budget.counts()
		out := fmt.Sprintf("budget active=%d commits=%d releases=%d", active, commits, releases)
		for _, dir := range []string{RequestsDirName, processingDirName, ResponsesDirName} {
			entries, err := os.ReadDir(filepath.Join(f.grant.MailboxHostDir, dir))
			names := make([]string, 0, len(entries))
			for _, entry := range entries {
				names = append(names, entry.Name())
			}
			out += fmt.Sprintf(" %s=%v%v", dir, names, err)
		}
		// The server's own held-descriptor view, which ServeOnce scans.
		for label, dir := range map[string]*os.File{"held-requests": f.server.requests, "held-processing": f.server.processing} {
			names, more, err := listNames(dir, MaxQueuedRequests)
			out += fmt.Sprintf(" %s=%v more=%v %v", label, names, more, err)
		}
		entries, _ := os.ReadDir(filepath.Join(f.grant.MailboxHostDir, ResponsesDirName))
		for _, entry := range entries {
			var stat unix.Stat_t
			if err := unix.Lstat(filepath.Join(f.grant.MailboxHostDir, ResponsesDirName, entry.Name()), &stat); err == nil {
				out += fmt.Sprintf(" stat(%s)=nlink:%d mode:%o size:%d mtime:%s", entry.Name(), stat.Nlink, stat.Mode&0o7777, stat.Size, statModTime(stat).Format(time.RFC3339Nano))
			}
		}
		return out + " now:" + time.Now().Format(time.RFC3339Nano)
	}
	// Preconditions: the consumed response holds the only slot, the client's
	// release is queued, and the blocked request is ahead of it in pass order.
	before := state()
	if active, _, _ := budget.counts(); active != 1 {
		t.Fatalf("precondition: consumed response must hold the only slot: %s", before)
	}
	queued, err := os.ReadDir(f.grant.RequestsHostDir)
	want := 1
	if stage == RequestsDirName {
		want = 2
	}
	if err != nil || len(queued) != want || (stage == RequestsDirName && queued[0].Name() != requestFileName(blocked.RequestID)) {
		t.Fatalf("precondition: want the blocked request ahead of one release: %s", before)
	}

	if _, err := f.server.ServeOnce(context.Background()); !errors.Is(err, ErrResponseCapacity) {
		t.Fatalf("first pass = %v, want the blocked request capacity-blocked\nbefore: %s\nafter:  %s", err, before, state())
	}
	if responseCount(t, f.grant.MailboxHostDir) != 0 {
		t.Fatal("release queued behind a capacity-blocked request was not processed")
	}
	if _, err := f.server.ServeOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if response := readTestResponse(t, f.grant.MailboxHostDir, blocked.RequestID); string(response.Body) != "ok:blocked" {
		t.Fatalf("blocked request response = %+v", response)
	}
}

// Retirement requires proof of the exact signed response. A release never
// produces a response of its own, and a replayed release is rejected by the
// replay ledger without effect.
func TestReleaseRequiresExactOwnedResponse(t *testing.T) {
	f := newFixture(t, Callbacks{}, ServerOptions{})
	operation, encoded := signedCall(t, f.auth, f.grant.CredentialHostDir, Call{Kind: CallOperation, Route: access.RouteQuery, Verb: "snapshot"})
	publishEnvelope(t, f.grant.RequestsHostDir, operation, encoded)
	if _, err := f.server.ServeOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	response := readTestResponse(t, f.grant.MailboxHostDir, operation.RequestID)
	release := func(digest string) access.SignedRequest {
		envelope, encoded := signedCall(t, f.auth, f.grant.CredentialHostDir, Call{Kind: CallRelease, Receipt: &Receipt{RequestID: operation.RequestID, ResponseDigest: digest}})
		publishEnvelope(t, f.grant.RequestsHostDir, envelope, encoded)
		if _, err := f.server.ServeOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		return envelope
	}
	wrong := responseDigest(response)
	wrong = wrong[:len(wrong)-1] + string("0123456789abcdef"[(int(wrong[len(wrong)-1])+1)%16])
	release(wrong)
	if responseCount(t, f.grant.MailboxHostDir) != 1 {
		t.Fatal("release with a wrong digest retired the response")
	}
	accepted := release(responseDigest(response))
	if responseCount(t, f.grant.MailboxHostDir) != 0 {
		t.Fatal("exact release did not retire the response or produced its own")
	}
	// Recreate an identically named response and replay the accepted release.
	data, _ := json.Marshal(response)
	if err := os.WriteFile(filepath.Join(f.grant.MailboxHostDir, ResponsesDirName, responseFileName(operation.RequestID)), data, regularFileMode); err != nil {
		t.Fatal(err)
	}
	replayed, _ := json.Marshal(accepted)
	publishEnvelope(t, f.grant.RequestsHostDir, accepted, replayed)
	if _, err := f.server.ServeOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if responseCount(t, f.grant.MailboxHostDir) != 1 {
		t.Fatal("replayed release had an effect")
	}
}

// A response awaiting a mandatory receipt is never released. Its receipt is
// processed even with the response pool exhausted, and acceptance retires it.
func TestReceiptSettlesAndRetiresWithoutResponseCapacity(t *testing.T) {
	budget := &testResponseBudget{limit: 1}
	settled := make(chan ReceiptSettlement, 1)
	f := newFixture(t, Callbacks{
		Dispatch: func(context.Context, DispatchRequest) (DispatchResult, error) {
			return DispatchResult{Status: StatusOK, Receipt: &ReceiptHooks{
				Grace: 5 * time.Second, Settled: func(value ReceiptSettlement) { settled <- value },
			}}, nil
		},
	}, ServerOptions{ResponseBudget: budget})
	operation, encoded := signedCall(t, f.auth, f.grant.CredentialHostDir, Call{Kind: CallOperation, Route: access.RouteAction, Verb: "done", ID: f.grant.SubjectID})
	publishEnvelope(t, f.grant.RequestsHostDir, operation, encoded)
	if _, err := f.server.ServeOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	response := readTestResponse(t, f.grant.MailboxHostDir, operation.RequestID)
	if !response.ReceiptRequired {
		t.Fatal("expected a receipt-required response")
	}
	release, releaseBytes := signedCall(t, f.auth, f.grant.CredentialHostDir, Call{Kind: CallRelease, Receipt: &Receipt{RequestID: operation.RequestID, ResponseDigest: responseDigest(response)}})
	publishEnvelope(t, f.grant.RequestsHostDir, release, releaseBytes)
	if _, err := f.server.ServeOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if responseCount(t, f.grant.MailboxHostDir) != 1 {
		t.Fatal("release retired a response with a pending mandatory receipt")
	}
	if active, _, _ := budget.counts(); active != 1 {
		t.Fatalf("pool should be exhausted by the pending response: active=%d", active)
	}
	receipt, receiptBytes := signedCall(t, f.auth, f.grant.CredentialHostDir, Call{Kind: CallReceipt, Receipt: &Receipt{RequestID: operation.RequestID, ResponseDigest: responseDigest(response)}})
	publishEnvelope(t, f.grant.RequestsHostDir, receipt, receiptBytes)
	if _, err := f.server.ServeOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case value := <-settled:
		if !value.Received || value.Reason != SettlementReceipt {
			t.Fatalf("settlement = %+v", value)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("receipt was blocked by exhausted response capacity")
	}
	if active, _, _ := budget.counts(); active != 0 || responseCount(t, f.grant.MailboxHostDir) != 0 {
		t.Fatalf("accepted receipt did not retire its response: active=%d files=%d", active, responseCount(t, f.grant.MailboxHostDir))
	}
}

// A client whose call was committed before a daemon restart still reads that
// durable success afterwards: awaitResponse checks the response against the
// service it sent to, before it looks at the current boot. The ordering is
// pinned: the client has published and already polled once without a result,
// and is held before its next read while the server commits, restarts and
// adopts. The restarted server must keep (and account) the prior-boot response,
// must not dispatch the request again, and the client's release, signed for the
// old boot, is rejected so the response falls back to age cleanup.
func TestRestartPreservesCommittedResponseForWaitingClient(t *testing.T) {
	var dispatched atomic.Int32
	f := newFixture(t, Callbacks{Dispatch: func(context.Context, DispatchRequest) (DispatchResult, error) {
		dispatched.Add(1)
		return DispatchResult{Status: StatusOK, Body: []byte("committed")}, nil
	}}, ServerOptions{})
	var reads atomic.Int32
	parked := make(chan struct{})
	resume := make(chan struct{})
	client := f.client(clientOptions{beforeResponseRead: func() {
		// The first read runs before the server has served anything. Hold the
		// second until the restart has finished.
		if reads.Add(1) == 2 {
			close(parked)
			<-resume
		}
	}})
	type outcome struct {
		result Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := client.Query(context.Background(), Query{Verb: "snapshot"})
		done <- outcome{result, err}
	}()
	select {
	case <-parked:
	case <-time.After(5 * time.Second):
		t.Fatal("client never polled for its response")
	}
	if responseCount(t, f.grant.MailboxHostDir) != 0 {
		t.Fatal("response existed before the server served the request")
	}
	if _, err := f.server.ServeOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if responseCount(t, f.grant.MailboxHostDir) != 1 {
		t.Fatal("request was not committed before the restart")
	}
	if err := f.server.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.auth.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := access.Open(f.authDir)
	if err != nil {
		t.Fatal(err)
	}
	f.auth = restarted
	budget := &testResponseBudget{limit: 1}
	f.server, err = OpenServerMailbox(f.grant.SubjectID, f.grant.MailboxHostDir, restarted, restarted, Callbacks{
		Authorize: func(context.Context, access.Principal, Call) error { return nil },
		Dispatch: func(context.Context, DispatchRequest) (DispatchResult, error) {
			dispatched.Add(1)
			return DispatchResult{Status: StatusOK}, nil
		},
	}, ServerOptions{ResponseBudget: budget})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.server.PublishService(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := f.server.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(resume)
	var got outcome
	select {
	case got = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("client did not finish")
	}
	if got.err != nil || got.result.Status != StatusOK || string(got.result.Body) != "committed" {
		t.Fatalf("durable pre-restart success lost: %+v, %v", got.result, got.err)
	}
	if active, _, _ := budget.counts(); active != 1 {
		t.Fatalf("prior-boot response not adopted into the bounded pool: active=%d", active)
	}
	if n := reads.Load(); n != 2 {
		t.Fatalf("client read %d times; want exactly the parked read to observe the result", n)
	}
	if _, err := f.server.ServeOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := dispatched.Load(); n != 1 {
		t.Fatalf("request dispatched %d times across the restart", n)
	}
	if responseCount(t, f.grant.MailboxHostDir) != 1 {
		t.Fatal("a release signed for the previous boot retired a response")
	}
}

// Mandatory receipts are one-way. Repeated receipt-requiring operations from
// the production client must each complete and settle, and neither the receipt
// nor its accepted response may remain in the pool. The spare-capacity case
// matters: with free slots a conditional discard would still publish and retain
// an unread receipt response.
func TestRepeatedMandatoryReceiptsDoNotExhaustPool(t *testing.T) {
	for _, limit := range []int{1, 4} {
		t.Run(fmt.Sprintf("pool=%d", limit), func(t *testing.T) {
			budget := &testResponseBudget{limit: limit}
			settled := make(chan ReceiptSettlement, 16)
			f := newFixture(t, Callbacks{
				Dispatch: func(context.Context, DispatchRequest) (DispatchResult, error) {
					return DispatchResult{Status: StatusOK, Receipt: &ReceiptHooks{
						Grace: 5 * time.Second, Settled: func(value ReceiptSettlement) { settled <- value },
					}}, nil
				},
			}, ServerOptions{ResponseBudget: budget})
			client := f.client(clientOptions{})
			for i := 0; i < 8; i++ {
				result, err, elapsed := pumpUntil(t, []*Server{f.server}, func(ctx context.Context) (Result, error) {
					return client.Action(ctx, Action{Verb: "done", ID: f.grant.SubjectID})
				})
				if err != nil || !result.ReceiptSent {
					t.Fatalf("operation %d = %+v, %v", i, result, err)
				}
				if elapsed > time.Second {
					t.Fatalf("operation %d waited %v for response capacity", i, elapsed)
				}
				deadline := time.After(2 * time.Second)
				for waiting := true; waiting; {
					if _, err := f.server.ServeOnce(context.Background()); err != nil && !errors.Is(err, ErrResponseCapacity) {
						t.Fatal(err)
					}
					select {
					case value := <-settled:
						if !value.Received || value.Reason != SettlementReceipt {
							t.Fatalf("operation %d settled without its receipt: %+v", i, value)
						}
						waiting = false
					case <-deadline:
						t.Fatalf("operation %d receipt never settled", i)
					case <-time.After(5 * time.Millisecond):
					}
				}
				if active, _, _ := budget.counts(); active != 0 || responseCount(t, f.grant.MailboxHostDir) != 0 {
					t.Fatalf("operation %d left retained responses: active=%d files=%d", i, active, responseCount(t, f.grant.MailboxHostDir))
				}
			}
		})
	}
}

// Several subjects share one daemon-wide response pool far smaller than their
// combined traffic, as hook and event reads do in practice. With production
// clients every call completes promptly and the pool drains to zero.
func TestSharedResponsePoolSustainsProductionClientTraffic(t *testing.T) {
	root := t.TempDir()
	authority, err := access.Open(filepath.Join(root, "authority"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = authority.Close() })
	budget := &testResponseBudget{limit: 4}
	const subjects, calls = 6, 40
	var servers []*Server
	var clients []*Client
	for i := 0; i < subjects; i++ {
		grant, err := authority.EnsureSession(context.Background(), fmt.Sprintf("subject-%d", i), filepath.Join(root, fmt.Sprintf("ns-%d", i)))
		if err != nil {
			t.Fatal(err)
		}
		writeTestContext(t, grant.CredentialHostDir, grant.SubjectID, grant.MailboxHostDir)
		server, err := OpenServerMailbox(grant.SubjectID, grant.MailboxHostDir, authority, authority, Callbacks{
			Authorize: func(context.Context, access.Principal, Call) error { return nil },
			Dispatch: func(context.Context, DispatchRequest) (DispatchResult, error) {
				return DispatchResult{Status: StatusOK, Body: []byte("ok")}, nil
			},
		}, ServerOptions{ResponseBudget: budget})
		if err != nil {
			t.Fatal(err)
		}
		if err := server.PublishService(context.Background()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = server.Close() })
		client, err := openClientAt(grant.CredentialHostDir, clientOptions{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = client.Close() })
		servers = append(servers, server)
		clients = append(clients, client)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var serving sync.WaitGroup
	for _, server := range servers {
		serving.Add(1)
		go func() {
			defer serving.Done()
			for ctx.Err() == nil {
				_, _ = server.ServeOnce(ctx)
				time.Sleep(2 * time.Millisecond)
			}
		}()
	}
	var wg sync.WaitGroup
	errs := make(chan error, subjects*calls)
	for _, client := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < calls; i++ {
				callCtx, callCancel := context.WithTimeout(ctx, 2*time.Second)
				started := time.Now()
				_, err := client.Query(callCtx, Query{Verb: "snapshot"})
				callCancel()
				if err != nil {
					errs <- fmt.Errorf("call %d after %v: %w", i, time.Since(started), err)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if active, _, _ := budget.counts(); active == 0 {
			break
		}
		if time.Now().After(deadline) {
			active, _, _ := budget.counts()
			t.Fatalf("shared pool did not drain after released traffic: active=%d", active)
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	serving.Wait()
}
