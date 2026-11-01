package direct

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"amux/internal/daemon"
	"amux/internal/hostbackend"

	"github.com/gorilla/websocket"
)

// Config configures a Server.
type Config struct {
	Authority    *Authority
	Backend      Backend
	Endpoints    []string // canonical https origins clients dial
	AllowOrigins []string // exact browser origins allowed to call (CORS / WebSocket Origin)
	Now          func() time.Time
	WatchPoll    time.Duration // inventory poll for watch; default 2s
	Revalidate   time.Duration // stream scope revalidation; default 5s
	Keepalive    time.Duration // SSE keepalive; default 15s
	Logf         func(format string, args ...any)
}

// Server is the direct HTTP surface.
type Server struct {
	cfg       Config
	hosts     map[string]string // Host header -> canonical endpoint
	origins   map[string]bool
	mux       *http.ServeMux
	limiterMu sync.Mutex
	failures  map[string][]time.Time // per source address
}

// NewServer validates the configuration and builds the handler.
func NewServer(cfg Config) (*Server, error) {
	if cfg.Authority == nil || cfg.Backend == nil {
		return nil, errors.New("direct server needs an authority and a backend")
	}
	if len(cfg.Endpoints) == 0 {
		return nil, errors.New("direct server needs at least one --endpoint")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.WatchPoll <= 0 {
		cfg.WatchPoll = 2 * time.Second
	}
	if cfg.Revalidate <= 0 {
		cfg.Revalidate = 5 * time.Second
	}
	if cfg.Keepalive <= 0 {
		cfg.Keepalive = 15 * time.Second
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	s := &Server{cfg: cfg, hosts: map[string]string{}, origins: map[string]bool{}, failures: map[string][]time.Time{}}
	for i, e := range cfg.Endpoints {
		c, err := CanonicalOrigin(e, false)
		if err != nil {
			return nil, fmt.Errorf("endpoint: %w", err)
		}
		if c != e {
			return nil, fmt.Errorf("endpoint %q is not canonical (use %q)", e, c)
		}
		cfg.Endpoints[i] = c
		for _, h := range hostKeys(c) {
			s.hosts[h] = c
		}
	}
	for _, o := range cfg.AllowOrigins {
		c, err := CanonicalOrigin(o, true)
		if err != nil || c != o {
			return nil, fmt.Errorf("allowed origin %q must be an exact canonical origin", o)
		}
		s.origins[o] = true
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+PathHello, s.hello)
	mux.HandleFunc("POST "+PathToken, s.token)
	mux.HandleFunc("POST "+PathRevoke, s.revoke)
	mux.HandleFunc("GET "+PathSession, s.sessions)
	mux.HandleFunc("GET "+PathWatch, s.watch)
	mux.HandleFunc("GET "+PathSession+"/{sid}/history", s.history)
	mux.HandleFunc("GET "+PathSession+"/{sid}/stream", s.stream)
	mux.HandleFunc("POST "+PathSession+"/{sid}/actions", s.actions)
	s.mux = mux
	return s, nil
}

// ServeHTTP gates every request on Host and Origin before routing.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	endpoint, ok := s.hosts[strings.ToLower(r.Host)]
	if !ok {
		writeErr(w, http.StatusMisdirectedRequest, CodeEndpointNotBound, "this host name is not a configured endpoint")
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), endpointKey{}, endpoint))
	if origin := r.Header.Get("Origin"); origin != "" {
		if !s.origins[origin] {
			writeErr(w, http.StatusForbidden, CodeOriginNotAllowed, "origin not allowed")
			return
		}
		h.Set("Access-Control-Allow-Origin", origin)
		h.Add("Vary", "Origin")
		if r.Method == http.MethodOptions {
			h.Set("Access-Control-Allow-Methods", "GET, POST")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			h.Set("Access-Control-Max-Age", "600")
			if r.Header.Get("Access-Control-Request-Private-Network") == "true" {
				h.Set("Access-Control-Allow-Private-Network", "true")
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	s.mux.ServeHTTP(w, r)
}

type endpointKey struct{}

func endpointOf(r *http.Request) string { v, _ := r.Context().Value(endpointKey{}).(string); return v }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	switch status {
	case http.StatusUnauthorized:
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
	case http.StatusForbidden:
		if code == CodeDenied {
			w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_scope", scope="steer"`)
		}
	}
	writeJSON(w, status, APIError{Code: code, Error: msg})
}

func writeOAuth(w http.ResponseWriter, status int, code, desc string) {
	writeJSON(w, status, OAuthError{Error: code, Description: desc})
}

func decodeJSON(r *http.Request, w http.ResponseWriter, v any) error {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		return errors.New("content type must be application/json")
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxJSONBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data")
	}
	return nil
}

