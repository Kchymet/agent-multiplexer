package codexauth

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

// configOverrides pin the central home's native behavior regardless of any
// config.toml: credentials only in the private auth.json the parser reads (never
// a keyring the parser cannot see), and ChatGPT login only (never an API key).
var configOverrides = []string{
	"-c", `cli_auth_credentials_store="file"`,
	"-c", `forced_login_method="chatgpt"`,
}

// scrubbedEnv are removed from the native server's environment: another home,
// API-key credentials that could shadow the ChatGPT login, and log filters that
// could make the server print request headers.
var scrubbedEnv = map[string]bool{
	"CODEX_HOME": true, "OPENAI_API_KEY": true, "CODEX_API_KEY": true,
	"RUST_LOG": true, "CODEX_LOG": true,
}

const (
	maxFrame    = 4 << 20
	stopTimeout = 3 * time.Second
)

// server is one managed `codex app-server` speaking newline-delimited JSON-RPC
// over stdio (the native default transport; frames omit "jsonrpc").
type server struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	wmu   sync.Mutex
	done  chan struct{}
	quit  <-chan struct{}

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan response
	subs    map[string]chan json.RawMessage
	dead    bool
	err     error

	stderr *redactingWriter
}

type response struct {
	result json.RawMessage
	err    *rpcError
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("rpc error %d: %s", e.Code, redact(e.Message))
}

func serverEnv(base []string, home string) []string {
	env := make([]string, 0, len(base)+1)
	for _, kv := range base {
		key, _, _ := strings.Cut(kv, "=")
		if !scrubbedEnv[key] {
			env = append(env, kv)
		}
	}
	return append(env, "CODEX_HOME="+home)
}

func startServer(ctx context.Context, cfg Config, quit <-chan struct{}) (*server, error) {
	argv := append(append(append([]string(nil), cfg.Argv...), configOverrides...), "app-server")
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = cfg.Home
	cmd.Env = serverEnv(cfg.Env, cfg.Home)
	cmd.SysProcAttr = sysProcAttr()
	// A descendant holding stdout/stderr open must not keep Wait (and so the
	// liveness of this server) pending after the native process exits.
	cmd.WaitDelay = 2 * time.Second
	s := &server{
		cmd:     cmd,
		done:    make(chan struct{}),
		quit:    quit,
		pending: map[int64]chan response{},
		subs:    map[string]chan json.RawMessage{},
		stderr:  &redactingWriter{out: cfg.Stderr},
	}
	// Native stderr never enters an error, event, or log here; it reaches only
	// the caller's optional diagnostic writer, line-redacted.
	cmd.Stderr = s.stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("codexauth: app-server stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("codexauth: app-server stdout: %w", err)
	}
	s.stdin = stdin
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("codexauth: start codex app-server: %w", err)
	}
	go s.read(stdout)
	initCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := s.call(initCtx, "initialize", map[string]any{
		"clientInfo": map[string]any{"name": "amux-codexauth", "title": "amux central Codex auth", "version": "1"},
	}); err != nil {
		_ = s.stop()
		return nil, fmt.Errorf("codexauth: initialize native server: %w", err)
	}
	if err := s.write(map[string]any{"method": "initialized"}); err != nil {
		_ = s.stop()
		return nil, fmt.Errorf("codexauth: initialize native server: %w", err)
	}
	return s, nil
}

func (s *server) read(stdout io.Reader) {
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64<<10), maxFrame)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) > 0 {
			s.dispatch(line)
		}
	}
	readErr := sc.Err()
	waitErr := s.cmd.Wait()
	s.mu.Lock()
	s.dead = true
	switch {
	case readErr != nil:
		s.err = fmt.Errorf("read: %w", readErr)
	case waitErr != nil:
		s.err = waitErr
	default:
		s.err = errors.New("exited")
	}
	pend := s.pending
	s.pending = map[int64]chan response{}
	s.mu.Unlock()
	for _, ch := range pend {
		ch <- response{err: &rpcError{Code: -1, Message: "native server exited"}}
	}
	close(s.done)
}

