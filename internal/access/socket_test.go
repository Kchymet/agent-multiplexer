package access

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"net"
	"testing"
)

func TestPinnedTLSAcceptsAuthorityAndRejectsHostileEndpoint(t *testing.T) {
	a, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	dir, err := a.Ensure(context.Background(), SubjectHost, LocalHostSubject)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := LoadCredential(dir)
	if err != nil {
		t.Fatal(err)
	}
	clientConfig, err := ClientTLSConfig(credential)
	if err != nil {
		t.Fatal(err)
	}
	serverConfig, err := a.ServerTLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	if clientErr, serverErr := tlsHandshake(clientConfig, serverConfig); clientErr != nil || serverErr != nil {
		t.Fatalf("pinned handshake failed: client=%v server=%v", clientErr, serverErr)
	}

	hostile, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hostile.Close() })
	hostileConfig, err := hostile.ServerTLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	clientErr, _ := tlsHandshake(clientConfig, hostileConfig)
	if clientErr == nil {
		t.Fatal("client accepted an endpoint without the pinned daemon certificate")
	}
}

func TestSocketProofBindsChallengeAndCurrentGeneration(t *testing.T) {
	a, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	dir, _ := a.Ensure(context.Background(), SubjectHost, LocalHostSubject)
	credential, _ := LoadCredential(dir)
	challenge, _ := a.NewChallenge()
	proof, err := SignProof(credential, challenge, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	principal, err := a.VerifyProof(context.Background(), challenge, proof, SubjectHost)
	if err != nil || principal.SubjectID != LocalHostSubject {
		t.Fatalf("proof principal=%+v err=%v", principal, err)
	}
	other, _ := a.NewChallenge()
	if _, err := a.VerifyProof(context.Background(), other, proof, SubjectHost); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("relayed proof error = %v", err)
	}
	if _, err := a.Rotate(context.Background(), SubjectHost, LocalHostSubject); err != nil {
		t.Fatal(err)
	}
	if _, err := a.VerifyProof(context.Background(), challenge, proof, SubjectHost); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("old-generation proof error = %v", err)
	}
}

func tlsHandshake(clientConfig, serverConfig *tls.Config) (error, error) {
	serverConn, clientConn := net.Pipe()
	server := tls.Server(serverConn, serverConfig)
	client := tls.Client(clientConn, clientConfig)
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Handshake(); _ = server.Close() }()
	clientErr := client.Handshake()
	_ = client.Close()
	return clientErr, <-serverDone
}
