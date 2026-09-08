package codexapp

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// controlledMsgConn makes writer admission and retirement observable without
// replacing any rpcConn behavior. A blocked write honors both its context and
// Close, exactly as the private msgConn contract requires.
type controlledMsgConn struct {
	mu sync.Mutex

	writes    [][]byte
	failWrite int
	started   chan int
	block     chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	closes    int
}

func newControlledMsgConn() *controlledMsgConn {
	return &controlledMsgConn{
		started: make(chan int, 16),
		done:    make(chan struct{}),
	}
}

func (c *controlledMsgConn) ReadMessage() ([]byte, error) {
	<-c.done
	return nil, io.EOF
}

func (c *controlledMsgConn) WriteMessage(ctx context.Context, b []byte) error {
	c.mu.Lock()
	c.writes = append(c.writes, append([]byte(nil), b...))
	n := len(c.writes)
	fail := c.failWrite == n
	block := c.block
	c.mu.Unlock()

	c.started <- n
	if fail {
		return io.ErrUnexpectedEOF
	}
	if n == 1 && block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		case <-c.done:
			return errClosed
		}
	}
	return nil
}

func (c *controlledMsgConn) Close() error {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closes++
		c.mu.Unlock()
		close(c.done)
	})
	return nil
}

func (c *controlledMsgConn) counts() (writes, closes int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.writes), c.closes
}

func waitPending(t *testing.T, rpc *rpcConn, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		rpc.mu.Lock()
		got := len(rpc.pending)
		rpc.mu.Unlock()
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pending calls = %d, want %d", got, want)
		}
		runtime.Gosched()
	}
}

func TestRPCWriterAdmissionCancellationDoesNotRetireTransport(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  func() (context.Context, context.CancelFunc)
		fire func(context.CancelFunc)
		want error
	}{
		{
			name: "deadline",
			ctx: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), 30*time.Millisecond)
			},
			fire: func(context.CancelFunc) {},
			want: context.DeadlineExceeded,
		},
		{
			name: "cancel",
			ctx: func() (context.Context, context.CancelFunc) {
				return context.WithCancel(context.Background())
			},
			fire: func(cancel context.CancelFunc) { cancel() },
			want: context.Canceled,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transport := newControlledMsgConn()
			transport.block = make(chan struct{})
			rpc := newRPCConn(transport)
			defer rpc.close()

			firstDone := make(chan error, 1)
			go func() { firstDone <- rpc.notify("first", map[string]any{"v": 1}) }()
			if n := <-transport.started; n != 1 {
				t.Fatalf("first write number = %d", n)
			}

			ctx, cancel := tc.ctx()
			defer cancel()
			secondDone := make(chan error, 1)
			go func() {
				_, err := rpc.call(ctx, "queued", map[string]any{"v": 2})
				secondDone <- err
			}()
			waitPending(t, rpc, 1)
			tc.fire(cancel)

			select {
			case err := <-secondDone:
				if !errors.Is(err, tc.want) {
					t.Fatalf("queued call error = %v, want %v", err, tc.want)
				}
			case <-time.After(time.Second):
				t.Fatal("queued call did not honor cancellation")
			}
			if writes, closes := transport.counts(); writes != 1 || closes != 0 {
				t.Fatalf("before release writes/closes = %d/%d, want 1/0", writes, closes)
			}
			waitPending(t, rpc, 0)

			close(transport.block)
			if err := <-firstDone; err != nil {
				t.Fatalf("first write: %v", err)
			}
			if err := rpc.notify("later", nil); err != nil {
				t.Fatalf("healthy transport rejected later write: %v", err)
			}
			if writes, closes := transport.counts(); writes != 2 || closes != 0 {
				t.Fatalf("final writes/closes = %d/%d, want 2/0", writes, closes)
			}
		})
	}
}

func TestRPCAlreadyCancelledCallDoesNotRegisterOrWrite(t *testing.T) {
	transport := newControlledMsgConn()
	rpc := newRPCConn(transport)
	defer rpc.close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := rpc.call(ctx, "never", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("call error = %v, want context.Canceled", err)
	}
	rpc.mu.Lock()
	nextID, pending := rpc.nextID, len(rpc.pending)
	rpc.mu.Unlock()
	if nextID != 0 || pending != 0 {
		t.Fatalf("nextID/pending = %d/%d, want 0/0", nextID, pending)
	}
	if writes, closes := transport.counts(); writes != 0 || closes != 0 {
		t.Fatalf("writes/closes = %d/%d, want 0/0", writes, closes)
	}
}

