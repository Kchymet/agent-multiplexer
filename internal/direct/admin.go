package direct

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"amux/internal/access"
)

// Local administration. The channel is a Unix socket inside the 0700 direct
// root, which agent sandboxes never mount. Reaching the socket is not
// authority: before any request is read, the caller must prove the current
// SubjectHost credential through the daemon's unchanged challenge/proof (the
// HostVerifier). Session, provider and direct-client credentials are refused,
// and a refused caller causes no registry write.

// ProveFunc signs the daemon's challenge with the caller's credential.
type ProveFunc func(access.SocketChallenge) (access.SocketProof, error)

// HostVerifier authenticates a relayed proof as the current host credential.
// Production uses daemon.RelayHostProof.
type HostVerifier func(ctx context.Context, prove ProveFunc) error

// Admin request operations.
const (
	OpEnroll        = "enroll"
	OpList          = "list"
	OpRevoke        = "revoke"
	OpResetIdentity = "reset_identity"
)

const adminDeadline = 20 * time.Second

type adminFrame struct {
	Type      string                  `json:"type"` // challenge | proof | authenticated | request | response
	Challenge *access.SocketChallenge `json:"challenge,omitempty"`
	Proof     *access.SocketProof     `json:"proof,omitempty"`
	Request   *AdminRequest           `json:"request,omitempty"`
	Response  *AdminResponse          `json:"response,omitempty"`
	Error     string                  `json:"error,omitempty"`
}

// AdminRequest is one administration call.
type AdminRequest struct {
	Op          string        `json:"op"`
	Label       string        `json:"label,omitempty"`
	Permissions []string      `json:"permissions,omitempty"`
	Resource    ResourceScope `json:"resource,omitempty"`
	TTLSeconds  int64         `json:"ttl_seconds,omitempty"`
	ClientID    string        `json:"client_id,omitempty"`
}

// AdminResponse answers it.
type AdminResponse struct {
	OK        bool         `json:"ok"`
	Error     string       `json:"error,omitempty"`
	Bundle    string       `json:"bundle,omitempty"`
	ExpiresAt int64        `json:"expires_at,omitempty"`
	ServerID  string       `json:"server_id,omitempty"`
	Endpoints []string     `json:"endpoints,omitempty"`
	Clients   []ClientInfo `json:"clients,omitempty"`
}

// EncodeBundle renders a paste bundle.
func EncodeBundle(b Bundle) (string, error) {
	data, err := json.Marshal(b)
	if err != nil {
		return "", err
	}
	return BundlePrefix + base64.RawURLEncoding.EncodeToString(data), nil
}

// DecodeBundle parses and validates a paste bundle (the reference for clients).
func DecodeBundle(s string, now time.Time) (Bundle, error) {
	if len(s) < len(BundlePrefix) || s[:len(BundlePrefix)] != BundlePrefix {
		return Bundle{}, errors.New("not an amux-direct:1 bundle")
	}
	data, err := base64.RawURLEncoding.DecodeString(s[len(BundlePrefix):])
	if err != nil {
		return Bundle{}, errors.New("bundle is not base64url")
	}
	var b Bundle
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		return Bundle{}, fmt.Errorf("bundle: %w", err)
	}
	switch {
	case b.V != 1:
		return Bundle{}, errors.New("bundle: unsupported version")
	case b.ServerID == "" || len(b.Endpoints) == 0:
		return Bundle{}, errors.New("bundle: missing server_id or endpoints")
	case !validNonce(b.Code):
		return Bundle{}, errors.New("bundle: code must be base64url of 32 bytes")
	case now.Unix() >= b.ExpiresAt:
		return Bundle{}, errors.New("bundle: expired")
	}
	for _, e := range b.Endpoints {
		if c, err := CanonicalOrigin(e, false); err != nil || c != e {
			return Bundle{}, fmt.Errorf("bundle: endpoint %q is not a canonical https origin", e)
		}
	}
	return b, nil
}

