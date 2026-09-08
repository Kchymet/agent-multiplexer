package launchenv

import (
	"slices"
	"strings"
	"testing"
)

func TestBuildAllowsOnlyFunctionalAmbientAndExplicitOverlay(t *testing.T) {
	ambient := []string{
		"PATH=/usr/bin:/bin", "HOME=/home/test", "LANG=en_US.UTF-8", "LC_TIME=C",
		"COLORTERM=truecolor", "AWS_ACCESS_KEY_ID=ambient-aws",
		"LC_SECRET=not-a-locale",
		"AWS_SECRET_ACCESS_KEY=ambient-secret", "AWS_SESSION_TOKEN=ambient-session",
		"AZURE_CLIENT_SECRET=ambient-azure", "GOOGLE_API_KEY=ambient-google",
		"GH_ENTERPRISE_TOKEN=ambient-gh", "GITHUB_ENTERPRISE_TOKEN=ambient-github",
		"AMUX_MUX_TOKEN=ambient-amux", "LD_PRELOAD=/tmp/host.so",
	}
	overlay := []string{
		"AMUX_SESSION_ID=session-1", "AMUX_ROLE=", "CODEX_HOME=/session/.amux/codex",
		"CLAUDE_CODE_OAUTH_TOKEN=", "TERM=xterm-256color",
	}
	got, err := Build(ambient, overlay, ModelCapability{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"PATH=/amux-bin:/usr/bin:/bin", "HOME=/home/test", "LANG=en_US.UTF-8", "LC_TIME=C",
		"COLORTERM=truecolor", "AMUX_SESSION_ID=session-1", "AMUX_ROLE=",
		"CODEX_HOME=/session/.amux/codex", "CLAUDE_CODE_OAUTH_TOKEN=",
		"TERM=xterm-256color",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("Build omitted %q: %v", want, got)
		}
	}
	joined := strings.Join(got, "\n")
	for _, denied := range []string{
		"AWS_", "AZURE_", "GOOGLE_", "GH_ENTERPRISE_TOKEN=",
		"GITHUB_ENTERPRISE_TOKEN=", "AMUX_MUX_TOKEN=", "LD_PRELOAD=", "LC_SECRET=",
	} {
		if strings.Contains(joined, denied) {
			t.Errorf("Build retained denied ambient %q: %v", denied, got)
		}
	}
}

func TestBuildRejectsNonCapabilityFinalOverlay(t *testing.T) {
	for _, entry := range []string{
		"AWS_ACCESS_KEY_ID=overlay-aws", "AZURE_CLIENT_SECRET=overlay-azure",
		"GOOGLE_API_KEY=overlay-google", "GH_ENTERPRISE_TOKEN=overlay-gh",
		"GITHUB_ENTERPRISE_TOKEN=overlay-github", "AMUX_MUX_TOKEN=overlay-amux",
	} {
		t.Run(strings.SplitN(entry, "=", 2)[0], func(t *testing.T) {
			if _, err := Build([]string{"PATH=/bin"}, []string{entry}, ModelCapability{}); err == nil {
				t.Fatalf("Build accepted %q", entry)
			}
		})
	}
}

func TestBuildProjectsOnlySelectedModelAccount(t *testing.T) {
	ambient := []string{
		"OPENAI_API_KEY=codex-key", "CODEX_API_KEY=codex-alt",
		"OPENAI_BASE_URL=https://openai.invalid",
		"ANTHROPIC_API_KEY=claude-key", "ANTHROPIC_AUTH_TOKEN=claude-alt",
		"CLAUDE_CODE_OAUTH_TOKEN=claude-oauth", "ANTHROPIC_BASE_URL=https://anthropic.invalid",
		"AWS_SECRET_ACCESS_KEY=cloud-key",
	}
	for _, tc := range []struct {
		name    string
		account ModelAccount
		want    []string
		deny    []string
	}{
		{"codex", CodexModelAccount,
			[]string{"OPENAI_API_KEY=codex-key", "CODEX_API_KEY=codex-alt", "OPENAI_BASE_URL=https://openai.invalid"},
			[]string{"ANTHROPIC_", "CLAUDE_CODE_OAUTH_TOKEN=", "AWS_"}},
		{"claude", ClaudeModelAccount,
			[]string{"ANTHROPIC_API_KEY=claude-key", "ANTHROPIC_AUTH_TOKEN=claude-alt", "CLAUDE_CODE_OAUTH_TOKEN=claude-oauth", "ANTHROPIC_BASE_URL=https://anthropic.invalid"},
			[]string{"OPENAI_", "CODEX_API_KEY=", "AWS_"}},
		{"none", NoModelAccount, nil, []string{"OPENAI_", "CODEX_API_KEY=", "ANTHROPIC_", "CLAUDE_CODE_OAUTH_TOKEN=", "AWS_"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Build(ambient, nil, ModelCapability{account: tc.account})
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range tc.want {
				if !slices.Contains(got, want) {
					t.Errorf("selected account omitted %q: %v", want, got)
				}
			}
			joined := strings.Join(got, "\n")
			for _, deny := range tc.deny {
				if strings.Contains(joined, deny) {
					t.Errorf("selected account retained %q: %v", deny, got)
				}
			}
		})
	}
}

func TestCredentialOverlayCannotMintModelAccount(t *testing.T) {
	for _, account := range []ModelAccount{NoModelAccount, ClaudeModelAccount, CodexModelAccount} {
		if _, err := Build(nil, []string{"OPENAI_API_KEY=caller-key"}, ModelCapability{account: account}); err == nil {
			t.Fatalf("account %d accepted a caller-provided credential", account)
		}
	}
}

func TestExplicitSyntheticModelEnvironmentIsAccountScoped(t *testing.T) {
	capability, err := ForModelEnvironment(CodexModelAccount, []string{
		"OPENAI_BASE_URL=http://127.0.0.1:12345/v1",
		"OPENAI_API_KEY=synthetic-test-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := Build([]string{
		"OPENAI_API_KEY=ambient-must-be-overridden",
		"ANTHROPIC_API_KEY=ambient-other-account",
	}, nil, capability)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"OPENAI_BASE_URL=http://127.0.0.1:12345/v1",
		"OPENAI_API_KEY=synthetic-test-key",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("explicit model capability omitted %q: %v", want, got)
		}
	}
	if strings.Contains(strings.Join(got, "\n"), "ANTHROPIC_API_KEY=") {
		t.Fatalf("explicit Codex capability crossed account boundary: %v", got)
	}
	if _, err := ForModelEnvironment(CodexModelAccount, []string{"ANTHROPIC_API_KEY=wrong-account"}); err == nil {
		t.Fatal("Codex model capability accepted an Anthropic credential")
	}
}

func TestBuildDropsUnscopedExecutablePaths(t *testing.T) {
	got, err := Build([]string{
		"PATH=/tmp/operator-bin:relative:/home/operator/.local/bin:/usr/local/bin:/bin",
	}, nil, ModelCapability{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got, "PATH=/amux-bin:/usr/local/bin:/bin") {
		t.Fatalf("sanitized PATH = %v", got)
	}
}