func (s *server) dispatch(line []byte) {
	var m struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		Result json.RawMessage `json:"result"`
		Error  *rpcError       `json:"error"`
	}
	if json.Unmarshal(line, &m) != nil {
		return
	}
	hasID := len(m.ID) > 0 && string(m.ID) != "null"
	switch {
	case m.Method != "" && hasID:
		// The central server has no threads, so it has nothing legitimate to ask.
		_ = s.write(map[string]any{"id": m.ID, "error": rpcError{Code: -32601, Message: "unsupported server request"}})
	case m.Method != "":
		s.mu.Lock()
		ch := s.subs[m.Method]
		s.mu.Unlock()
		if ch != nil {
			select {
			case ch <- append(json.RawMessage(nil), m.Params...):
			default:
			}
		}
	case hasID:
		var id int64
		if json.Unmarshal(m.ID, &id) != nil {
			return
		}
		s.mu.Lock()
		ch := s.pending[id]
		delete(s.pending, id)
		s.mu.Unlock()
		if ch != nil {
			ch <- response{result: m.Result, err: m.Error}
		}
	}
}

func (s *server) subscribe(method string) <-chan json.RawMessage {
	ch := make(chan json.RawMessage, 8)
	s.mu.Lock()
	s.subs[method] = ch
	s.mu.Unlock()
	return ch
}

func (s *server) unsubscribe(method string) {
	s.mu.Lock()
	delete(s.subs, method)
	s.mu.Unlock()
}

func (s *server) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	s.mu.Lock()
	if s.dead {
		s.mu.Unlock()
		return nil, fmt.Errorf("codexauth: %s: native server exited", method)
	}
	s.nextID++
	id := s.nextID
	ch := make(chan response, 1)
	s.pending[id] = ch
	s.mu.Unlock()
	drop := func() {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
	}
	if err := s.write(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		drop()
		return nil, fmt.Errorf("codexauth: %s: %w", method, err)
	}
	select {
	case <-ctx.Done():
		drop()
		return nil, fmt.Errorf("codexauth: %s: %w", method, ctx.Err())
	case <-s.quit:
		drop()
		return nil, ErrClosed
	case r := <-ch:
		if r.err != nil {
			return nil, fmt.Errorf("codexauth: %s: %w", method, r.err)
		}
		return r.result, nil
	}
}

func (s *server) write(obj any) error {
	b, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err = s.stdin.Write(append(b, '\n'))
	return err
}

func (s *server) alive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.dead
}

func (s *server) exitErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil {
		return errors.New("exited")
	}
	return s.err
}

// stop closes stdin, signals the whole process group, and escalates to SIGKILL,
// returning only after the reader has reaped the process.
func (s *server) stop() error {
	_ = s.stdin.Close()
	pid := s.cmd.Process.Pid
	select {
	case <-s.done:
		return nil
	default:
	}
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	select {
	case <-s.done:
	case <-time.After(stopTimeout):
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		<-s.done
	}
	return nil
}

// tokenPattern matches JWTs, OpenAI-style keys, token-bearing JSON members,
// bearer headers, and any long opaque credential-shaped run.
var tokenPattern = regexp.MustCompile(
	`eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]*` +
		`|(?i)"[a-z_]*(token|key|secret)[a-z_]*"\s*:\s*"[^"]*"` +
		`|(?i)bearer\s+\S+` +
		`|sk-[A-Za-z0-9_-]{8,}` +
		`|[A-Za-z0-9._~+/=-]{40,}`)

func redact(s string) string { return tokenPattern.ReplaceAllString(s, "<redacted>") }

// redactingWriter redacts native stderr line by line before forwarding it. A
// partial line is held until its newline; output is discarded without out.
type redactingWriter struct {
	out io.Writer

	mu      sync.Mutex
	partial []byte
}

func (r *redactingWriter) Write(p []byte) (int, error) {
	if r.out == nil {
		return len(p), nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.partial = append(r.partial, p...)
	for {
		i := bytes.IndexByte(r.partial, '\n')
		if i < 0 {
			if len(r.partial) > maxFrame {
				r.partial = r.partial[:0] // never retain an unbounded unterminated line
			}
			return len(p), nil
		}
		line := redact(string(r.partial[:i])) + "\n"
		r.partial = r.partial[i+1:]
		_, _ = io.WriteString(r.out, line)
	}
}
