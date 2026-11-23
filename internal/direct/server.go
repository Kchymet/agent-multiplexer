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

	"slices"

	"amux/internal/core"
	"amux/internal/daemon"
	"amux/internal/hostbackend"
	amuxstore "amux/internal/store"
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
	// Wake serves the direct wake routes; nil answers them wake_disabled.
	Wake *WakeService
	// Approvals bridges POST /direct/v1/enroll/cloud verifies to the provider
	// (direct-approval-v1); nil answers them orchestrator_unreachable.
	Approvals *ApprovalQueue
	// ExtraOrigins, when set, supplies further exact browser origins at
	// request time (the Harness web origin the provider learned from its
	// registration, registered.webOrigin). Invalid values are ignored.
	ExtraOrigins func() []string
}

// Server is the direct HTTP surface.
type Server struct {
	cfg       Config
	hosts     map[string]string // Host header -> canonical endpoint
	origins   map[string]bool
	mux       *http.ServeMux
	limiterMu sync.Mutex
	failures  map[string][]time.Time // per source address
	jobs      *creationJobs          // async creations (creations.go)
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
	s := &Server{cfg: cfg, hosts: map[string]string{}, origins: map[string]bool{}, failures: map[string][]time.Time{}, jobs: newCreationJobs()}
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
	mux.HandleFunc("POST "+PathClaim, s.claim)
	mux.HandleFunc("POST "+PathEnrollCloud, s.enrollCloud)
	mux.HandleFunc("GET "+PathSession, s.sessions)
	mux.HandleFunc("GET "+PathWatch, s.watch)
	mux.HandleFunc("GET "+PathSession+"/{sid}/history", s.history)
	mux.HandleFunc("GET "+PathSession+"/{sid}/stream", s.stream)
	mux.HandleFunc("POST "+PathSession+"/{sid}/actions", s.actions)
	mux.HandleFunc("POST "+PathWorkgroups, s.createWorkgroup)
	mux.HandleFunc("GET "+PathCapabilities, s.capabilities)
	mux.HandleFunc("GET "+PathCreations+"/{cid}", s.creationStatus)
	mux.HandleFunc("GET "+PathAccounts, s.accounts)
	mux.HandleFunc("GET "+PathQueue, s.queueRoute)
	mux.HandleFunc("GET "+PathQueueWatch, s.queueWatch)
	mux.HandleFunc("POST "+PathQueue+"/{id}/answer", s.answerRoute)
	mux.HandleFunc("GET "+PathSummary, s.summaryRoute)
	mux.HandleFunc("POST "+PathItems+"/{id}/state", s.itemStateRoute)
	s.registerWakeRoutes(mux)
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
		if !s.originAllowed(origin) {
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

// originAllowed reports whether origin is an exact allowed browser origin:
// configured, or supplied by ExtraOrigins (canonical values only). No
// credentials are ever allowed: the API authenticates with bearer tokens.
func (s *Server) originAllowed(origin string) bool {
	if s.origins[origin] {
		return true
	}
	if s.cfg.ExtraOrigins == nil {
		return false
	}
	for _, o := range s.cfg.ExtraOrigins() {
		if o == origin {
			if c, err := CanonicalOrigin(o, true); err == nil && c == o {
				return true
			}
		}
	}
	return false
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
		if code == CodeDenied && w.Header().Get("WWW-Authenticate") == "" {
			w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_scope", scope="steer"`)
		}
	}
	writeJSON(w, status, APIError{Code: code, Error: msg})
}

// writeDenied is a 403 denied naming the permission the operation needs.
func writeDenied(w http.ResponseWriter, perm, msg string) {
	w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_scope", scope="`+perm+`"`)
	writeErr(w, http.StatusForbidden, CodeDenied, msg)
}

func writeOAuth(w http.ResponseWriter, status int, code, desc string) {
	writeJSON(w, status, OAuthError{Error: code, Description: desc})
}

// exchangeNote collects how one enrollment exchange (claim, or a token
// request redeeming an enrollment code) ended, for the attempt record and the
// server log. It holds no code, verifier, challenge, label or token.
type exchangeNote struct {
	step, kind  string
	ok          bool
	err, reason string
}

// refuse writes an OAuth refusal and notes it. reason is set for an
// invalid_grant (OAuthError.Reason) and noted as ReasonOther otherwise.
func (n *exchangeNote) refuse(w http.ResponseWriter, status int, code, desc, reason string) {
	writeJSON(w, status, OAuthError{Error: code, Description: desc, Reason: reason})
	n.err, n.reason = code, reason
	if n.reason == "" {
		n.reason = ReasonOther
	}
}

func (s *Server) noteExchange(n *exchangeNote) {
	if n.step == "" {
		return // not an enrollment exchange (a refresh, or no grant type)
	}
	at := s.cfg.Authority.noteEnroll(EnrollAttempt{Step: n.step, ClientKind: n.kind, OK: n.ok, Error: n.err, Reason: n.reason})
	s.cfg.Logf("direct: enrollment %s", at)
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
	n := &exchangeNote{}
	defer s.noteExchange(n)
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
		n.step, n.kind = EnrollStepRedeem, clientKindClass(vals.Get("client_kind"))
		code := vals.Get("code")
		if code == "" {
			n.refuse(w, http.StatusBadRequest, OAuthInvalidRequest, "code is required", "")
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
		n.refuse(w, http.StatusBadRequest, OAuthInvalidRequest, reqErr.Error(), "")
	case errors.Is(err, errInvalidCode):
		s.recordFailure(src)
		reason := ""
		if n.step != "" {
			reason = codeReason(err) // enrollment codes only; refresh refusals stay unclassified
		}
		n.refuse(w, http.StatusBadRequest, OAuthInvalidGrant, "the code or refresh token is invalid, expired, used or revoked", reason)
	case err != nil && strings.Contains(err.Error(), "client_kind"), err != nil && strings.Contains(err.Error(), "label"):
		n.refuse(w, http.StatusBadRequest, OAuthInvalidRequest, err.Error(), "")
	case err != nil:
		// The store write failed. Whether the presented code or refresh token
		// was consumed is unknown (a write can fail after its rename). Clients
		// must not assume the old token is still usable; see docs/direct-access.md.
		s.cfg.Logf("direct: token endpoint: %v", err)
		n.refuse(w, http.StatusServiceUnavailable, OAuthTemporarilyUnavail,
			"the grant store did not confirm the write; no token was returned, and the presented code or refresh token may already be consumed", "")
	default:
		n.ok = true
		w.Header().Set("Pragma", "no-cache")
		writeJSON(w, http.StatusOK, resp)
	}
}

// claim binds a device ticket to the redeeming device's S256 challenge
// (Authority.ClaimTicket). Like the token endpoint it is unauthenticated (the
// ticket is the authority), form-encoded, never takes URL parameters, and
// counts failed attempts toward the same per-source limit.
func (s *Server) claim(w http.ResponseWriter, r *http.Request) {
	src := source(r)
	n := &exchangeNote{step: EnrollStepClaim}
	defer s.noteExchange(n)
	if s.limited(src) {
		w.Header().Set("Retry-After", "60")
		n.refuse(w, http.StatusTooManyRequests, OAuthTemporarilyUnavail, "too many failed attempts; retry later", "")
		return
	}
	vals, err := parseForm(w, r)
	if err != nil {
		n.refuse(w, http.StatusBadRequest, OAuthInvalidRequest, err.Error(), "")
		return
	}
	exp, err := s.cfg.Authority.ClaimTicket(vals.Get("code"), vals.Get("code_challenge"), vals.Get("code_challenge_method"))
	var reqErr *requestError
	switch {
	case errors.As(err, &reqErr):
		n.refuse(w, http.StatusBadRequest, OAuthInvalidRequest, reqErr.Error(), "")
	case errors.Is(err, errInvalidCode):
		s.recordFailure(src)
		n.refuse(w, http.StatusBadRequest, OAuthInvalidGrant, "the ticket is invalid, expired, used or claimed by another device", codeReason(err))
	case err != nil:
		s.cfg.Logf("direct: claim endpoint: %v", err)
		n.refuse(w, http.StatusServiceUnavailable, OAuthTemporarilyUnavail,
			"the grant store did not confirm the claim; retry with the same code_challenge", "")
	default:
		n.ok = true
		w.Header().Set("Pragma", "no-cache")
		writeJSON(w, http.StatusOK, ClaimResponse{Claimed: true, ExpiresAt: exp})
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
	if errors.Is(err, errDenied) {
		writeDenied(w, perm, "this client may not perform that operation")
		return nil
	}
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
	writeJSON(w, http.StatusOK, SessionsResponse{ServerID: s.cfg.Authority.ServerID(), Sessions: s.withIncarnations(l, filteredWith(l.Grant, rows, includeOverseer(r)))})
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
	if _, _, err := authorizeTarget(l, s.cfg.Backend, sid); err != nil {
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
	if _, _, err := authorizeTarget(l, s.cfg.Backend, sid); err != nil {
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
				if _, _, err := authorizeTarget(l, s.cfg.Backend, sid); err != nil {
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

// requiredPerm is the permission a request body's verb needs, read leniently
// before admission so the permission check still precedes any field
// validation. An unparseable body or unknown verb needs steer, as in S1, so a
// grant without steer is refused before it learns anything about the verb.
func requiredPerm(body []byte, fallback string) string {
	var peek struct {
		Verb string `json:"verb"`
	}
	if json.Unmarshal(body, &peek) == nil {
		if p, ok := VerbPermissions[peek.Verb]; ok {
			return p
		}
	}
	return fallback
}

// readAction reads and strictly decodes an action-shaped body after the lease
// was admitted. It reports whether it already answered.
func readAction(w http.ResponseWriter, r *http.Request, body []byte, req *ActionRequest) bool {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "content type must be application/json")
		return true
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(req); err != nil || dec.More() {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "invalid action request")
		return true
	}
	if req.IdemKey == "" || len(req.IdemKey) > MaxIdemKeyLen || strings.ContainsAny(req.IdemKey, "\x00\r\n") {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "idem_key is required (1-128 bytes)")
		return true
	}
	return false
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
	perm := requiredPerm(body, PermSteer)
	l := s.admit(w, r, perm)
	if l == nil {
		return
	}
	defer l.Done()
	var req ActionRequest
	if readAction(w, r, body, &req) {
		return
	}
	// The strict decode must name a verb whose permission is the one admitted.
	// (Both decodes follow the same JSON rules; this makes the binding explicit.)
	if want, ok := VerbPermissions[req.Verb]; ok && want != perm || !ok && perm != PermSteer {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "invalid action request")
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
	// The fresh destination check: scope (the console is never in scope),
	// archive state, the row's own capability and, for add-agent, the machine
	// catalog and the workgroup's current repo ceiling.
	if req.Async && req.Verb != VerbAddAgent {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "async applies only to creation verbs")
		return
	}
	precheck := func(l *Lease) (admission, error) {
		rows, err := targetSnapshot(l, s.cfg.Backend, sid)
		if err != nil {
			return admission{}, err
		}
		byID := indexRows(rows)
		row, ok := byID[sid]
		if !ok || !inScope(l.Grant, row, byID) {
			return admission{}, errNotFound
		}
		var audit *AuditRecord
		if isOverseer(row) {
			// The overseer takes prompt, interject and stop, and only from a
			// grant that already holds everything it could do itself.
			if !overseerVerbAllowed(l.Grant, req.Verb) {
				if req.Verb == VerbPrompt || req.Verb == VerbInterject || req.Verb == VerbStop {
					return admission{}, errOverseerDenied
				}
				return admission{}, errUnsupported
			}
			audit = &AuditRecord{Route: "overseer.steer", Verb: req.Verb, Target: sid, Text: req.Fields["text"]}
		}
		if err := admissible(row, byID, req.Verb); err != nil {
			return admission{}, err
		}
		if audit != nil {
			return admission{audit: audit}, nil
		}
		if req.Verb == VerbAddAgent {
			if err := s.checkCatalog(l, act, sid); err != nil {
				return admission{}, err
			}
			// Negotiate the identity protocol and pin the member to the root
			// incarnation admitted here (creation_identity.go).
			inc, err := s.rootIdentity(l, sid)
			return admission{rootInc: inc}, err
		}
		return admission{affected: archiveAffected(row, rows, byID, req.Verb)}, nil
	}
	s.runAction(w, l, req, actionHash(sid, req), act, precheck)
}

// createWorkgroup is POST /direct/v1/workgroups: new-workgroup or
// new-workgroup-empty. Only an all_sessions grant holding create may create a
// root; a workgroup-scoped grant is refused before any backend call.
func (s *Server) createWorkgroup(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxJSONBody))
	if err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "body too large")
		return
	}
	l := s.admit(w, r, PermCreate)
	if l == nil {
		return
	}
	defer l.Done()
	if !mayCreateRoots(l.Grant) {
		writeDenied(w, PermCreate, "a workgroup-scoped client cannot create new workgroups")
		return
	}
	var req CreateRequest
	if readAction(w, r, body, &req) {
		return
	}
	act, err := createActionFor(req)
	if errors.Is(err, errUnsupported) {
		writeErr(w, http.StatusConflict, CodeUnsupportedVerb, "this verb is not available on direct connections")
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "invalid fields for "+req.Verb)
		return
	}
	s.runAction(w, l, req, actionHash("", req), act, func(l *Lease) (admission, error) {
		if err := s.checkCatalog(l, act, ""); err != nil {
			return admission{}, err
		}
		// An older daemon is refused here, before the key is recorded.
		return admission{}, s.cfg.Backend.CreationIdentity(l.Context())
	})
}

