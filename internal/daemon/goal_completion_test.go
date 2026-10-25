package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"amux/internal/agent"
	"amux/internal/codexapp"
	"amux/internal/core"
	"amux/internal/engine"
	"amux/internal/store"
	"amux/internal/wsops"
)

// goalAppServer is a minimal App Server over WebSocket for a native-goal
// coordinator: it answers the supervisor's handshake (a fresh thread with no
// goal yet) and lets the test push the notifications a live goal lifecycle
// produces. It follows newStructuredRPCServer, which is turn-only.
type goalAppServer struct {
	endpoint string
	threadID string
	ready    chan struct{} // closed once the supervisor has read the (empty) goal

	mu   sync.Mutex
	conn *websocket.Conn
}

func newGoalAppServer(t *testing.T) *goalAppServer {
	t.Helper()
	g := &goalAppServer{threadID: "goal-thread", ready: make(chan struct{})}
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade goal app server: %v", err)
			return
		}
		g.mu.Lock()
		g.conn = conn
		g.mu.Unlock()
		defer conn.Close()
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var call struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			if json.Unmarshal(raw, &call) != nil || len(call.ID) == 0 || string(call.ID) == "null" {
				continue
			}
			var result any = map[string]any{}
			switch call.Method {
			case "initialize":
				result = map[string]any{"capabilities": map[string]any{}}
			case "thread/start", "thread/resume":
				result = map[string]any{"thread": map[string]any{"id": g.threadID}}
			case "thread/goal/get":
				result = map[string]any{"goal": nil}
			}
			g.write(map[string]any{"id": call.ID, "result": result})
			if call.Method == "thread/goal/get" {
				once.Do(func() { close(g.ready) })
			}
		}
	}))
	t.Cleanup(server.Close)
	g.endpoint = "ws" + strings.TrimPrefix(server.URL, "http")
	return g
}

func (g *goalAppServer) write(v any) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.conn != nil {
		_ = g.conn.WriteJSON(v)
	}
}

func (g *goalAppServer) notify(method string, params any) {
	g.write(map[string]any{"method": method, "params": params})
}