// ── hello ──────────────────────────────────────────────────────────────────

func (s *Server) hello(w http.ResponseWriter, r *http.Request) {
	var req HelloRequest
	if err := decodeJSON(r, w, &req); err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "invalid hello request")
		return
	}
	if req.Protocol != Protocol {
		writeErr(w, http.StatusBadRequest, CodeProtocol, "unsupported protocol")
		return
	}
	if !validNonce(req.ClientNonce) {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "client_nonce must be base64url of 32 bytes")
		return
	}
	if req.Endpoint != endpointOf(r) {
		writeErr(w, http.StatusMisdirectedRequest, CodeEndpointNotBound, "endpoint does not match the origin this request reached")
		return
	}
	proof, err := s.cfg.Authority.Identity().SignHello(req, s.cfg.Now())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, CodeInternal, "could not sign hello")
		return
	}
	writeJSON(w, http.StatusOK, HelloResponse{Proof: proof})
}

// ── token / revoke (RFC 6749, RFC 7009) ────────────────────────────────────

// Token endpoint abuse limits. Failures are counted per source address, so one
// peer presenting bad codes or tokens locks out only itself; other clients'
// renewals are unaffected. Memory is bounded by maxTrackedSources.
const (
	failureWindow     = time.Minute
	maxFailures       = 5
	maxTrackedSources = 4096
)

func source(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) limited(src string) bool {
	s.limiterMu.Lock()
	defer s.limiterMu.Unlock()
	cutoff := s.cfg.Now().Add(-failureWindow)
	kept := s.failures[src][:0]
	for _, t := range s.failures[src] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(s.failures, src)
		return false
	}
	s.failures[src] = kept
	return len(kept) >= maxFailures
}

func (s *Server) recordFailure(src string) {
	s.limiterMu.Lock()
	defer s.limiterMu.Unlock()
	if _, ok := s.failures[src]; !ok && len(s.failures) >= maxTrackedSources {
		// Drop sources whose window has passed; if all are live, forget the
		// oldest rather than grow without bound.
		cutoff := s.cfg.Now().Add(-failureWindow)
		oldest, oldestAt := "", s.cfg.Now()
		for k, ts := range s.failures {
			if len(ts) == 0 || !ts[len(ts)-1].After(cutoff) {
				delete(s.failures, k)
				continue
			}
			if ts[len(ts)-1].Before(oldestAt) {
				oldest, oldestAt = k, ts[len(ts)-1]
			}
		}
		if len(s.failures) >= maxTrackedSources {
			delete(s.failures, oldest)
		}
	}
	s.failures[src] = append(s.failures[src], s.cfg.Now())
}

func parseForm(w http.ResponseWriter, r *http.Request) (url.Values, error) {
	if r.URL.RawQuery != "" {
		return nil, errors.New("parameters must be in the form body, never the URL")
	}
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
		return nil, errors.New("content type must be application/x-www-form-urlencoded")
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxFormBody))
	if err != nil {
		return nil, err
	}
	vals, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, err
	}
	for k, v := range vals {
		if len(v) != 1 {
			return nil, fmt.Errorf("parameter %s repeated", k)
		}
	}
	return vals, nil
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	src := source(r)
	if s.limited(src) {
		w.Header().Set("Retry-After", "60")
		writeOAuth(w, http.StatusTooManyRequests, OAuthTemporarilyUnavail, "too many failed attempts; retry later")
		return
	}
	vals, err := parseForm(w, r)
	if err != nil {
		writeOAuth(w, http.StatusBadRequest, OAuthInvalidRequest, err.Error())
		return
	}
	var resp TokenResponse
	switch vals.Get("grant_type") {
	case GrantTypeEnrollmentCode:
		code := vals.Get("code")
		if code == "" {
			writeOAuth(w, http.StatusBadRequest, OAuthInvalidRequest, "code is required")
			return
		}
		resp, err = s.cfg.Authority.Redeem(code, vals.Get("client_label"), vals.Get("client_kind"), vals.Get("code_verifier"))
	case GrantTypeRefresh:
		rt := vals.Get("refresh_token")
		if rt == "" {
			writeOAuth(w, http.StatusBadRequest, OAuthInvalidRequest, "refresh_token is required")
			return
		}
		resp, err = s.cfg.Authority.Refresh(rt)
	default:
		writeOAuth(w, http.StatusBadRequest, OAuthUnsupportedGrantType, "unsupported grant_type")
		return
	}
	var reqErr *requestError
	switch {
	case errors.As(err, &reqErr):
		writeOAuth(w, http.StatusBadRequest, OAuthInvalidRequest, reqErr.Error())
	case errors.Is(err, errInvalidCode):
		s.recordFailure(src)
		writeOAuth(w, http.StatusBadRequest, OAuthInvalidGrant, "the code or refresh token is invalid, expired, used or revoked")
	case err != nil && strings.Contains(err.Error(), "client_kind"), err != nil && strings.Contains(err.Error(), "label"):
		writeOAuth(w, http.StatusBadRequest, OAuthInvalidRequest, err.Error())
	case err != nil:
		// The store write failed. Whether the presented code or refresh token
		// was consumed is unknown (a write can fail after its rename). Clients
		// must not assume the old token is still usable; see docs/direct-access.md.
		s.cfg.Logf("direct: token endpoint: %v", err)
		writeOAuth(w, http.StatusServiceUnavailable, OAuthTemporarilyUnavail,
			"the grant store did not confirm the write; no token was returned, and the presented code or refresh token may already be consumed")
	default:
		w.Header().Set("Pragma", "no-cache")
		writeJSON(w, http.StatusOK, resp)
	}
}