// checkCatalog validates a creation action against the machine's current
// catalog: every harness it will launch must be verified here, and every repo
// must be tracked (new-workgroup*) or inside the target workgroup's current
// grant ceiling (add-agent). The daemon enforces the ceiling again itself.
func (s *Server) checkCatalog(l *Lease, act core.Action, root string) error {
	cat, err := s.cfg.Backend.Catalog(l.Context())
	if err != nil {
		return err
	}
	for _, kind := range requestedHarnesses(act) {
		if !slices.ContainsFunc(cat.Harnesses, func(h CatalogHarness) bool { return h.Name == kind }) {
			return errNotInCatalog
		}
	}
	allowed := cat.Repos
	if root != "" {
		ceiling, ok := cat.Ceilings[root]
		if !ok {
			return errNotInCatalog
		}
		allowed = ceiling
	}
	for _, repo := range amuxstore.SplitRepos(act.Fields["repos"]) {
		if !slices.Contains(allowed, repo) {
			return errNotInCatalog
		}
	}
	return nil
}

// runAction is the shared idempotent execution path: peek the key, run the
// fresh destination precheck, durably record the key, re-check the lease, and
// only then hand the action to the daemon under the bounded execution context.
// With req.Async (creation verbs only) it runs as a background job instead
// (runCreationAsync); the synchronous path is unchanged.
func (s *Server) runAction(w http.ResponseWriter, l *Lease, req ActionRequest, bodyHash string, act core.Action, precheck func(*Lease) (admission, error)) {
	if req.Async {
		s.runCreationAsync(w, l, req, bodyHash, act, precheck)
		return
	}
	if out, rec, err := s.cfg.Authority.BeginAction(l, req.IdemKey, bodyHash, true); err != nil {
		writeAuthErr(w, err)
		return
	} else if s.idemReply(w, l, out, rec) {
		return
	}
	adm, err := precheck(l)
	if err != nil {
		s.writePrecheckErr(w, l, req.Verb, err)
		return
	}
	if adm.audit != nil {
		adm.audit.ClientID, adm.audit.Label = l.ClientID, s.cfg.Authority.ClientLabel(l.ClientID)
		if err := s.cfg.Authority.Audit(adm.audit.with("admitted")); err != nil {
			s.cfg.Logf("direct: audit: %v", err)
			writeErr(w, http.StatusServiceUnavailable, CodeUnavailable, "the audit log could not be written; nothing ran")
			return
		}
	}
	if code := l.AuthEnded(); code != "" { // expiry before execution still denies
		writeErr(w, http.StatusUnauthorized, code, "access ended before the action ran")
		return
	}
	out, rec, err := s.cfg.Authority.BeginAction(l, req.IdemKey, bodyHash, false)
	if s.writeBeginErr(w, err) {
		return
	}
	if s.idemReply(w, l, out, rec) {
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
	var ident amuxstore.Identity
	if core.DescriptorFor(act.Action).CreatesSession {
		ident, err = s.applyCreation(exec, act, adm)
	} else {
		ident.ID, err = s.cfg.Backend.Apply(exec, act)
	}
	cancel()
	status, body := s.settle(l, req.IdemKey, req.Verb, act, adm.affected, ident, err)
	if adm.audit != nil {
		outcome := "ok"
		if e, isErr := body.(APIError); isErr {
			outcome = e.Code
		}
		if err := s.cfg.Authority.Audit(adm.audit.with(outcome)); err != nil {
			s.cfg.Logf("direct: audit: %v", err)
		}
	}
	writeJSON(w, status, body)
}

// writeBeginErr answers a failure to record an action's key; it reports
// whether it did. Nothing ran.
func (s *Server) writeBeginErr(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, errAuthRevoked):
		writeAuthErr(w, err)
	case errors.Is(err, errCapacity):
		// Not a temporary throttle: retrying will not help. The client must
		// enroll again (a new grant starts with an empty record set).
		writeErr(w, http.StatusConflict, CodeCapacity, err.Error())
	default:
		writeErr(w, http.StatusServiceUnavailable, CodeUnavailable, "could not record the action; it was not executed")
	}
	return true
}