// AdminServer serves the admin socket for a running direct server.
type AdminServer struct {
	Authority *Authority
	Endpoints []string
	Verify    HostVerifier
	Logf      func(string, ...any)
	ln        net.Listener
}

// AdminSocketPath is the admin socket inside root.
func AdminSocketPath(root string) string { return filepath.Join(root, AdminSocketName) }

// Listen binds the admin socket (0600) inside root. The store lock held by the
// Authority guarantees no other server owns the path.
func (s *AdminServer) Listen(root string) error {
	path := AdminSocketPath(root)
	_ = os.Remove(path)
	old := syscall.Umask(0o177)
	ln, err := net.Listen("unix", path)
	syscall.Umask(old)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return err
	}
	s.ln = ln
	return nil
}

// Serve accepts admin connections until ctx ends.
func (s *AdminServer) Serve(ctx context.Context) error {
	if s.Logf == nil {
		s.Logf = func(string, ...any) {}
	}
	go func() { <-ctx.Done(); s.ln.Close() }()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go s.handle(ctx, conn)
	}
}

func (s *AdminServer) handle(parent context.Context, conn net.Conn) {
	defer conn.Close()
	ctx, cancel := context.WithTimeout(parent, adminDeadline)
	defer cancel()
	_ = conn.SetDeadline(time.Now().Add(adminDeadline))
	reader := bufio.NewReaderSize(conn, access.MaxBodyBytes+1)
	enc := json.NewEncoder(conn)
	read := func() (adminFrame, error) {
		line, err := reader.ReadSlice('\n')
		if err != nil || len(line) > access.MaxBodyBytes {
			return adminFrame{}, errors.New("bad admin frame")
		}
		var f adminFrame
		return f, json.Unmarshal(line, &f)
	}
	err := s.Verify(ctx, func(ch access.SocketChallenge) (access.SocketProof, error) {
		if err := enc.Encode(adminFrame{Type: "challenge", Challenge: &ch}); err != nil {
			return access.SocketProof{}, err
		}
		f, err := read()
		if err != nil || f.Type != "proof" || f.Proof == nil {
			return access.SocketProof{}, errors.New("no proof")
		}
		return *f.Proof, nil
	})
	if err != nil {
		s.Logf("direct: admin caller refused: host authentication failed")
		_ = enc.Encode(adminFrame{Type: "response", Response: &AdminResponse{Error: "host authentication failed"}})
		return
	}
	if err := enc.Encode(adminFrame{Type: "authenticated"}); err != nil {
		return
	}
	f, err := read()
	if err != nil || f.Type != "request" || f.Request == nil {
		return
	}
	resp := Administer(s.Authority, s.Endpoints, *f.Request)
	_ = enc.Encode(adminFrame{Type: "response", Response: &resp})
}

// Administer executes one already host-authenticated request.
func Administer(a *Authority, endpoints []string, req AdminRequest) AdminResponse {
	fail := func(err error) AdminResponse { return AdminResponse{Error: err.Error()} }
	switch req.Op {
	case OpEnroll:
		if len(endpoints) == 0 {
			return fail(errors.New("enrollment needs a running `amux direct serve` with configured endpoints"))
		}
		code, exp, err := a.NewCode(req.Label, req.Permissions, req.Resource, time.Duration(req.TTLSeconds)*time.Second)
		if err != nil {
			return fail(err)
		}
		bundle, err := EncodeBundle(Bundle{V: 1, ServerID: a.ServerID(), Endpoints: endpoints, Code: code, ExpiresAt: exp})
		if err != nil {
			return fail(err)
		}
		return AdminResponse{OK: true, Bundle: bundle, ExpiresAt: exp, ServerID: a.ServerID(), Endpoints: endpoints}
	case OpList:
		return AdminResponse{OK: true, ServerID: a.ServerID(), Clients: a.List()}
	case OpRevoke:
		if err := a.Revoke(req.ClientID, "revoked by host"); err != nil {
			return fail(err)
		}
		return AdminResponse{OK: true}
	case OpResetIdentity:
		id, err := a.ResetIdentity()
		if err != nil {
			return fail(err)
		}
		return AdminResponse{OK: true, ServerID: id}
	}
	return fail(fmt.Errorf("unknown admin operation %q", req.Op))
}