func TestRPCAdmittedWriteFailureRetiresAndReleasesPending(t *testing.T) {
	transport := newControlledMsgConn()
	transport.failWrite = 2
	rpc := newRPCConn(transport)

	firstDone := make(chan error, 1)
	go func() {
		_, err := rpc.call(context.Background(), "waiting", nil)
		firstDone <- err
	}()
	if n := <-transport.started; n != 1 {
		t.Fatalf("first write number = %d", n)
	}
	waitPending(t, rpc, 1)

	if err := rpc.notify("fails", nil); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("failed admitted write = %v, want io.ErrUnexpectedEOF", err)
	}
	select {
	case err := <-firstDone:
		if err == nil || !strings.Contains(err.Error(), "connection closed") {
			t.Fatalf("released pending call error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("pending call was not released by transport retirement")
	}
	if err := rpc.notify("after-retirement", nil); !errors.Is(err, errClosed) {
		t.Fatalf("write after retirement = %v, want errClosed", err)
	}
	if err := rpc.close(); err != nil {
		t.Fatalf("idempotent close: %v", err)
	}
	if writes, closes := transport.counts(); writes != 2 || closes != 1 {
		t.Fatalf("writes/closes = %d/%d, want 2/1", writes, closes)
	}
	waitPending(t, rpc, 0)
}

func TestRPCCloseUnblocksAdmittedAndQueuedCalls(t *testing.T) {
	for i := 0; i < 50; i++ {
		transport := newControlledMsgConn()
		transport.block = make(chan struct{})
		rpc := newRPCConn(transport)

		firstDone := make(chan error, 1)
		secondDone := make(chan error, 1)
		go func() {
			_, err := rpc.call(context.Background(), "first", nil)
			firstDone <- err
		}()
		if n := <-transport.started; n != 1 {
			t.Fatalf("iteration %d: first write number = %d", i, n)
		}
		go func() {
			_, err := rpc.call(context.Background(), "second", nil)
			secondDone <- err
		}()
		waitPending(t, rpc, 2)

		closeDone := make(chan error, 1)
		go func() { closeDone <- rpc.close() }()
		for name, ch := range map[string]<-chan error{
			"first": firstDone, "second": secondDone, "close": closeDone,
		} {
			select {
			case err := <-ch:
				if name != "close" && err == nil {
					t.Fatalf("iteration %d: %s unexpectedly succeeded", i, name)
				}
				if name == "close" && err != nil {
					t.Fatalf("iteration %d: close: %v", i, err)
				}
			case <-time.After(time.Second):
				t.Fatalf("iteration %d: %s did not finish", i, name)
			}
		}
		waitPending(t, rpc, 0)
		if writes, closes := transport.counts(); writes != 1 || closes != 1 {
			t.Fatalf("iteration %d: writes/closes = %d/%d, want 1/1", i, writes, closes)
		}
	}
}

func TestRPCNormalNotificationAndResponses(t *testing.T) {
	transport := newControlledMsgConn()
	rpc := newRPCConn(transport)
	defer rpc.close()

	if err := rpc.notify("event", map[string]any{"ok": true}); err != nil {
		t.Fatal(err)
	}
	if err := rpc.respond(json.RawMessage(`"r1"`), map[string]any{"accepted": true}); err != nil {
		t.Fatal(err)
	}
	if err := rpc.respondErr(json.RawMessage(`7`), -32000, "denied"); err != nil {
		t.Fatal(err)
	}

	transport.mu.Lock()
	frames := append([][]byte(nil), transport.writes...)
	transport.mu.Unlock()
	if len(frames) != 3 {
		t.Fatalf("frames = %d, want 3", len(frames))
	}
	wants := []string{
		`{"method":"event","params":{"ok":true}}`,
		`{"id":"r1","result":{"accepted":true}}`,
		`{"error":{"code":-32000,"message":"denied"},"id":7}`,
	}
	for i, want := range wants {
		if string(frames[i]) != want {
			t.Errorf("frame %d = %s, want %s", i, frames[i], want)
		}
	}
}

// observedNetConn witnesses entry into the actual Gorilla socket write after the
// handshake. It does not alter, delay, or complete that write.
type observedNetConn struct {
	net.Conn
	mu      sync.Mutex
	observe chan struct{}
}

func (c *observedNetConn) arm() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.observe = make(chan struct{})
	return c.observe
}

