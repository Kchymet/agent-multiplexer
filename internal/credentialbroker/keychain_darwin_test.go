//go:build darwin

package credentialbroker

import (
	"context"
	"os"
	"strings"
	"testing"
)

// Only synthetic items in a unique namespace are modified. Never touch a host
// Claude login, and never include credential values in test output.
func TestNativeKeychainRoundTrip(t *testing.T) {
	if os.Getenv("AMUX_TEST_KEYCHAIN") != "1" {
		t.Skip("set AMUX_TEST_KEYCHAIN=1 for a synthetic native Keychain round trip")
	}
	k := Keychain{Env: os.Environ()}
	o := Operation{Account: HostAccount(), Service: ClaudeServices(t.TempDir())[1]}
	t.Cleanup(func() { o.Verb = Delete; o.Value = ""; _, _ = k.Execute(context.Background(), o) })
	for _, value := range []string{"synthetic-token", strings.Repeat("synthetic-", 600)} {
		o.Verb, o.Value = Write, value
		result, err := k.Execute(context.Background(), o)
		if err != nil || result.ExitCode != 0 {
			t.Fatalf("native write: exit=%d error=%v", result.ExitCode, err)
		}
		o.Verb, o.Value = Read, ""
		result, err = k.Execute(context.Background(), o)
		if err != nil || result.ExitCode != 0 || result.Value != value {
			t.Fatalf("native read mismatch: exit=%d error=%v", result.ExitCode, err)
		}
	}
	o.Verb = Delete
	if result, err := k.Execute(context.Background(), o); err != nil || result.ExitCode != 0 {
		t.Fatalf("native delete: exit=%d error=%v", result.ExitCode, err)
	}
	o.Verb = Read
	if result, err := k.Execute(context.Background(), o); err != nil || result.ExitCode != 44 {
		t.Fatalf("missing item: exit=%d error=%v", result.ExitCode, err)
	}
}