func (s *Server) revoke(w http.ResponseWriter, r *http.Request) {
	vals, err := parseForm(w, r)
	if err != nil || vals.Get("token") == "" {
		writeOAuth(w, http.StatusBadRequest, OAuthInvalidRequest, "token is required in the form body")
		return
	}
	if err := s.cfg.Authority.RevokeToken(vals.Get("token")); err != nil {
		s.cfg.Logf("direct: revoke: %v", err)
		writeOAuth(w, http.StatusServiceUnavailable, OAuthTemporarilyUnavail, "revocation did not complete")
		return
	}
	w.WriteHeader(http.StatusOK)
}

// ── authenticated resource routes ──────────────────────────────────────────

func bearer(r *http.Request) string {
	v := r.Header.Get("Authorization")
	if len(v) > 7 && strings.EqualFold(v[:7], "Bearer ") {
		return strings.TrimSpace(v[7:])
	}
	return ""
}

// admit authenticates and authorizes perm. It makes no backend call.
func (s *Server) admit(w http.ResponseWriter, r *http.Request, perm string) *Lease {
	l, err := s.cfg.Authority.Admit(r.Context(), bearer(r), perm)
	if err != nil {
		writeAuthErr(w, err)
		return nil
	}
	return l
}

func writeAuthErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errAuthExpired):
		writeErr(w, http.StatusUnauthorized, CodeAuthExpired, "access token expired")
	case errors.Is(err, errAuthRevoked):
		writeErr(w, http.StatusUnauthorized, CodeAuthRevoked, "access revoked")
	case errors.Is(err, errDenied):
		writeErr(w, http.StatusForbidden, CodeDenied, "this client may not perform that operation")
	default:
		writeErr(w, http.StatusUnauthorized, CodeAuthInvalid, "missing or unknown access token")
	}
}

// writeBackendErr reports a backend failure without host detail.
func (s *Server) writeBackendErr(w http.ResponseWriter, l *Lease, err error) {
	switch {
	case l.Revoked():
		writeErr(w, http.StatusUnauthorized, CodeAuthRevoked, "access revoked")
	case l.Expired():
		writeErr(w, http.StatusUnauthorized, CodeAuthExpired, "access token expired")
	case errors.Is(err, errNotFound):
		writeErr(w, http.StatusNotFound, CodeNotFound, "no such session")
	case errors.Is(err, hostbackend.ErrUnavailable):
		writeErr(w, http.StatusServiceUnavailable, CodeUnavailable, "the amux daemon is not reachable")
	default:
		s.cfg.Logf("direct: backend: %v", err)
		writeErr(w, http.StatusServiceUnavailable, CodeUnavailable, "the amux daemon could not answer")
	}
}

func (s *Server) sessions(w http.ResponseWriter, r *http.Request) {
	l := s.admit(w, r, PermRead)
	if l == nil {
		return
	}
	defer l.Done()
	rows, err := s.cfg.Backend.Snapshot(l.Context())
	if err != nil {
		s.writeBackendErr(w, l, err)
		return
	}
	writeJSON(w, http.StatusOK, SessionsResponse{ServerID: s.cfg.Authority.ServerID(), Sessions: filtered(l.Grant, rows)})
}

