package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"amux/internal/direct"
)

func TestWriteDirectConnections(t *testing.T) {
	var b bytes.Buffer
	writeDirectConnections(&b, nil) // an older server: nothing printed
	if b.Len() != 0 {
		t.Fatalf("printed for an older server: %q", b.String())
	}
	now := time.Now()
	writeDirectConnections(&b, &direct.ConnStats{
		Since: now.Unix(), TCPAccepted: 9, TLSOK: 7, TLSFailed: 2, Requests: 12, Authenticated: 8, StreamsOpened: 3, StreamsActive: 1,
		Failures:   map[string]int64{direct.FailTLSMismatch: 2, direct.FailAuthInvalid: 1},
		StreamEnds: map[string]int64{direct.FailClientClosed: 1, direct.FailAuthExpired: 1},
		Refresh:    direct.RefreshStats{OK: 4, GraceRetry: 1},
		Listen:     direct.ListenStats{Addr: "192.168.1.20:8443", Present: false, ChangedAt: now.Unix()},
		Recent: []direct.ConnAttempt{{AtMS: now.UnixMilli(), Phase: direct.PhaseTCP, Reason: direct.FailTLSMismatch,
			Remote: "192.168.1.40:5555", Detail: "remote error: tls: bad certificate"}},
	})
	out := b.String()
	for _, want := range []string{
		"tls ok 7 failed 2", "failures: tls_mismatch 2, auth_invalid 1",
		"stream ends: auth_expired 1, client_closed 1",
		"listen address 192.168.1.20:8443 is not assigned",
		"grace retries 1",
		`phase=tcp ok=false reason=tls_mismatch remote=192.168.1.40:5555 dur_ms=0 detail="remote error: tls: bad certificate"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
