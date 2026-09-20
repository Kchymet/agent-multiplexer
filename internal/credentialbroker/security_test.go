package credentialbroker

import (
	"encoding/hex"
	"slices"
	"strings"
	"testing"
)

func TestSecurityHelperVocabulary(t *testing.T) {
	value := `{"claudeAiOauth":{"accessToken":"synthetic","refreshToken":"synthetic-refresh"}}`
	for _, tc := range []struct {
		name                   string
		args                   []string
		input, verb, wantValue string
	}{
		{"read", []string{"find-generic-password", "-a", "user", "-w", "-s", "Claude Code-credentials"}, "", Read, ""},
		{"delete", []string{"delete-generic-password", "-a", "user", "-s", "Claude Code-credentials"}, "", Delete, ""},
		{"update argv", []string{"add-generic-password", "-U", "-a", "user", "-s", "Claude Code-credentials", "-X", hex.EncodeToString([]byte(value))}, "", Write, value},
		{"update stdin", []string{"-i"}, `add-generic-password -U -a "user" -s "Claude Code-credentials" -X "` + hex.EncodeToString([]byte(value)) + `" `, Write, value},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, err := ParseSecurity(tc.args, strings.NewReader(tc.input))
			if err != nil || o.Verb != tc.verb || o.Value != tc.wantValue || o.Service != "Claude Code-credentials" || o.Account != "user" {
				t.Fatalf("parse failed: %v", err)
			}
		})
	}
	for _, args := range [][]string{
		{"dump-keychain"},
		{"find-generic-password", "-a", "user", "-s", "service", "-w", "/another/keychain"},
		{"find-generic-password", "-a", "user", "-s", "service", "-s", "other", "-w"},
		{"add-generic-password", "-U", "-a", "user", "-s", "service", "-X", "not-hex"},
		{"delete-generic-password", "-a", "user", "-s", "service", "-w", "value"},
	} {
		if _, err := ParseSecurity(args, strings.NewReader("")); err == nil {
			t.Fatal("accepted unsupported operation")
		}
	}
	for _, input := range []string{
		"find-generic-password -a user -s service -w\ndelete-generic-password -a user -s service",
		`add-generic-password -U -a user -s "unclosed -X 61`,
		strings.Repeat("x", 2*MaxValue+2049),
	} {
		if _, err := ParseSecurity([]string{"-i"}, strings.NewReader(input)); err == nil {
			t.Fatal("accepted malformed/multiple commands")
		}
	}
}

func TestClaudeServiceSelection(t *testing.T) {
	if !slices.Equal(ClaudeServices(""), []string{"Claude Code", "Claude Code-credentials"}) {
		t.Fatal("default account changed")
	}
	if slices.Equal(ClaudeServices(""), ClaudeServices("/home/user/.claude")) {
		t.Fatal("explicit selector collapsed to default")
	}
	if slices.Equal(ClaudeServices("/work"), ClaudeServices("/work/")) {
		t.Fatal("selector spelling lost")
	}
	if !slices.Equal(ClaudeServices("/caf\u00e9"), ClaudeServices("/cafe\u0301")) {
		t.Fatal("NFC normalization missing")
	}
}
