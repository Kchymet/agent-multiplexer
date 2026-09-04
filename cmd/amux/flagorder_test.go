package main

import (
	"flag"
	"io"
	"reflect"
	"strings"
	"testing"
)

// TestParseFlagsAnyOrder covers the parse itself: flags are flags wherever they
// sit relative to the operands, "--" still ends flag parsing, and an unknown flag
// is an error rather than a silently ignored word.
func TestParseFlagsAnyOrder(t *testing.T) {
	tests := []struct {
		name         string
		args         []string
		wantStr      string
		wantBool     bool
		wantOperands []string
		wantErr      bool
	}{
		{
			name:         "flags before the operand",
			args:         []string{"--str", "x", "--flag", "addr"},
			wantStr:      "x",
			wantBool:     true,
			wantOperands: []string{"addr"},
		},
		{
			name:         "flags after the operand",
			args:         []string{"addr", "--str", "x", "--flag"},
			wantStr:      "x",
			wantBool:     true,
			wantOperands: []string{"addr"},
		},
		{
			name:         "flags on both sides",
			args:         []string{"--str", "x", "addr", "--flag"},
			wantStr:      "x",
			wantBool:     true,
			wantOperands: []string{"addr"},
		},
		{
			name:         "several operands keep their order",
			args:         []string{"one", "--str=x", "two", "--flag", "three"},
			wantStr:      "x",
			wantBool:     true,
			wantOperands: []string{"one", "two", "three"},
		},
		{
			name:         "no flags at all",
			args:         []string{"addr"},
			wantOperands: []string{"addr"},
		},
		{
			name:         "double dash ends flag parsing",
			args:         []string{"--str", "x", "--", "--flag", "addr"},
			wantStr:      "x",
			wantOperands: []string{"--flag", "addr"},
		},
		{
			name:    "unknown flag after the operand errors",
			args:    []string{"addr", "--nope"},
			wantErr: true,
		},
		{
			name:    "unknown flag before the operand errors",
			args:    []string{"--nope", "addr"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			str := fs.String("str", "", "")
			b := fs.Bool("flag", false, "")
			operands, err := parseFlagsAnyOrder(fs, tt.args)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseFlagsAnyOrder(%q) = %q, want error", tt.args, operands)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseFlagsAnyOrder(%q): %v", tt.args, err)
			}
			if *str != tt.wantStr || *b != tt.wantBool {
				t.Errorf("parseFlagsAnyOrder(%q): --str=%q --flag=%v, want --str=%q --flag=%v", tt.args, *str, *b, tt.wantStr, tt.wantBool)
			}
			if !reflect.DeepEqual(operands, tt.wantOperands) {
				t.Errorf("parseFlagsAnyOrder(%q) operands = %q, want %q", tt.args, operands, tt.wantOperands)
			}
		})
	}
}

// TestProvideFlagsAnyOrder is the regression the fix exists for: the invocation
// people actually type — address first, flags after — must configure the provider
// identically to the flags-first spelling. It used to parse to an address and
// nothing else, so a --ca that was never read surfaced as a certificate error.
func TestProvideFlagsAnyOrder(t *testing.T) {
	orders := map[string][]string{
		"address first": {"localhost:7443", "--ca", "dev-ca.pem", "--token-file", "tok", "--publish-sessions", "--runtime-events", "--label", "zone=home"},
		"flags first":   {"--ca", "dev-ca.pem", "--token-file", "tok", "--publish-sessions", "--runtime-events", "--label", "zone=home", "localhost:7443"},
		"--orchestrator": {"--orchestrator", "localhost:7443", "--ca", "dev-ca.pem", "--token-file", "tok",
			"--publish-sessions", "--runtime-events", "--label", "zone=home"},
	}
	for name, args := range orders {
		t.Run(name, func(t *testing.T) {
			f, operands, err := parseProvideFlags(t, args)
			if err != nil {
				t.Fatalf("parse(%q): %v", args, err)
			}
			addr, err := provideAddr(f.orch, operands)
			if err != nil {
				t.Fatalf("provideAddr: %v", err)
			}
			if addr != "localhost:7443" {
				t.Errorf("address = %q, want localhost:7443", addr)
			}
			if f.caFile != "dev-ca.pem" {
				t.Errorf("--ca = %q, want dev-ca.pem", f.caFile)
			}
			if f.tokenFile != "tok" {
				t.Errorf("--token-file = %q, want tok", f.tokenFile)
			}
			if !f.publishSes || !f.rtEvents {
				t.Errorf("--publish-sessions=%v --runtime-events=%v, want both true", f.publishSes, f.rtEvents)
			}
			if !reflect.DeepEqual([]string(f.labels), []string{"zone=home"}) {
				t.Errorf("--label = %q, want [zone=home]", f.labels)
			}
		})
	}
}

// TestProvideAddr covers the address resolution around that parse: no address at
// all is left for the caller to explain, while an ambiguous or surplus operand is
// refused with a message that names the offending words.
func TestProvideAddr(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		want     string
		wantErr  bool
		errWords []string
	}{
		{name: "positional", args: []string{"localhost:7443"}, want: "localhost:7443"},
		{name: "flag", args: []string{"--orchestrator", "localhost:7443"}, want: "localhost:7443"},
		{name: "flag and matching positional", args: []string{"--orchestrator", "localhost:7443", "localhost:7443"}, want: "localhost:7443"},
		{name: "no address", args: nil, want: ""},
		{
			name:     "flag and a different positional",
			args:     []string{"--orchestrator", "a:1", "b:2"},
			wantErr:  true,
			errWords: []string{"a:1", "b:2"},
		},
		{
			name:     "surplus operand",
			args:     []string{"localhost:7443", "--ca", "dev-ca.pem", "extra"},
			wantErr:  true,
			errWords: []string{"extra"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, operands, err := parseProvideFlags(t, tt.args)
			if err != nil {
				t.Fatalf("parse(%q): %v", tt.args, err)
			}
			got, err := provideAddr(f.orch, operands)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("provideAddr(%q) = %q, want error", tt.args, got)
				}
				for _, w := range tt.errWords {
					if !strings.Contains(err.Error(), w) {
						t.Errorf("provideAddr(%q) error %q does not name %q", tt.args, err, w)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("provideAddr(%q): %v", tt.args, err)
			}
			if got != tt.want {
				t.Errorf("provideAddr(%q) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}

// parseProvideFlags parses args against provide's real flag surface, so these
// tests exercise the same registration and parse the command runs.
func parseProvideFlags(t *testing.T, args []string) (*provideFlags, []string, error) {
	t.Helper()
	fs := flag.NewFlagSet("provide", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	f := &provideFlags{}
	f.register(fs)
	operands, err := parseFlagsAnyOrder(fs, args)
	return f, operands, err
}