// precheckCode maps a precheck refusal to its wire code, status and text.
func precheckCode(l *Lease, verb string, err error) (int, string, string) {
	switch {
	case errors.Is(err, errOverseerDenied):
		return http.StatusForbidden, CodeDenied, "steering the overseer needs an all-sessions client with steer, manage and create"
	case errors.Is(err, errArchived):
		return http.StatusConflict, CodeArchived, "the session or its workgroup is archived (or, for unarchive, not archived)"
	case errors.Is(err, errUnsupported):
		return http.StatusConflict, CodeUnsupportedVerb, "this session cannot serve " + verb
	case errors.Is(err, errNotInCatalog):
		return http.StatusUnprocessableEntity, CodeNotInCatalog, "a harness or repository is not in this machine's creation catalog for this target"
	case l.Revoked():
		return http.StatusUnauthorized, CodeAuthRevoked, "access revoked"
	case l.Expired() || errors.Is(context.Cause(l.Context()), errGrantEnded):
		return http.StatusUnauthorized, CodeAuthExpired, "access expired"
	case errors.Is(err, errNotFound):
		return http.StatusNotFound, CodeNotFound, "no such session"
	case errors.Is(err, hostbackend.ErrUpgradeRequired):
		return http.StatusServiceUnavailable, CodeDaemonUpgradeRequired, "the amux daemon must be upgraded (and restarted) before it can create sessions over direct access"
	case errors.Is(err, hostbackend.ErrUnavailable):
		return http.StatusServiceUnavailable, CodeUnavailable, "the amux daemon is not reachable"
	}
	return http.StatusServiceUnavailable, CodeUnavailable, "the amux daemon could not answer"
}