// A native-goal coordinator whose goal went active → complete and finished its
// final turn is left exactly where it was: the daemon's poll archives nothing
// and stops no runtime, and the completed goal stays observable. Only the user's
// explicit archive takes the workgroup off the rail and stops its coordinator
// and members, and the user's explicit restore brings it back open.
func TestGoalCompletionLeavesWorkgroupOpenUntilExplicitArchive(t *testing.T) {
	isolateControl(t)
	t.Setenv("AMUX_JAIL", "off")
	d := New("", nil, time.Hour)
	d.engine = newFakeEngine()
	managerCtx, stopManager := context.WithCancel(t.Context())
	t.Cleanup(stopManager)
	d.codex = codexapp.NewManager(managerCtx, "")
	t.Cleanup(d.codex.Shutdown)

	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	root := store.Session{ID: "root", Agent: agent.GoalRuntime, Dir: t.TempDir()}
	if !agent.NativeGoals(root) {
		t.Fatal("fixture is not a native-goal coordinator")
	}
	member := store.Session{ID: "member", RootID: root.ID, Agent: "claude", Dir: t.TempDir()}
	other := store.Session{ID: "other", Agent: agent.GoalRuntime, Dir: t.TempDir()}
	for _, s := range []store.Session{root, member, other} {
		if err := db.PutSession(s); err != nil {
			t.Fatal(err)
		}
		for tab := 0; tab < 3; tab++ {
			if _, err := d.engine.Ensure(context.Background(), engine.Spec{Key: engine.Key{AgentID: s.ID, Tab: tab}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	// The rail snapshot is what killEngineFor reads to find a workgroup's
	// members; a sourceless poll empties it, so the test restores it as a
	// populated daemon would have it.
	railSnapshot := func() {
		d.mu.Lock()
		d.sessions = []core.Session{{ID: root.ID}, {ID: member.ID, RootID: root.ID}, {ID: other.ID}}
		d.mu.Unlock()
	}

	server := newGoalAppServer(t)
	sup, err := d.codex.Ensure(t.Context(), root.ID, root.Dir, nil, []string{"sleep", "60"},
		server.endpoint, "", "", "", codexapp.LaunchOptions{Goals: true})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-server.ready:
	case <-time.After(10 * time.Second):
		t.Fatal("supervisor never read the thread goal")
	}

	// The live lifecycle: the task's turn, the goal established, the model
	// completing it, the final turn ending — then a usage update observed after
	// the turn ended, so waiting for it proves the whole sequence was consumed.
	goal := map[string]any{"threadId": server.threadID, "objective": "ship the fix", "status": codexapp.GoalActive,
		"tokenBudget": nil, "tokensUsed": 10, "timeUsedSeconds": 1, "createdAt": 1, "updatedAt": 2}
	server.notify("turn/started", map[string]any{"threadId": server.threadID, "turn": map[string]any{"id": "final"}})
	server.notify("thread/goal/updated", map[string]any{"threadId": server.threadID, "goal": goal})
	goal["status"], goal["updatedAt"] = codexapp.GoalComplete, 3
	server.notify("thread/goal/updated", map[string]any{"threadId": server.threadID, "goal": goal})
	server.notify("turn/completed", map[string]any{"threadId": server.threadID, "turn": map[string]any{"id": "final", "status": "completed"}})
	goal["tokensUsed"], goal["updatedAt"] = 4242, 4
	server.notify("thread/goal/updated", map[string]any{"threadId": server.threadID, "goal": goal})
	deadline := time.Now().Add(10 * time.Second)
	for {
		if g, ok := sup.Goal(); ok && g.Status == codexapp.GoalComplete && g.TokensUsed == 4242 {
			break
		}
		if time.Now().After(deadline) {
			g, ok := sup.Goal()
			t.Fatalf("completed goal never observed: %+v ok=%t", g, ok)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// However many polls pass after the goal completed, the daemon takes no
	// lifecycle decision of its own.
	for i := 0; i < 3; i++ {
		d.pollOnce(t.Context())
	}
	for _, id := range []string{root.ID, member.ID, other.ID} {
		if got, _, _ := db.GetSession(id); got.Archived || got.ArchivedAt != 0 {
			t.Fatalf("poll archived %s: %+v", id, got)
		}
	}
	if live := d.engine.Live(); len(live) != 9 {
		t.Fatalf("poll stopped a runtime: %+v", live)
	}
	if _, ok := d.codex.Get(root.ID); !ok {
		t.Fatal("poll closed the coordinator's supervisor")
	}
	if g, ok := sup.Goal(); !ok || g.Status != codexapp.GoalComplete || g.Objective != "ship the fix" {
		t.Fatalf("completed goal no longer observable: %+v ok=%t", g, ok)
	}

	// The user's explicit archive still works and still stops the whole workgroup.
	railSnapshot()
	if r := d.handle(t.Context(), core.Action{Action: core.ActionSetArchived, ID: root.ID, Fields: map[string]string{"archived": "true"}}); !r.OK {
		t.Fatalf("explicit archive = %+v", r)
	}
	got, _, err := db.GetSession(root.ID)
	if err != nil || !got.Archived || got.ArchivedAt == 0 {
		t.Fatalf("explicit archive not persisted: %+v %v", got, err)
	}
	if live := d.engine.Live(); len(live) != 3 {
		t.Fatalf("live runtimes after archive: %+v", live)
	}
	for _, key := range d.engine.Live() {
		if key.AgentID != other.ID {
			t.Fatalf("archived workgroup still running: %+v", key)
		}
	}
	if _, ok := d.codex.Get(root.ID); ok {
		t.Fatal("explicit archive left the coordinator's supervisor running")
	}
	if _, found, err := db.GetSession(member.ID); err != nil || !found {
		t.Fatalf("member removed by archive: %v", err)
	}

	// And the user's explicit restore reopens it for the next task. (The host
	// restore action's credential regrant needs a prepared coordinator sandbox
	// and is covered by actions_test; the store transition is what matters here.)
	if err := wsops.SetArchived(t.Context(), root.ID, false); err != nil {
		t.Fatalf("explicit restore: %v", err)
	}
	if got, _, _ := db.GetSession(root.ID); got.Archived || got.ArchivedAt != 0 {
		t.Fatalf("restore left the workgroup archived: %+v", got)
	}
	d.pollOnce(t.Context())
	if got, _, _ := db.GetSession(root.ID); got.Archived {
		t.Fatal("poll re-archived a restored workgroup")
	}
}
