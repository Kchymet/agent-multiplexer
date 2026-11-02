package provider

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"amux/internal/wiretls"
	"github.com/kchymet/agent-multiplexer/harnessproto"
)

const revokeTestToken = "synthetic-revoke-secret"
const revokeSuccess = `{"type":"provider-revoked","ok":true,"version":2,"providerId":"prov-test"}` + "\n"

func TestRevokeAcknowledgment(t *testing.T) {
	for _, tc := range []struct {
		name, response string
		ok             bool
	}{
		{"success", revokeSuccess, true},
		{"old broker", `{"type":"registered","ok":true,"version":2,"providerId":"prov-test"}` + "\n", false},
		{"EOF", "", false},
		{"malformed", "{\"" + revokeTestToken + "\n", false},
		{"oversized", strings.Repeat("x", 32<<10), false},
		{"wrong version", `{"type":"provider-revoked","ok":true,"version":1,"providerId":"p"}` + "\n", false},
		{"missing identity", `{"type":"provider-revoked","ok":true,"version":2}` + "\n", false},
		{"contradictory success", `{"type":"provider-revoked","ok":true,"version":2,"providerId":"p","error":"internal"}` + "\n", false},
		{"missing ok", `{"type":"provider-revoked","version":2,"providerId":"p"}` + "\n", false},
		{"secret in error", `{"type":"provider-revoked","error":"` + revokeTestToken + `"}` + "\n", false},
		{"secret in type", `{"type":"` + revokeTestToken + `"}` + "\n", false},
		{"bad token", `{"type":"provider-revoked","error":"bad-token"}` + "\n", false},
		{"unsupported", `{"type":"provider-revoked","error":"unsupported-version"}` + "\n", false},
		{"internal", `{"type":"provider-revoked","error":"internal"}` + "\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := net.Pipe()
			done := make(chan struct{})
			go func() {
				defer close(done)
				defer b.Close()
				m, err := harnessproto.NewConn(b).ReadHarness()
				if err != nil {
					t.Error(err)
					return
				}
				if m.Type != harnessproto.HProviderRevoke || m.Token != revokeTestToken || len(m.Versions) != 1 || m.Versions[0] != 2 || m.Capabilities != nil || len(m.Panes) != 0 || m.Name != "" {
					t.Error("revocation must send only its authentication/version first frame")
				}
				_, _ = io.WriteString(b, tc.response)
			}()
			err := Revoke(context.Background(), Config{Token: revokeTestToken, AllowCompute: true, Name: "must-not-publish", Dial: func(context.Context) (net.Conn, error) { return a, nil }})
			<-done
			if (err == nil) != tc.ok {
				t.Fatalf("error = %v, want success %v", err, tc.ok)
			}
			if err != nil && strings.Contains(err.Error(), revokeTestToken) {
				t.Fatal("credential disclosed in error")
			}
		})
	}
}

func TestRevokeCancellation(t *testing.T) {
	for _, phase := range []string{"dial", "write", "read"} {
		for _, deadline := range []bool{false, true} {
			t.Run(phase+"/"+map[bool]string{true: "deadline", false: "cancel"}[deadline], func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				want := context.Canceled
				if deadline {
					cancel()
					ctx, cancel = context.WithTimeout(context.Background(), 30*time.Millisecond)
					want = context.DeadlineExceeded
				}
				defer cancel()
				a, b := net.Pipe()
				defer a.Close()
				defer b.Close()
				entered := make(chan struct{})
				dial := func(ctx context.Context) (net.Conn, error) {
					if d, ok := ctx.Deadline(); !ok || time.Until(d) > revokeTimeout {
						t.Error("exchange is missing its bounded deadline")
					}
					if phase != "read" {
						close(entered)
					}
					if phase == "dial" {
						<-ctx.Done()
						return nil, ctx.Err()
					}
					return a, nil
				}
				if phase == "read" {
					go func() { _, _ = harnessproto.NewConn(b).ReadHarness(); close(entered) }()
				}
				done := make(chan error, 1)
				go func() { done <- Revoke(ctx, Config{Token: revokeTestToken, Dial: dial}) }()
				<-entered
				if !deadline {
					cancel()
				}
				select {
				case err := <-done:
					if !errors.Is(err, want) {
						t.Fatalf("error = %v, want %v", err, want)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("cancellation did not unblock exchange")
				}
			})
		}
	}
}

func TestRevokeRetryAfterLostAcknowledgment(t *testing.T) {
	// Script a lost acknowledgment followed by idempotent success. The client
	// must retry without first registering or replacing the original token.
	for attempt := range 3 {
		a, b := net.Pipe()
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer b.Close()
			m, err := harnessproto.NewConn(b).ReadHarness()
			if err != nil || m.Type != harnessproto.HProviderRevoke || m.Token != revokeTestToken {
				t.Error("retry changed credential or registered before revoking")
				return
			}
			if attempt > 0 {
				_, _ = io.WriteString(b, revokeSuccess)
			}
		}()
		err := Revoke(context.Background(), Config{Token: revokeTestToken, Dial: func(context.Context) (net.Conn, error) { return a, nil }})
		<-done
		if (err == nil) != (attempt > 0) {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
	}
}

func TestRevokeTLSIdentityBeforeToken(t *testing.T) {
	cert, key := genCert(t, t.TempDir())
	srv, err := wiretls.ServerConfig(cert, key, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, ca, server string
		ok               bool
	}{
		{"trusted", cert, "localhost", true},
		{"wrong name", cert, "wrong.example", false},
		{"untrusted", "", "localhost", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ln, err := tls.Listen("tcp", "127.0.0.1:0", srv)
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			done := make(chan struct{})
			go func() {
				defer close(done)
				conn, err := ln.Accept()
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
				m, err := harnessproto.NewConn(conn).ReadHarness()
				if !tc.ok {
					if err == nil || m.Token != "" {
						t.Error("unverified server received a token")
					}
					return
				}
				if err != nil || m.Token != revokeTestToken {
					t.Error("verified TLS exchange failed")
					return
				}
				_, _ = io.WriteString(conn, revokeSuccess)
			}()
			err = Revoke(context.Background(), Config{Token: revokeTestToken, Orchestrator: ln.Addr().String(), CAFile: tc.ca, ServerName: tc.server})
			<-done
			if (err == nil) != tc.ok {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestRevokeEmptyTokenDoesNotDial(t *testing.T) {
	err := Revoke(context.Background(), Config{Token: " \n", Dial: func(context.Context) (net.Conn, error) { t.Fatal("empty token dialed"); return nil, nil }})
	if err == nil {
		t.Fatal("empty token accepted")
	}
}
