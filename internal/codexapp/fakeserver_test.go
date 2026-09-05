package codexapp

import (
	"bufio"
	"encoding/json"
	"net"
	"sync"
	"testing"
	"time"
)

// fakeServer is an in-memory App Server over one end of a net.Pipe. It answers the
// handshake and turn calls with canned results, records every client message for
// assertions, and lets a test push notifications and server-initiated requests —
// so the supervisor's protocol is exercised end to end without a real codex binary.
type fakeServer struct {
	t    *testing.T
	conn net.Conn

	mu       sync.Mutex
	calls    []incoming               // every client request/notification seen, in order
	respByID map[string]chan incoming // client responses to server requests, keyed by our id
	turnID   string
	closed   bool
}

// newFakePair wires a supervisor to a fakeServer over net.Pipe and starts the
// server read loop. The caller drives the supervisor via attach(client).
func newFakePair(t *testing.T) (*Supervisor, *fakeServer, net.Conn) {
	t.Helper()
	client, server := net.Pipe()
	fs := &fakeServer{t: t, conn: server, respByID: map[string]chan incoming{}}
	go fs.loop()
	sup := New(Config{SessionID: "s-test"})
	return sup, fs, client
}

// newRawPair returns a connected net.Pipe pair for a test that wires its own
// supervisor Config (e.g. a resume) rather than the default newFakePair.
func newRawPair(t *testing.T) (client, server net.Conn) {
	t.Helper()
	return net.Pipe()
}

func (fs *fakeServer) loop() {
	sc := bufio.NewScanner(fs.conn)
	sc.Buffer(make([]byte, 1<<16), 1<<20)
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		var m incoming
		if err := json.Unmarshal(line, &m); err != nil {
			continue
		}
		fs.mu.Lock()
		fs.calls = append(fs.calls, m)
		hasID := len(m.ID) > 0 && string(m.ID) != "null"
		// A response to one of OUR server requests (has id, no method).
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

// handleCall answers a client request with a canned result.
func (fs *fakeServer) handleCall(m incoming) {
	var result any = map[string]any{}
	switch m.Method {
	case "initialize":
		result = map[string]any{"capabilities": map[string]any{}}
	case "thread/start":
		result = map[string]any{"thread": map[string]any{"id": "thr_1"}}
	case "thread/resume":
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
	b = append(b, '\n')
	fs.mu.Lock()
	closed := fs.closed
	fs.mu.Unlock()
	if closed {
		return
	}
	_, _ = fs.conn.Write(b)
}

// pushNotify sends a server→client notification (no id).
func (fs *fakeServer) pushNotify(method string, params any) {
	fs.write(map[string]any{"method": method, "params": params})
}

// pushRequest sends a server→client request (with an id) and returns the client's
// response, waiting up to a short deadline. id is a string the test chooses.
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

// completeTurn pushes turn/completed for the current turn.
func (fs *fakeServer) completeTurn(status string) {
	fs.mu.Lock()
	id := fs.turnID
	fs.mu.Unlock()
	fs.pushNotify("turn/completed", map[string]any{
		"turn": map[string]any{"id": id, "status": status},
	})
}

func (fs *fakeServer) close() {
	fs.mu.Lock()
	fs.closed = true
	fs.mu.Unlock()
	_ = fs.conn.Close()
}

// sawCall reports whether the client ever sent a request/notification with method.
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
