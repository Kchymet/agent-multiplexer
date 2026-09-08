package codexapp

import (
	"slices"
	"strings"
	"testing"

	"amux/internal/launchenv"
)

func TestAppServerEnvironmentIsSanitizedBeforeExec(t *testing.T) {
	ambient := []string{
		"PATH=/usr/bin:/bin",
		"OPENAI_API_KEY=selected-codex-key",
		"ANTHROPIC_API_KEY=other-model-key",
		"AWS_SECRET_ACCESS_KEY=operator-aws",
		"AZURE_CLIENT_SECRET=operator-azure",
		"GOOGLE_APPLICATION_CREDENTIALS=/operator/gcp.json",
		"GH_ENTERPRISE_TOKEN=operator-gh",
		"AMUX_MUX_TOKEN=operator-amux",
	}
	got, err := appServerEnvironment(ambient, []string{"AMUX_SESSION_ID=subject"}, launchenv.ForRuntime("codex"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"PATH=/usr/bin:/bin", "OPENAI_API_KEY=selected-codex-key", "AMUX_SESSION_ID=subject"} {
		if !slices.Contains(got, want) {
			t.Errorf("AppServer environment omitted %q: %v", want, got)
		}
	}
	joined := strings.Join(got, "\n")
	for _, denied := range []string{
		"ANTHROPIC_API_KEY=", "AWS_SECRET_ACCESS_KEY=", "AZURE_CLIENT_SECRET=",
		"GOOGLE_APPLICATION_CREDENTIALS=", "GH_ENTERPRISE_TOKEN=", "AMUX_MUX_TOKEN=",
	} {
		if strings.Contains(joined, denied) {
			t.Errorf("AppServer inherited %s: %v", denied, got)
		}
	}
}

func TestAppServerSyntheticModelEndpointRequiresTypedCapability(t *testing.T) {
	if _, err := appServerEnvironment(nil, []string{
		"OPENAI_BASE_URL=http://127.0.0.1:12345/v1",
		"OPENAI_API_KEY=synthetic-key",
	}, launchenv.ForRuntime("codex")); err == nil {
		t.Fatal("generic AppServer overlay granted a synthetic model account")
	}
	capability, err := launchenv.ForModelEnvironment(launchenv.CodexModelAccount, []string{
		"OPENAI_BASE_URL=http://127.0.0.1:12345/v1",
		"OPENAI_API_KEY=synthetic-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := appServerEnvironment(nil, nil, capability)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"OPENAI_BASE_URL=http://127.0.0.1:12345/v1",
		"OPENAI_API_KEY=synthetic-key",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("typed synthetic model capability omitted %q: %v", want, got)
		}
	}
}