func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("sid")
	q := r.URL.Query()
	before, limit := int64(0), 100
	for k := range q {
		if k != "before_seq" && k != "limit" {
			writeErr(w, http.StatusBadRequest, CodeBadRequest, "unknown query parameter "+k)
			return
		}
	}
	if v := q.Get("before_seq"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			writeErr(w, http.StatusBadRequest, CodeBadRequest, "before_seq must be a positive integer")
			return
		}
		before = n
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 || n > MaxHistoryLimit {
			writeErr(w, http.StatusBadRequest, CodeBadRequest, fmt.Sprintf("limit must be 1-%d", MaxHistoryLimit))
			return
		}
		limit = n
	}
	if !validSessionID(sid) {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "invalid session id")
		return
	}
	l := s.admit(w, r, PermRead)
	if l == nil {
		return
	}
	defer l.Done()
	if _, _, err := authorizeTarget(l.Context(), s.cfg.Backend, l.Grant, sid); err != nil {
		s.writeBackendErr(w, l, err)
		return
	}
	page, ok := s.cfg.Backend.History(l.Context(), sid, before, limit)
	if code := l.AuthEnded(); code != "" {
		writeErr(w, http.StatusUnauthorized, code, "access ended")
		return
	}
	resp := HistoryResponse{SessionID: sid, Events: []RuntimeEvent{}}
	if ok {
		resp.Runtime, resp.Seq, resp.HeadSeq, resp.HasMore = page.Runtime, page.Seq, page.Head, page.HasMore
		if page.Events != nil {
			resp.Events = page.Events
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("sid")
	q := r.URL.Query()
	after := int64(0)
	for k := range q {
		if k != "after" {
			writeErr(w, http.StatusBadRequest, CodeBadRequest, "unknown query parameter "+k)
			return
		}
	}
	if v := q.Get("after"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, CodeBadRequest, "after must be a non-negative integer")
			return
		}
		after = n
	}
	if !validSessionID(sid) {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "invalid session id")
		return
	}
	l := s.admit(w, r, PermRead)
	if l == nil {
		return
	}
	defer l.Done()
	if _, _, err := authorizeTarget(l.Context(), s.cfg.Backend, l.Grant, sid); err != nil {
		s.writeBackendErr(w, l, err)
		return
	}
	ctx, cancel := context.WithCancel(l.Context())
	defer cancel()
	batches, ok := s.cfg.Backend.Stream(ctx, sid, after)
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	_ = rc.Flush() // the client sees the open stream at once, before any event
	send := func(event string, v any) bool {
		data, _ := json.Marshal(v)
		_ = rc.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	if !ok {
		send("end", StreamEnd{Code: CodeNotFound})
		return
	}
	// The lease context carries both revocation and access-token expiry, and
	// cancels the backend stream and any revalidation call with it.
	ended := func() bool {
		if code := l.AuthEnded(); code != "" {
			send("auth", StreamEnd{Code: code})
			return true
		}
		return false
	}
	keep := time.NewTicker(s.cfg.Keepalive)
	defer keep.Stop()
	validated := s.cfg.Now()
	for {
		select {
		case <-l.Context().Done():
			ended()
			return
		case <-r.Context().Done():
			return
		case <-keep.C:
			_ = rc.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil || rc.Flush() != nil {
				return
			}
		case b, open := <-batches:
			if !open {
				if !ended() {
					send("end", StreamEnd{Code: CodeUnavailable})
				}
				return
			}
			if len(b.Events) == 0 {
				continue
			}
			if s.cfg.Now().Sub(validated) >= s.cfg.Revalidate {
				if _, _, err := authorizeTarget(l.Context(), s.cfg.Backend, l.Grant, sid); err != nil {
					if !ended() {
						code := CodeNotFound
						if !errors.Is(err, errNotFound) {
							code = CodeUnavailable
						}
						send("end", StreamEnd{Code: code})
					}
					return
				}
				validated = s.cfg.Now()
			}
			if l.Context().Err() != nil {
				ended() // never send data after expiry or revocation
				return
			}
			if !send("events", StreamBatch{Seq: b.Seq, Runtime: b.Runtime, Events: b.Events}) {
				return
			}
		}
	}
}