func (c *observedNetConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	observe := c.observe
	c.observe = nil
	c.mu.Unlock()
	if observe != nil {
		close(observe)
	}
	return c.Conn.Write(b)
}

func openStoppedWebSocket(t *testing.T) (*wsConn, *observedNetConn) {
	t.Helper()
	peer, rawClient := net.Pipe()
	client := &observedNetConn{Conn: rawClient}
	t.Cleanup(func() {
		_ = peer.Close()
		_ = rawClient.Close()
	})

	handshake := make(chan error, 1)
	go func() {
		req, err := http.ReadRequest(bufio.NewReader(peer))
		if err != nil {
			handshake <- err
			return
		}
		sum := sha1.Sum([]byte(req.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		_, err = fmt.Fprintf(peer, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", base64.StdEncoding.EncodeToString(sum[:]))
		handshake <- err
		// Deliberately return without reading another byte. peer remains open until
		// the test defer, so only the client's context interrupt can release write.
	}()

	u, err := url.Parse("ws://fixture.invalid/")
	if err != nil {
		t.Fatal(err)
	}
	conn, _, err := websocket.NewClient(client, u, nil, 1024, 1024)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if err := <-handshake; err != nil {
		t.Fatalf("peer handshake: %v", err)
	}
	return &wsConn{c: conn}, client
}

func TestWSConnDeadlineInterruptsStoppedPeerAndRetiresRPC(t *testing.T) {
	transport, client := openStoppedWebSocket(t)
	writeEntered := client.arm()

	rpc := newRPCConn(transport)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	callDone := make(chan error, 1)
	go func() {
		_, err := rpc.call(ctx, "turn/start", map[string]any{"input": strings.Repeat("x", 64<<10)})
		callDone <- err
	}()
	select {
	case <-writeEntered:
	case <-time.After(time.Second):
		t.Fatal("Gorilla did not enter the stopped peer write")
	}
	var err error
	select {
	case err = <-callDone:
	case <-time.After(time.Second):
		t.Fatal("context did not interrupt the stopped peer write")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked WebSocket call error = %v, want context deadline", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("blocked WebSocket write returned after %v, want bounded cancellation", elapsed)
	}
	if err := rpc.notify("after-retirement", nil); !errors.Is(err, errClosed) {
		t.Fatalf("write after interrupted frame = %v, want errClosed", err)
	}
	waitPending(t, rpc, 0)
	if err := rpc.close(); err != nil {
		t.Fatalf("idempotent rpc close: %v", err)
	}
}

func TestWSConnCancellationAndCloseRaceJoinsWriter(t *testing.T) {
	transport, client := openStoppedWebSocket(t)
	writeEntered := client.arm()
	ctx, cancel := context.WithCancel(context.Background())
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- transport.WriteMessage(ctx, []byte(strings.Repeat("x", 64<<10)))
	}()
	select {
	case <-writeEntered:
	case <-time.After(time.Second):
		t.Fatal("Gorilla did not enter the stopped peer write")
	}

	start := make(chan struct{})
	closeDone := make(chan error, 1)
	go func() {
		<-start
		closeDone <- transport.Close()
	}()
	cancelDone := make(chan struct{})
	go func() {
		<-start
		cancel()
		close(cancelDone)
	}()
	close(start)

	select {
	case err := <-writeDone:
		if err == nil {
			t.Fatal("interrupted WebSocket write unexpectedly succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("cancel/Close race abandoned the WebSocket writer")
	}
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("concurrent Close did not finish")
	}
	select {
	case <-cancelDone:
	case <-time.After(time.Second):
		t.Fatal("concurrent cancellation did not finish")
	}
	if err := transport.Close(); err != nil {
		t.Fatalf("idempotent Close: %v", err)
	}
}
