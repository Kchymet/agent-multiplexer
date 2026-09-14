package daemon

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"amux/internal/access"
	"amux/internal/codexapp"
	"amux/internal/core"
	"amux/internal/sessionrpc"
	"amux/internal/store"

	"github.com/gorilla/websocket"
)

// TestSignedMailboxLiveStructuredPromptAdmission exercises the complete
// authenticated file-RPC boundary. The durable response is read before the
// deferred admission gate opens, making callback return precede BeginPrompt
// deterministically. Each denial case changes live state only after that ACK.
func TestSignedMailboxLiveStructuredPromptAdmission(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*testing.T, *signedStructuredFixture)
		want   bool
	}{
		{name: "current", want: true},
		{name: "membership moved", change: moveSignedStructuredTarget},
		{name: "credential revoked", change: revokeSignedStructuredCaller},
		{name: "runtime replaced", change: replaceSignedStructuredRuntime},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSignedStructuredFixture(t)
			call := sessionrpc.Call{
				Kind: sessionrpc.CallOperation, Route: access.RouteAction,
				Verb: core.ActionSteer, ID: f.member.ID,
				Fields: map[string]string{
					core.SteerVerb: core.SteerPrompt,
					core.SteerText: "deliver after acknowledgement",
				},
			}
			requestID := publishSignedMailboxCall(t, f.d.authority, f.grant, call)
			if processed, err := f.server.ServeOnce(f.servingCtx); err != nil || processed != 1 {
				t.Fatalf("ServeOnce = %d, %v", processed, err)
			}
			response := readMailboxResponse(t, f.grant, requestID)
			if response.Status != sessionrpc.StatusOK {
				t.Fatalf("accepted response = %+v", response)
			}
			select {
			case bounded := <-f.admissionEntered:
				if !bounded {
					t.Fatal("deferred authorization was not bounded by steering admission timeout")
				}
			case <-time.After(time.Second):
				t.Fatal("accepted work did not reach deferred admission")
			}

			if tc.change != nil {
				tc.change(t, f)
			}
			close(f.releaseAdmission)
			waitDeferredWork(t, f.d)

			select {
			case <-f.turnStarted:
				if !tc.want {
					t.Fatal("BeginPrompt reached the App Server after final admission was denied")
				}
			case <-time.After(100 * time.Millisecond):
				if tc.want {
					t.Fatal("acknowledged live structured prompt never reached BeginPrompt")
				}
			}
		})
	}
}

type signedStructuredFixture struct {
	d                *Daemon
	runtime          *sessionRuntime
	root             store.Session
	member           store.Session
	grant            access.SessionAccess
	server           *sessionrpc.Server
	servingCtx       context.Context
	managerCtx       context.Context
	endpoint         string
	original         *codexapp.Supervisor
	turnStarted      <-chan struct{}
	admissionEntered chan bool
	releaseAdmission chan struct{}
}