func (s *Server) actions(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("sid")
	if !validSessionID(sid) {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "invalid session id")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxJSONBody))
	if err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "body too large")
		return
	}
	l := s.admit(w, r, PermSteer)
	if l == nil {
		return
	}
	defer l.Done()
	var req ActionRequest
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "content type must be application/json")
		return
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || dec.More() {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "invalid action request")
		return
	}
	if req.IdemKey == "" || len(req.IdemKey) > MaxIdemKeyLen || strings.ContainsAny(req.IdemKey, "\x00\r\n") {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "idem_key is required (1-128 bytes)")
		return
	}
	act, err := actionFor(sid, req)
	if errors.Is(err, errUnsupported) {
		writeErr(w, http.StatusConflict, CodeUnsupportedVerb, "this verb is not available on direct connections")
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "invalid fields for "+req.Verb)
		return
	}
	bodyHash := actionHash(sid, req)
	if out, rec, err := s.cfg.Authority.BeginAction(l, req.IdemKey, bodyHash, true); err != nil {
		writeAuthErr(w, err)
		return
	} else if s.idemReply(w, out, rec) {
		return
	}
	row, byID, err := authorizeTarget(l.Context(), s.cfg.Backend, l.Grant, sid)
	if err != nil {
		s.writeBackendErr(w, l, err)
		return
	}
	if err := steerable(row, byID, req.Verb); err != nil {
		if errors.Is(err, errArchived) {
			writeErr(w, http.StatusConflict, CodeArchived, "the session or its workgroup is archived")
		} else {
			writeErr(w, http.StatusConflict, CodeUnsupportedVerb, "this session cannot serve "+req.Verb)
		}
		return
	}
	if code := l.AuthEnded(); code != "" { // expiry before execution still denies
		writeErr(w, http.StatusUnauthorized, code, "access ended before the action ran")
		return
	}
	out, rec, err := s.cfg.Authority.BeginAction(l, req.IdemKey, bodyHash, false)
	switch {
	case errors.Is(err, errAuthRevoked):
		writeAuthErr(w, err)
		return
	case errors.Is(err, errCapacity):
		// Not a temporary throttle: retrying will not help. The client must
		// enroll again (a new grant starts with an empty record set).
		writeErr(w, http.StatusConflict, CodeCapacity, err.Error())
		return
	case err != nil:
		writeErr(w, http.StatusServiceUnavailable, CodeUnavailable, "could not record the action; it was not executed")
		return
	}
	if s.idemReply(w, out, rec) {
		return
	}
	if code := l.AuthEnded(); code != "" {
		s.cfg.Authority.AbortAction(l, req.IdemKey)
		writeErr(w, http.StatusUnauthorized, code, "access ended before the action ran")
		return
	}
	// From here the action is admitted: it runs under a bounded execution
	// lifetime that only revocation cuts short, not token expiry or disconnect.
	exec, cancel := l.ExecContext(actionExecLimit)
	_, err = s.cfg.Backend.Apply(exec, act)
	cancel()
	var rejected *daemon.RejectedError
	switch {
	case err == nil:
		s.cfg.Authority.FinishAction(l, req.IdemKey, true, "", "")
		writeJSON(w, http.StatusOK, ActionResponse{OK: true})
	case errors.As(err, &rejected):
		// The daemon answered not-OK. That is a known answer, not proof of no
		// side effect; it is recorded and replayed, never re-run. (For S1, a
		// client repeating a stop under a new key is safe.)
		s.cfg.Logf("direct: action %s on %s rejected: %v", req.Verb, sid, err)
		s.cfg.Authority.FinishAction(l, req.IdemKey, false, CodeRejected, "the daemon rejected the action")
		writeErr(w, http.StatusUnprocessableEntity, CodeRejected, "the daemon rejected the action")
	case errors.Is(err, hostbackend.ErrUnavailable):
		// The call never reached an authenticated daemon: nothing ran.
		s.cfg.Authority.FinishAction(l, req.IdemKey, false, CodeUnavailable, "the amux daemon was not reachable; the action did not run")
		writeErr(w, http.StatusServiceUnavailable, CodeUnavailable, "the amux daemon was not reachable; the action did not run")
	default:
		// Cancelled (revocation or the execution bound) or a transport failure
		// after dispatch: the daemon may or may not have applied it.
		s.cfg.Authority.AmbiguousAction(l, req.IdemKey)
		if l.Revoked() {
			writeErr(w, http.StatusUnauthorized, CodeAuthRevoked, "access revoked during the action; its outcome is unknown")
		} else {
			writeErr(w, http.StatusConflict, CodeIdemAmbiguous, "the action's outcome is unknown; it will not run again with this idem_key")
		}
	}
}