// AdminCall sends one request to a running server's admin socket, proving the
// host credential cred. cred must be the SubjectHost credential from the
// protected (HOME-independent) directory.
func AdminCall(ctx context.Context, root string, cred access.Credential, req AdminRequest) (AdminResponse, error) {
	if cred.Kind != access.SubjectHost {
		return AdminResponse{}, errors.New("administration requires the host credential")
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", AdminSocketPath(root))
	if err != nil {
		return AdminResponse{}, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(adminDeadline))
	reader := bufio.NewReaderSize(conn, access.MaxBodyBytes+1)
	enc := json.NewEncoder(conn)
	read := func() (adminFrame, error) {
		line, err := reader.ReadSlice('\n')
		if err != nil || len(line) > access.MaxBodyBytes {
			return adminFrame{}, errors.New("direct server closed the admin channel")
		}
		var f adminFrame
		return f, json.Unmarshal(line, &f)
	}
	f, err := read()
	if err != nil {
		return AdminResponse{}, err
	}
	if f.Type == "response" && f.Response != nil {
		return *f.Response, errors.New(f.Response.Error)
	}
	if f.Type != "challenge" || f.Challenge == nil {
		return AdminResponse{}, errors.New("unexpected admin frame")
	}
	proof, err := access.SignProof(cred, *f.Challenge, rand.Reader)
	if err != nil {
		return AdminResponse{}, fmt.Errorf("the challenge is not from this host's daemon: %w", err)
	}
	if err := enc.Encode(adminFrame{Type: "proof", Proof: &proof}); err != nil {
		return AdminResponse{}, err
	}
	if f, err = read(); err != nil {
		return AdminResponse{}, err
	}
	if f.Type != "authenticated" {
		if f.Response != nil {
			return *f.Response, errors.New(f.Response.Error)
		}
		return AdminResponse{}, errors.New("host authentication failed")
	}
	if err := enc.Encode(adminFrame{Type: "request", Request: &req}); err != nil {
		return AdminResponse{}, err
	}
	if f, err = read(); err != nil {
		return AdminResponse{}, err
	}
	if f.Response == nil {
		return AdminResponse{}, errors.New("no admin response")
	}
	if !f.Response.OK {
		return *f.Response, errors.New(f.Response.Error)
	}
	return *f.Response, nil
}

// OfflineAdminister runs a request against a stopped server's store. It takes
// the store's exclusive lock (ErrStoreBusy if a server owns it) and requires
// the same host proof, verified by the daemon, before opening the registry.
func OfflineAdminister(ctx context.Context, root string, cred access.Credential, verify HostVerifier, req AdminRequest) (AdminResponse, error) {
	if cred.Kind != access.SubjectHost {
		return AdminResponse{}, errors.New("administration requires the host credential")
	}
	if req.Op == OpEnroll {
		return AdminResponse{}, errors.New("enrollment needs a running `amux direct serve` (it publishes the configured endpoints)")
	}
	if err := verify(ctx, func(ch access.SocketChallenge) (access.SocketProof, error) {
		return access.SignProof(cred, ch, rand.Reader)
	}); err != nil {
		return AdminResponse{}, fmt.Errorf("host authentication failed: %w", err)
	}
	a, err := OpenAuthority(root, Lifetimes{}, nil)
	if err != nil {
		return AdminResponse{}, err
	}
	defer a.Close()
	resp := Administer(a, nil, req)
	if !resp.OK {
		return resp, errors.New(resp.Error)
	}
	return resp, nil
}
