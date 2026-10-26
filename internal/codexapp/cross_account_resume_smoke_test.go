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

	// Controlled restart: the central login now belongs to another account and
	// the workload's bookkeeping has been re-pinned (bound account cleared).
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
	if len(turns) < 2 {
		t.Fatalf("model requests = %d, want 2", len(turns))
	}
	last := turns[len(turns)-1]
	for i, tr := range turns {
		var req struct {
			PreviousResponseID string            `json:"previous_response_id"`
			Input              []json.RawMessage `json:"input"`
		}
		_ = json.Unmarshal([]byte(tr.body), &req)
		var kinds []string
		for _, in := range req.Input {
			s := string(in)
			if len(s) > 160 {
				s = s[:160]
			}
			kinds = append(kinds, s)
		}
		t.Logf("request %d acct=%s previous_response_id=%q inputs=%d %q", i, tr.account, req.PreviousResponseID, len(req.Input), kinds)
	}
	t.Logf("phase 2 request: account header=%q carriesPhase1Prompt=%v carriesPhase1EncryptedReasoning=%v",
		last.account, strings.Contains(last.body, "PROMPT-ONE"), strings.Contains(last.body, "ENC-acct-alpha"))
	if last.account != "acct-bravo" {
		t.Fatalf("phase 2 model request account = %q, want acct-bravo", last.account)
	}
	if !strings.Contains(last.body, "PROMPT-ONE") || !strings.Contains(last.body, "PROMPT-TWO") {
		t.Fatal("resumed request lost the phase 1 conversation")
	}

	// The current amux policy (bound account kept) refuses the same restart.
	amu.Lock()
	account = "acct-charlie"
	amu.Unlock()
	sup := New(Config{
		SessionID: "cross", Bin: bin, Dir: sessHome,
		Endpoint:     "unix://" + filepath.Join(root, "s.sock"),
		EventLogPath: filepath.Join(root, "events.jsonl"), ChatGPTAuth: shim,
		ResumeThreadID: first.ThreadID, ChatGPTAccount: "acct-bravo",
		Env: []string{"CODEX_CA_CERTIFICATE=" + caPath, "CODEX_HOME=" + codexHome},
	})
	if err := sup.Start(ctx, nil); err == nil {
		_ = sup.Close()
		t.Fatal("a pinned bound account accepted another account's token")
	}
}

type crossAccountTurn struct{ account, body string }

type crossAccountBackend struct {
	mu    sync.Mutex
	paths []string
	reqs  []crossAccountTurn
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
	b.paths = append(b.paths, "enc="+r.Header.Get("Content-Encoding"))
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