func (s *Server) writePrecheckErr(w http.ResponseWriter, l *Lease, verb string, err error) {
	if errors.Is(err, errOverseerDenied) {
		w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_scope", scope="steer manage create"`)
	}
	status, code, msg := precheckCode(l, verb, err)
	if code == CodeUnavailable {
		s.cfg.Logf("direct: backend: %v", err)
	}
	writeErr(w, status, code, msg)
}

// settle classifies one finished daemon call, records it under key and
// returns the synchronous answer.
//
// A creation's ident is what the daemon created (with incarnations); any other
// verb's carries nothing. A creation is recorded as a success only with a
// complete identity of the expected shape; it is the identity the daemon
// returned, never one read back afterwards.
func (s *Server) settle(l *Lease, key, verb string, act core.Action, affected []string, ident amuxstore.Identity, err error) (int, any) {
	creates := core.DescriptorFor(act.Action).CreatesSession
	newID := ident.ID
	wantRoot := newID
	if act.Action == core.ActionAddAgent {
		wantRoot = act.ID
	}
	var rejected *daemon.RejectedError
	switch {
	case err == nil && creates && validSessionID(newID) && ident.Complete() && ident.RootID == wantRoot &&
		(newID != wantRoot || ident.Incarnation == ident.RootIncarnation):
		res := ActionResponse{OK: true, NewID: newID, RootID: wantRoot}
		s.cfg.Authority.FinishCreated(l, key, res, ident)
		return http.StatusOK, res
	case errors.Is(err, hostbackend.ErrUpgradeRequired):
		// Negotiated on the dispatch connection before the action was sent:
		// nothing ran. Reached only if the daemon changed since the precheck.
		s.cfg.Authority.FinishAction(l, key, false, CodeDaemonUpgradeRequired, "the amux daemon must be upgraded before it can create sessions over direct access; nothing was sent to it")
		return http.StatusServiceUnavailable, APIError{Code: CodeDaemonUpgradeRequired, Error: "the amux daemon must be upgraded before it can create sessions over direct access; nothing was sent to it"}
	case err == nil && creates:
		// The daemon reported success without a usable id. Something may exist,
		// so the key must never create again: record it as ambiguous.
		s.cfg.Authority.AmbiguousAction(l, key)
		return http.StatusConflict, APIError{Code: CodeIdemAmbiguous, Error: "the creation's outcome is unknown; it will not run again with this idem_key"}
	case err == nil:
		res := ActionResponse{OK: true, Affected: affected}
		s.cfg.Authority.FinishResult(l, key, res)
		return http.StatusOK, res
	case errors.As(err, &rejected):
		// The daemon answered not-OK. That is a known answer, not proof of no
		// side effect; it is recorded and replayed, never re-run.
		s.cfg.Logf("direct: action %s on %q rejected: %v", verb, act.ID, err)
		s.cfg.Authority.FinishAction(l, key, false, CodeRejected, "the daemon rejected the action")
		return http.StatusUnprocessableEntity, APIError{Code: CodeRejected, Error: "the daemon rejected the action"}
	case errors.Is(err, hostbackend.ErrUnavailable):
		// The call never reached an authenticated daemon (the dial failed
		// before anything was sent): nothing ran.
		s.cfg.Authority.FinishAction(l, key, false, CodeUnavailable, "the amux daemon was not reachable; the action did not run")
		return http.StatusServiceUnavailable, APIError{Code: CodeUnavailable, Error: "the amux daemon was not reachable; the action did not run"}
	default:
		// Cancelled (revocation, grant expiry or the execution bound) or a
		// transport failure after dispatch. Closing the socket does not stop
		// the daemon: it runs an accepted action to completion under its own
		// lifetime, so the creation may well finish. The outcome is unknown.
		s.cfg.Authority.AmbiguousAction(l, key)
		if l.Revoked() {
			return http.StatusUnauthorized, APIError{Code: CodeAuthRevoked, Error: "access revoked during the action; its outcome is unknown"}
		}
		return http.StatusConflict, APIError{Code: CodeIdemAmbiguous, Error: "the action's outcome is unknown; it will not run again with this idem_key"}
	}
}

