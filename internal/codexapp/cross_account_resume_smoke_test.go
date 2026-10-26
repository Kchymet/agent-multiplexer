package codexapp

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestSmokeCrossAccountThreadResume is the native evidence for continuing one
// Codex thread after the central ChatGPT login is replaced by another account.
// It drives the REAL App Server (AMUX_TEST_CODEX_BIN, CI-pinned 0.157.1) against
// a local TLS fake of the ChatGPT backend with synthetic, unsigned JWTs: no
// real account, credential or network is involved.
//
// Phase 1 runs a turn as account A. The App Server is then stopped (a
// controlled restart) and phase 2 resumes the SAME thread id while injecting
// account B through the same externally managed token mode. Asserted:
//   - the native server accepts account B's tokens for the resumed thread;
//   - thread/resume keeps the exact thread id and the phase-1 history;
//   - phase 2's model request is sent as account B and carries the phase-1
//     conversation, including the reasoning item's opaque encrypted_content.
//
// The last point is the residual provider-side question: the real backend may
// refuse another account's encrypted reasoning. A fake cannot prove backend
// acceptance, so amux keeps its empty-thread fallback for that failure.
func TestSmokeCrossAccountThreadResume(t *testing.T) {
	bin := os.Getenv("AMUX_TEST_CODEX_BIN")
	if bin == "" {
		t.Skip("set AMUX_TEST_CODEX_BIN")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_STATE_HOME", root)
	t.Setenv("XDG_RUNTIME_DIR", root)

	b := &crossAccountBackend{}
	caPEM, leaf := smokeCA(t)
	srv := httptest.NewUnstartedServer(b)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{leaf}}
	srv.StartTLS()
	defer srv.Close()
	caPath := filepath.Join(root, "ca.pem")
	if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	sessHome := filepath.Join(root, "session")
	codexHome := filepath.Join(sessHome, ".codex")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	// Request compression is disabled so the fake can read the request body.
	cfg := fmt.Sprintf("chatgpt_base_url = %q\nmodel = 'gpt-5.6'\n[features]\nenable_request_compression = false\n[projects.%q]\ntrust_level = 'trusted'\n", srv.URL+"/backend-api/", sessHome)
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	var amu sync.Mutex
	account := "acct-alpha"
	var requests []TokenRequest
	shim := providerFunc(func(_ context.Context, r TokenRequest) (ChatGPTToken, error) {
		amu.Lock()
		defer amu.Unlock()
		requests = append(requests, r)
		return ChatGPTToken{AccessToken: crossAccountJWT(account), AccountID: account, PlanType: "pro"}, nil
	})
	run := func(phase, resume, bound, prompt string) Identity {
		sup := New(Config{
			SessionID: "cross", Bin: bin, Dir: sessHome,
			Endpoint:       "unix://" + filepath.Join(root, "s.sock"),
			EventLogPath:   filepath.Join(root, "events.jsonl"),
			ChatGPTAuth:    shim,
			ResumeThreadID: resume,
			ResumeRequired: resume != "",
			ChatGPTAccount: bound,
			Env:            []string{"CODEX_CA_CERTIFICATE=" + caPath, "CODEX_HOME=" + codexHome},
		})
		if err := sup.Start(ctx, nil); err != nil {
			t.Fatalf("%s start: %v", phase, err)
		}
		defer sup.Close()
		col := newSmokeCollector(sup.Subscribe(ctx, 0))
		if err := sup.Prompt(ctx, prompt); err != nil {
			t.Fatalf("%s prompt: %v", phase, err)
		}
		if _, ok := col.awaitTurnEnd(45 * time.Second); !ok {
			t.Fatalf("%s: turn did not end; backend saw %v", phase, b.snapshot())
		}
		return sup.Identity()
	}

	first := run("phase 1", "", "", "PROMPT-ONE remember the word lighthouse")
	if first.ThreadID == "" || first.ChatGPTAccount != "acct-alpha" {
		t.Fatalf("phase 1 identity = %+v", first)
	}

	// Controlled restart: the central login now belongs to another account, so
	// the new process carries no bound-account constraint.
	amu.Lock()
	account = "acct-bravo"
	amu.Unlock()
	second := run("phase 2", first.ThreadID, "", "PROMPT-TWO what was the word")
	if second.ThreadID != first.ThreadID {
		t.Fatalf("resume changed thread: %s -> %s", first.ThreadID, second.ThreadID)
	}
	if second.ChatGPTAccount != "acct-bravo" {
		t.Fatalf("phase 2 bound account = %q, want acct-bravo", second.ChatGPTAccount)
	}
	turns := b.turns()
	if len(turns) != 2 || turns[0].account != "acct-alpha" || turns[1].account != "acct-bravo" {
		t.Fatalf("model requests = %+v, want one per account", turns)
	}
	history := crossAccountHistory(t, turns[1].body)
	t.Logf("phase 2 request as %s carries: %q", turns[1].account, history)
	// The resumed request replays the exact phase 1 conversation in order:
	// prompt, the other account's opaque reasoning, the assistant reply, and
	// then the new prompt, with no server-side previous_response_id.
	want := []string{"user:PROMPT-ONE", "reasoning:ENC-acct-alpha-1", "assistant:answer 1", "user:PROMPT-TWO"}
	if !crossAccountSubsequence(history, want) {
		t.Fatalf("phase 2 history = %q, want ordered %q", history, want)
	}

	// A provider failure on a turn (for example a backend refusing another
	// account's encrypted reasoning) fails that turn only: the identity keeps
	// the same thread and the next restart still resumes its full history.
	amu.Lock()
	account = "acct-charlie"
	amu.Unlock()
	b.setFail(true)
	third := run("phase 3", first.ThreadID, "", "PROMPT-THREE after a provider refusal")
	if third.ThreadID != first.ThreadID {
		t.Fatalf("turn failure changed thread: %s -> %s", first.ThreadID, third.ThreadID)
	}
	b.setFail(false)
	fourth := run("phase 4", first.ThreadID, "", "PROMPT-FOUR recovered")
	turns = b.turns()
	history = crossAccountHistory(t, turns[len(turns)-1].body)
	if fourth.ThreadID != first.ThreadID || !crossAccountSubsequence(history, append(want, "user:PROMPT-FOUR")) {
		t.Fatalf("recovery lost the thread or history: %s %q", fourth.ThreadID, history)
	}

	// Within one process the bound account is a constraint (reconnect): a
	// different account's token is refused rather than adopted.
	sup := New(Config{
		SessionID: "cross", Bin: bin, Dir: sessHome,
		Endpoint:     "unix://" + filepath.Join(root, "s.sock"),
		EventLogPath: filepath.Join(root, "events.jsonl"), ChatGPTAuth: shim,
		ResumeThreadID: first.ThreadID, ResumeRequired: true, ChatGPTAccount: "acct-bravo",
		Env: []string{"CODEX_CA_CERTIFICATE=" + caPath, "CODEX_HOME=" + codexHome},
	})
	if err := sup.Start(ctx, nil); err == nil {
		_ = sup.Close()
		t.Fatal("a bound account accepted another account's token")
	}
}

