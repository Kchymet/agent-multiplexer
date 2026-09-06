package codexapp

import (
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"
)

// memConn is an in-memory msgConn: message-framed (one []byte per message), so the
// supervisor's JSON-RPC-over-WebSocket layer is exercised without a real socket or
// handshake. Closing either end unblocks both.
type memConn struct {
	in   <-chan []byte
	out  chan<- []byte
	done chan struct{}
	once *sync.Once
}

func newMemPair() (client, server *memConn) {
	a2b := make(chan []byte, 64)
	b2a := make(chan []byte, 64)
	done := make(chan struct{})
	once := &sync.Once{}
	client = &memConn{in: b2a, out: a2b, done: done, once: once}
	server = &memConn{in: a2b, out: b2a, done: done, once: once}
	return client, server
}

func (c *memConn) ReadMessage() ([]byte, error) {
	select {
	case b, ok := <-c.in:
		if !ok {
			return nil, io.EOF
		}
		return b, nil
	case <-c.done:
		return nil, io.EOF
	}
}

func (c *memConn) WriteMessage(b []byte) error {
	msg := append([]byte(nil), b...)
	select {
	case c.out <- msg:
		return nil
	case <-c.done:
		return errClosed
	}
}

func (c *memConn) Close() error {
	c.once.Do(func() { close(c.done) })
	return nil
}

// fakeServer answers the App Server handshake/turn calls with canned results over
// a memConn, records every client message, and lets a test push notifications and
// server-initiated requests. It also asserts the client sends the corrected wire
// shapes (hyphenated enums).
type fakeServer struct {
	t    *testing.T
	conn msgConn

	mu         sync.Mutex
	calls      []incoming
	respByID   map[string]chan incoming
	turnID     string
	failMethod string // optional RPC failure for initialization tests
	resumeErr  string // when set, thread/resume replies with this JSON-RPC error message
}

func newFakePair(t *testing.T) (*Supervisor, *fakeServer, *memConn) {
	t.Helper()
	client, server := newMemPair()
	fs := &fakeServer{t: t, conn: server, respByID: map[string]chan incoming{}}
	go fs.loop()
	sup := New(Config{SessionID: "s-test", Endpoint: "unix:///tmp/x.sock"})
	return sup, fs, client
}

func (fs *fakeServer) loop() {
	for {
		msg, err := fs.conn.ReadMessage()
		if err != nil {
			return
		}
		var m incoming
		if err := json.Unmarshal(msg, &m); err != nil {
			continue
		}
		fs.mu.Lock()
		fs.calls = append(fs.calls, m)
		hasID := len(m.ID) > 0 && string(m.ID) != "null"
		if m.Method == "" && hasID {
			if ch := fs.respByID[string(m.ID)]; ch != nil {
				fs.mu.Unlock()
				ch <- m
				continue
			}
		}
		fs.mu.Unlock()
		if m.Method != "" && hasID {
			fs.handleCall(m)
		}
	}
}

func (fs *fakeServer) handleCall(m incoming) {
	if m.Method == fs.failMethod {
		fs.write(map[string]any{"id": m.ID, "error": map[string]any{"code": -32000, "message": "fixture failure"}})
		return
	}
	var result any = map[string]any{}
	switch m.Method {
	case "initialize":
		result = map[string]any{"capabilities": map[string]any{}}
	case "thread/start":
		var p struct {
			ApprovalPolicy string `json:"approvalPolicy"`
			Sandbox        string `json:"sandbox"`
		}
		_ = json.Unmarshal(m.Params, &p)
		if p.ApprovalPolicy != "on-request" || p.Sandbox != "workspace-write" {
			fs.t.Errorf("thread/start sent invalid enums: policy=%q sandbox=%q (want hyphenated)", p.ApprovalPolicy, p.Sandbox)
		}
		result = map[string]any{"thread": map[string]any{"id": "thr_1"}}
	case "thread/resume":
		fs.mu.Lock()
		rerr := fs.resumeErr
		fs.resumeErr = "" // only the initial missing-thread resume fails
		fs.mu.Unlock()
		if rerr != "" {
			fs.write(map[string]any{"id": m.ID, "error": map[string]any{"code": -32000, "message": rerr}})
			return
		}
		var p struct {
			ThreadID string `json:"threadId"`
		}
		_ = json.Unmarshal(m.Params, &p)
		result = map[string]any{"thread": map[string]any{"id": p.ThreadID}}
	case "turn/start":
		fs.mu.Lock()
		fs.turnID = "turn_1"
		fs.mu.Unlock()
		result = map[string]any{"turn": map[string]any{"id": "turn_1"}}
	case "turn/steer", "turn/interrupt":
		result = map[string]any{}
	}
	fs.write(map[string]any{"id": m.ID, "result": result})
}

func (fs *fakeServer) write(obj any) {
	b, _ := json.Marshal(obj)
	_ = fs.conn.WriteMessage(b)
}

func (fs *fakeServer) pushNotify(method string, params any) {
	fs.write(map[string]any{"method": method, "params": params})
}

// pushTurn pushes a turn/started then turn/completed for the current turn, the
// observed lifecycle that brackets a turn on the event stream.
func (fs *fakeServer) pushTurnStarted() {
	fs.mu.Lock()
	id := fs.turnID
	fs.mu.Unlock()
	fs.pushNotify("turn/started", map[string]any{"threadId": "thr_1", "turn": map[string]any{"id": id}})
}

func (fs *fakeServer) completeTurn(status string) {
	fs.mu.Lock()
	id := fs.turnID
	fs.mu.Unlock()
	// Real Codex 0.153.4 carries threadId on turn/completed (see mapping_test), and the
	// supervisor requires it to correlate the completion to the turn it is tracking, so
	// this helper mirrors pushTurnStarted rather than sending a thread-less completion.
	fs.pushNotify("turn/completed", map[string]any{"threadId": "thr_1", "turn": map[string]any{"id": id, "status": status}})
}

func (fs *fakeServer) pushRequest(method, id string, params any) incoming {
	fs.t.Helper()
	rawID, _ := json.Marshal(id)
	ch := make(chan incoming, 1)
	fs.mu.Lock()
	fs.respByID[string(rawID)] = ch
	fs.mu.Unlock()
	fs.write(map[string]any{"method": method, "id": id, "params": params})
	select {
	case resp := <-ch:
		return resp
	case <-time.After(2 * time.Second):
		fs.t.Fatalf("no client response to server request %s(%s)", method, id)
		return incoming{}
	}
}

func (fs *fakeServer) close() { _ = fs.conn.Close() }

func (fs *fakeServer) sawCall(method string) (incoming, bool) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	for _, c := range fs.calls {
		if c.Method == method {
			return c, true
		}
	}
	return incoming{}, false
}
