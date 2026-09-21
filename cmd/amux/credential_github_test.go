package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestGitHubCommandHost(t *testing.T) {
	t.Setenv("GH_CONFIG_DIR", t.TempDir())
	t.Setenv("GH_HOST", "github.com")
	t.Setenv("GH_REPO", "enterprise.example/owner/repo")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"api", "user"}, "github.com"},
		{[]string{"search", "repos", "amux"}, "github.com"},
		{[]string{"repo", "list"}, "github.com"},
		{[]string{"api", "--hostname", "enterprise.example", "user"}, "enterprise.example"},
		{[]string{"auth", "status", "-h", "enterprise.example"}, "enterprise.example"},
		{[]string{"pr", "list"}, "enterprise.example"},
		{[]string{"pr", "list", "-R", "other.example/org/repo"}, "other.example"},
		{[]string{"pr", "list", "--repo=other.example/org/repo"}, "other.example"},
		{[]string{"pr", "list", "-Rother.example/org/repo"}, "other.example"},
		{[]string{"pr", "view", "https://other.example/org/repo/pull/1"}, "other.example"},
		{[]string{"repo", "clone", "org/repo"}, "github.com"},
		{[]string{"repo", "view", "other.example/org/repo"}, "other.example"},
		{[]string{"api", "https://api.github.com/user"}, "github.com"},
		{[]string{"api", "user", "--field", "https://data.example"}, "github.com"},
		{[]string{"pr", "create", "--body", "https://data.example"}, "enterprise.example"},
	} {
		got, err := githubCommandHost(tc.args)
		if err != nil || got != tc.want {
			t.Errorf("%v: host=%q err=%v, want %q", tc.args, got, err, tc.want)
		}
	}
	for _, args := range [][]string{
		{"auth", "token", "--user", "other"},
		{"auth", "token", "-uother"},
		{"api", "--hostname", "--invalid"},
		{"api", "--hostname"},
		{"pr", "view", "--hostname", "github.com", "https://other.example/org/repo/pull/1"},
	} {
		if _, err := githubCommandHost(args); err == nil {
			t.Errorf("accepted invalid host selection: %v", args)
		}
	}
}

func TestGitHubGitCredentialProtocol(t *testing.T) {
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"} {
		t.Setenv(key, "")
	}
	for _, tc := range []struct {
		name, operation, input, host, output string
	}{
		{"get", "get", "protocol=https\nhost=github.com\nusername=old\n\n", "github.com", "username=x-access-token\npassword=synthetic\n\n"},
		{"gist", "get", "protocol=https\nhost=gist.github.com\n\n", "github.com", "username=x-access-token\npassword=synthetic\n\n"},
		{"unknown", "get", "protocol=https\nhost=unknown.example\n\n", "unknown.example", ""},
		{"http", "get", "protocol=http\nhost=github.com\n\n", "", ""},
		{"invalid host", "get", "protocol=https\nhost=github.com:444\n\n", "", ""},
		{"store", "store", "", "", ""},
		{"erase", "erase", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			called := ""
			read := func(host string) (string, error) {
				called = host
				if host != "github.com" {
					return "", errors.New("denied")
				}
				return "synthetic", nil
			}
			code := githubGitCredential([]string{tc.operation}, strings.NewReader(tc.input), &output, read)
			if code != 0 || called != tc.host || output.String() != tc.output {
				t.Fatal("incorrect Git credential protocol handling")
			}
		})
	}
}

func TestGitHubExplicitSessionToken(t *testing.T) {
	t.Setenv("GH_TOKEN", "explicit-public")
	t.Setenv("GITHUB_TOKEN", "fallback-public")
	t.Setenv("GH_ENTERPRISE_TOKEN", "explicit-enterprise")
	read := func(string) (string, error) { t.Fatal("explicit override reached host broker"); return "", nil }
	for host, want := range map[string]string{"github.com": "explicit-public", "company.ghe.com": "explicit-public", "enterprise.example": "explicit-enterprise"} {
		if token, err := githubToken(host, read); err != nil || token != want {
			t.Fatal("incorrect explicit token precedence")
		}
	}
}