func newSignedStructuredFixture(t *testing.T) *signedStructuredFixture {
	t.Helper()
	root := store.Session{ID: "signed-root", Scope: store.ScopeWork, Agent: "codex", Dir: t.TempDir()}
	member := store.Session{ID: "signed-member", RootID: root.ID, Agent: "codex", Dir: t.TempDir()}
	d, runtime, _ := sessionRuntimeFixture(t, root, member)
	d.sessionRPC = runtime
	t.Cleanup(runtime.close)
	d.engine = newFakeEngine() // steer validates engine availability before structured routing

	managerCtx, stopManager := context.WithCancel(t.Context())
	t.Cleanup(stopManager)
	d.codex = codexapp.NewManager(managerCtx, "")
	t.Cleanup(d.codex.Shutdown)
	endpoint, turnStarted := newStructuredRPCServer(t)
	sup, err := d.codex.Ensure(t.Context(), member.ID, member.Dir, nil,
		[]string{"sleep", "60"}, endpoint, "", "", "")
	if err != nil {
		t.Fatal(err)
	}

	grant, err := d.authority.EnsureSession(t.Context(), root.ID, root.Dir)
	if err != nil {
		t.Fatal(err)
	}
	server, err := sessionrpc.OpenServerMailbox(root.ID, grant.MailboxHostDir,
		d.authority, d.authority, sessionrpc.Callbacks{
			Authorize: runtime.authorizeCallback,
			Dispatch:  runtime.dispatchCallback,
		}, sessionrpc.ServerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	if err := server.PublishService(t.Context()); err != nil {
		t.Fatal(err)
	}

	entered := make(chan bool, 1)
	release := make(chan struct{})
	d.steerAdmission = func(ctx context.Context) error {
		_, bounded := ctx.Deadline()
		entered <- bounded
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return &signedStructuredFixture{
		d: d, runtime: runtime, root: root, member: member, grant: grant,
		server: server, servingCtx: t.Context(), managerCtx: managerCtx,
		endpoint: endpoint, original: sup, turnStarted: turnStarted,
		admissionEntered: entered, releaseAdmission: release,
	}
}

func newStructuredRPCServer(t *testing.T) (string, <-chan struct{}) {
	t.Helper()
	turnStarted := make(chan struct{}, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade structured test server: %v", err)
			return
		}
		defer conn.Close()
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var call struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if err := json.Unmarshal(raw, &call); err != nil {
				t.Errorf("decode structured call: %v", err)
				return
			}
			if len(call.ID) == 0 || string(call.ID) == "null" {
				continue
			}
			threadID := "signed-thread"
			var result any = map[string]any{}
			switch call.Method {
			case "initialize":
				result = map[string]any{"capabilities": map[string]any{}}
			case "thread/start":
				result = map[string]any{"thread": map[string]any{"id": threadID}}
			case "thread/resume":
				var params struct {
					ThreadID string `json:"threadId"`
				}
				_ = json.Unmarshal(call.Params, &params)
				if params.ThreadID != "" {
					threadID = params.ThreadID
				}
				result = map[string]any{"thread": map[string]any{"id": threadID}}
			case "turn/start":
				turnStarted <- struct{}{}
				result = map[string]any{"turn": map[string]any{"id": "signed-turn"}}
			}
			if err := conn.WriteJSON(map[string]any{"id": call.ID, "result": result}); err != nil {
				return
			}
			if call.Method == "turn/start" {
				_ = conn.WriteJSON(map[string]any{"method": "turn/started", "params": map[string]any{
					"threadId": threadID, "turn": map[string]any{"id": "signed-turn"},
				}})
				_ = conn.WriteJSON(map[string]any{"method": "turn/completed", "params": map[string]any{
					"threadId": threadID, "turn": map[string]any{"id": "signed-turn", "status": "completed"},
				}})
			}
		}
	}))
	t.Cleanup(server.Close)
	return "ws" + strings.TrimPrefix(server.URL, "http"), turnStarted
}

func publishSignedMailboxCall(t *testing.T, authority *access.FileAuthority, grant access.SessionAccess, call sessionrpc.Call) string {
	t.Helper()
	credential, err := access.LoadCredential(grant.CredentialHostDir)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(call)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := access.SignRequest(credential, authority.BootID(), body, time.Now(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(grant.RequestsHostDir, envelope.RequestID+".req"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	return envelope.RequestID
}

func readMailboxResponse(t *testing.T, grant access.SessionAccess, requestID string) sessionrpc.Response {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(grant.MailboxHostDir, sessionrpc.ResponsesDirName, requestID+".res"))
	if err != nil {
		t.Fatal(err)
	}
	var response sessionrpc.Response
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func moveSignedStructuredTarget(t *testing.T, f *signedStructuredFixture) {
	t.Helper()
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.SetRootID(f.member.ID, "different-root"); err != nil {
		t.Fatal(err)
	}
}

func revokeSignedStructuredCaller(t *testing.T, f *signedStructuredFixture) {
	t.Helper()
	if err := f.d.authority.Revoke(t.Context(), access.SubjectSession, f.root.ID); err != nil {
		t.Fatal(err)
	}
}

func replaceSignedStructuredRuntime(t *testing.T, f *signedStructuredFixture) {
	t.Helper()
	f.d.codex.Close(f.member.ID)
	replacement, err := f.d.codex.Ensure(t.Context(), f.member.ID, f.member.Dir, nil,
		[]string{"sleep", "60"}, f.endpoint, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if replacement == f.original {
		t.Fatal("manager did not replace the structured runtime")
	}
}

func waitDeferredWork(t *testing.T, d *Daemon) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		d.deferredWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("deferred work did not finish")
	}
}