// crossAccountHistory reduces a Responses request's input to role:text and
// reasoning:encrypted_content entries.
func crossAccountHistory(t *testing.T, body string) []string {
	t.Helper()
	var req struct {
		PreviousResponseID string `json:"previous_response_id"`
		Input              []struct {
			Type             string `json:"type"`
			Role             string `json:"role"`
			EncryptedContent string `json:"encrypted_content"`
			Content          []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"input"`
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode model request: %v", err)
	}
	if req.PreviousResponseID != "" {
		t.Fatalf("request relies on server state: previous_response_id=%q", req.PreviousResponseID)
	}
	var out []string
	for _, in := range req.Input {
		switch {
		case in.Type == "reasoning":
			out = append(out, "reasoning:"+in.EncryptedContent)
		case in.Type == "message" && (in.Role == "user" || in.Role == "assistant") && len(in.Content) > 0:
			text := in.Content[0].Text
			if strings.HasPrefix(text, "PROMPT-") {
				text, _, _ = strings.Cut(text, " ")
			}
			if in.Role == "assistant" || strings.HasPrefix(text, "PROMPT-") {
				out = append(out, in.Role+":"+text)
			}
		}
	}
	return out
}

func crossAccountSubsequence(got, want []string) bool {
	i := 0
	for _, g := range got {
		if i < len(want) && g == want[i] {
			i++
		}
	}
	return i == len(want)
}

type crossAccountTurn struct{ account, body string }

type crossAccountBackend struct {
	mu    sync.Mutex
	paths []string
	reqs  []crossAccountTurn
	fail  bool
}

func (b *crossAccountBackend) setFail(v bool) {
	b.mu.Lock()
	b.fail = v
	b.mu.Unlock()
}

func (b *crossAccountBackend) snapshot() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.paths...)
}

func (b *crossAccountBackend) turns() []crossAccountTurn {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]crossAccountTurn(nil), b.reqs...)
}

func (b *crossAccountBackend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	acct := r.Header.Get("chatgpt-account-id")
	b.mu.Lock()
	b.paths = append(b.paths, r.Method+" "+r.URL.Path)
	b.mu.Unlock()
	switch {
	case strings.HasSuffix(r.URL.Path, "/wham/accounts/check"):
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accounts": []map[string]any{{"id": acct, "plan_type": "pro", "name": "fixture",
				"workspace_backend_origin": "NO_CONSTRAINT", "account_routing_override": "NO_CONSTRAINT"}},
			"account_ordering": []string{acct}, "default_account_id": acct,
		})
		return
	case strings.HasSuffix(r.URL.Path, "/codex/responses"):
	default:
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
		return
	}
	b.mu.Lock()
	if r.Method != http.MethodPost {
		b.mu.Unlock()
		http.Error(w, "websocket unsupported", http.StatusNotFound)
		return
	}
	if b.fail {
		b.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","code":"invalid_encrypted_content","message":"The encrypted content could not be verified."}}`))
		return
	}
	b.reqs = append(b.reqs, crossAccountTurn{account: acct, body: string(body)})
	n := len(b.reqs)
	b.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	id := fmt.Sprintf("resp_%d", n)
	reasoning := map[string]any{"type": "reasoning", "id": fmt.Sprintf("rs_%d", n), "summary": []any{},
		"encrypted_content": fmt.Sprintf("ENC-%s-%d", acct, n)}
	msg := map[string]any{"type": "message", "id": fmt.Sprintf("msg_%d", n), "role": "assistant", "status": "completed",
		"content": []map[string]any{{"type": "output_text", "text": fmt.Sprintf("answer %d", n)}}}
	for _, ev := range []map[string]any{
		{"type": "response.created", "response": map[string]any{"id": id}},
		{"type": "response.output_item.done", "item": reasoning},
		{"type": "response.output_item.done", "item": msg},
		{"type": "response.completed", "response": map[string]any{"id": id, "usage": map[string]any{
			"input_tokens": 10, "output_tokens": 5, "total_tokens": 15,
			"input_tokens_details": map[string]any{"cached_tokens": 0}, "output_tokens_details": map[string]any{"reasoning_tokens": 0}}}},
	} {
		raw, _ := json.Marshal(ev)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev["type"], raw)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
}

// crossAccountJWT is an unsigned ChatGPT-shaped JWT for a synthetic account.
func crossAccountJWT(account string) string {
	enc := base64.RawURLEncoding.EncodeToString
	header, _ := json.Marshal(map[string]any{"alg": "none", "typ": "JWT"})
	claims, _ := json.Marshal(map[string]any{
		"exp":                            time.Now().Add(time.Hour).Unix(),
		"email":                          account + "@example.invalid",
		"https://api.openai.com/auth":    map[string]any{"chatgpt_account_id": account, "chatgpt_plan_type": "pro", "chatgpt_user_id": "user-" + account},
		"https://api.openai.com/profile": map[string]any{"email": account + "@example.invalid"},
	})
	return enc(header) + "." + enc(claims) + ".sig"
}