// capabilities is GET /direct/v1/capabilities. ServerVerbs is everything this
// server implements; Verbs is the subset this grant's permissions admit. A
// client tells "server too old" (the route answers 404 without a JSON
// {"code"} body, or a verb is absent from server_verbs) from "this grant may
// not" (the verb is in server_verbs but not verbs).
func (s *Server) capabilities(w http.ResponseWriter, r *http.Request) {
	l := s.admit(w, r, PermRead)
	if l == nil {
		return
	}
	defer l.Done()
	resp := CapabilitiesResponse{
		ServerID: s.cfg.Authority.ServerID(), Permissions: slices.Clone(l.Perms), ResourceScope: l.Resource,
		ServerVerbs: ServerVerbs(), Verbs: sessionVerbs(l.Grant), CreateWorkgroup: mayCreateRoots(l.Grant),
		AsyncCreate: l.Can(PermCreate),
	}
	accountStatus := mayReadAccounts(l.Grant)
	resp.AccountStatus = &accountStatus
	resp.Wake = &WakeCapabilities{V: 1}
	resp.Enrichments = enrichmentCaps(l.Context(), s.cfg.Backend)
	if _, ok := s.queueBackend(); ok {
		resp.Queue, resp.Overseer = queueCaps(l.Grant)
	}
	if s.cfg.Wake != nil {
		st := s.cfg.Wake.Status()
		resp.Wake.Enabled, resp.Wake.Degraded = st.Enabled, st.Degraded
	}
	if l.Can(PermCreate) {
		// A daemon without creation identities cannot create over direct
		// access: say so up front rather than at the first creation.
		switch err := s.cfg.Backend.CreationIdentity(l.Context()); {
		case errors.Is(err, hostbackend.ErrUpgradeRequired):
			resp.CreateWorkgroup, resp.AsyncCreate, resp.DaemonUpgradeRequired = false, false, true
		case err != nil:
			s.writeBackendErr(w, l, err)
			return
		}
		rows, err := s.cfg.Backend.Snapshot(l.Context())
		if err != nil {
			s.writeBackendErr(w, l, err)
			return
		}
		cat, err := s.cfg.Backend.Catalog(l.Context())
		if err != nil {
			s.writeBackendErr(w, l, err)
			return
		}
		resp.Catalog = projectCatalog(l.Grant, cat, rows)
	}
	if code := l.AuthEnded(); code != "" {
		writeErr(w, http.StatusUnauthorized, code, "access ended")
		return
	}
	writeJSON(w, http.StatusOK, resp)
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

// idemExpiredMsg is the fixed idem_expired text. It never says whether the
// action ran: only that it will not run again.
const idemExpiredMsg = "this idem_key was already used and its recorded result has expired; it will not run again. Check the machine for its effect before retrying with a new idem_key"

// idemReply answers a non-proceed idempotency outcome; it reports whether it did.
func (s *Server) idemReply(w http.ResponseWriter, l *Lease, out idemOutcome, rec *idemRecord) bool {
	switch out {
	case idemReplay:
		if isCreationSuccess(rec) {
			s.replayCreated(w, l, rec)
		} else if rec.OK {
			writeJSON(w, http.StatusOK, ActionResponse{OK: true, NewID: rec.NewID, RootID: rec.RootID, Affected: rec.Affected})
		} else {
			status := http.StatusUnprocessableEntity
			if rec.Code == CodeUnavailable || rec.Code == CodeDaemonUpgradeRequired {
				status = http.StatusServiceUnavailable
			} else if rec.Code == CodeAskerGone || rec.Code == CodeAnswered {
				status = http.StatusConflict
			}
			writeErr(w, status, rec.Code, rec.Error)
		}
	case idemConflict:
		writeErr(w, http.StatusUnprocessableEntity, CodeIdemConflict, "idem_key was used with a different request")
	case idemInProgress:
		writeErr(w, http.StatusConflict, CodeIdemInProgress, "an action with this idem_key is still running")
	case idemAmbiguous:
		writeErr(w, http.StatusConflict, CodeIdemAmbiguous, "an action with this idem_key has an unknown outcome; it will not run again")
	case idemExpired:
		writeErr(w, http.StatusConflict, CodeIdemExpired, idemExpiredMsg)
	default:
		return false
	}
	return true
}

// ── watch (WebSocket) ──────────────────────────────────────────────────────

// watch is GET /direct/v1/sessions/watch: the grant's inventory, pushed on
// change (serveWatch). ?include=overseer adds the overseer row, as on GET
// /sessions.
func (s *Server) watch(w http.ResponseWriter, r *http.Request) {
	overseer := includeOverseer(r)
	s.serveWatch(w, r, func(ctx context.Context, l *Lease) (any, error) {
		rows, err := s.cfg.Backend.Snapshot(ctx)
		if err != nil {
			return nil, err
		}
		return WatchSnapshot{Type: "snapshot", Sessions: s.withIncarnations(l, filteredWith(l.Grant, rows, overseer))}, nil
	})
}