// actionExecLimit bounds one admitted daemon call.
const actionExecLimit = 60 * time.Second

func actionHash(sid string, req ActionRequest) string {
	canon, _ := json.Marshal(struct {
		SID    string            `json:"sid"`
		Verb   string            `json:"verb"`
		Fields map[string]string `json:"fields"`
	}{sid, req.Verb, req.Fields})
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:])
}

// idemReply answers a non-proceed idempotency outcome; it reports whether it did.
func (s *Server) idemReply(w http.ResponseWriter, out idemOutcome, rec *idemRecord) bool {
	switch out {
	case idemReplay:
		if rec.OK {
			writeJSON(w, http.StatusOK, ActionResponse{OK: true})
		} else {
			status := http.StatusUnprocessableEntity
			if rec.Code == CodeUnavailable {
				status = http.StatusServiceUnavailable
			}
			writeErr(w, status, rec.Code, rec.Error)
		}
	case idemConflict:
		writeErr(w, http.StatusUnprocessableEntity, CodeIdemConflict, "idem_key was used with a different request")
	case idemInProgress:
		writeErr(w, http.StatusConflict, CodeIdemInProgress, "an action with this idem_key is still running")
	case idemAmbiguous:
		writeErr(w, http.StatusConflict, CodeIdemAmbiguous, "an action with this idem_key has an unknown outcome; it will not run again")
	default:
		return false
	}
	return true
}

// ── watch (WebSocket) ──────────────────────────────────────────────────────

func (s *Server) watch(w http.ResponseWriter, r *http.Request) {
	up := websocket.Upgrader{
		HandshakeTimeout: 10 * time.Second,
		// ServeHTTP already refused a disallowed Origin; an absent one is a
		// non-browser client.
		CheckOrigin: func(r *http.Request) bool { o := r.Header.Get("Origin"); return o == "" || s.origins[o] },
	}
	conn, err := up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetReadLimit(MaxWatchAuthSize)
	closeWith := func(code int, reason string) {
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(time.Second))
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var auth WatchAuth
	if err := conn.ReadJSON(&auth); err != nil || auth.Type != "auth" {
		closeWith(CloseAuthInvalid, CodeAuthInvalid)
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	l, err := s.cfg.Authority.Admit(r.Context(), auth.AccessToken, PermRead)
	if err != nil {
		switch {
		case errors.Is(err, errAuthExpired):
			closeWith(CloseAuthExpired, CodeAuthExpired)
		case errors.Is(err, errAuthRevoked):
			closeWith(CloseAuthRevoked, CodeAuthRevoked)
		case errors.Is(err, errDenied):
			closeWith(CloseDenied, CodeDenied)
		default:
			closeWith(CloseAuthInvalid, CodeAuthInvalid)
		}
		return
	}
	defer l.Done()
	ctx, cancel := context.WithCancel(l.Context())
	defer cancel()
	go func() { // reader: notice the client going away; no further client frames are meaningful
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				cancel()
				return
			}
		}
	}()
	var wmu sync.Mutex
	write := func(v any) bool {
		wmu.Lock()
		defer wmu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		return conn.WriteJSON(v) == nil
	}
	if !write(WatchWelcome{Type: "welcome", ServerID: s.cfg.Authority.ServerID(), ExpiresAt: l.ExpiresAt}) {
		return
	}
	poll := time.NewTicker(s.cfg.WatchPoll)
	defer poll.Stop()
	var last []byte
	push := func() bool {
		rows, err := s.cfg.Backend.Snapshot(ctx)
		if err != nil {
			// A daemon failure keeps the socket and retries next tick. If the
			// lease ended, the loop's ctx.Done branch sends the auth close code;
			// returning false here would drop the socket without one.
			return true
		}
		if ctx.Err() != nil {
			return true // the loop reports why; never send data after the lease ended
		}
		snap := WatchSnapshot{Type: "snapshot", Sessions: filtered(l.Grant, rows)}
		data, _ := json.Marshal(snap)
		if bytes.Equal(data, last) {
			return true
		}
		last = data
		return write(snap)
	}
	if !push() {
		return
	}
	for {
		select {
		case <-ctx.Done():
			switch l.AuthEnded() {
			case CodeAuthRevoked:
				closeWith(CloseAuthRevoked, CodeAuthRevoked)
			case CodeAuthExpired:
				closeWith(CloseAuthExpired, CodeAuthExpired)
			}
			return
		case <-poll.C:
			if !push() {
				return
			}
		}
	}
}
