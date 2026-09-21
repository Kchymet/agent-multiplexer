package credentialbroker

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/cli/go-gh/v2/pkg/auth"
	"github.com/cli/go-gh/v2/pkg/config"
)

const GitHubAccount = "active"

// GitHubHost accepts DNS hostnames only. Neither URLs, ports nor command-line
// options can reach the host CLI through the credential protocol.
func GitHubHost(o Operation) (string, bool) {
	host, ok := strings.CutPrefix(o.Service, "gh:")
	if !ok || o.Verb != Read || o.Account != GitHubAccount || o.Value != "" || len(host) == 0 || len(host) > 253 || host != strings.ToLower(host) {
		return "", false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return "", false
			}
		}
	}
	return host, true
}

// GitHubHosts reads host state afresh so login, logout and account switching
// take effect without restarting the daemon. Do not use config.Read's cache.
func GitHubHosts() []string {
	var hosts []string
	f, err := os.Open(filepath.Join(config.ConfigDir(), "hosts.yml"))
	if err == nil {
		defer f.Close()
		data, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
		if err == nil && len(data) <= 1<<20 {
			hosts, _ = config.ReadFromString(string(data)).Keys(nil)
		}
	}
	// Ambient tokens are limited to the daemon's selected host, never an
	// arbitrary requested enterprise endpoint.
	if os.Getenv("GH_TOKEN") != "" || os.Getenv("GITHUB_TOKEN") != "" {
		hosts = append(hosts, "github.com")
	}
	if host := os.Getenv("GH_HOST"); host != "" {
		key, fallback := GitHubTokenKeys(host)
		if os.Getenv(key) != "" || os.Getenv(fallback) != "" {
			hosts = append(hosts, host)
		}
	}
	return slices.DeleteFunc(hosts, func(host string) bool {
		_, ok := GitHubHost(Operation{Verb: Read, Service: "gh:" + host, Account: GitHubAccount})
		return !ok
	})
}

func GitHubAllowed(o Operation) bool {
	host, ok := GitHubHost(o)
	return ok && slices.Contains(GitHubHosts(), host)
}

func GitHubTokenKeys(host string) (string, string) {
	if auth.IsEnterprise(host) {
		return "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"
	}
	return "GH_TOKEN", "GITHUB_TOKEN"
}

// NativeGitHub is resolved only on the host when publishing a launch or serving
// a credential request. The session uses the protected gh-real alias instead.
func NativeGitHub() (string, error) {
	path, err := exec.LookPath("gh")
	if err != nil {
		return "", err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil || filepath.Base(path) == "amux" {
		return "", errors.New("native GitHub CLI unavailable")
	}
	return path, nil
}

type GitHub struct{ Env []string }

// Execute returns only the active account selected by host gh. Using the
// native CLI preserves its Keychain encoding and environment/config precedence.
// There is no account selector, login mutation, network call or stderr relay.
func (g GitHub) Execute(ctx context.Context, o Operation) (Result, error) {
	if !GitHubAllowed(o) {
		return Result{}, ErrInvalid
	}
	host, _ := GitHubHost(o)
	path, err := NativeGitHub()
	if err != nil {
		return Result{}, errors.New("GitHub credential service unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "auth", "token", "--hostname", host)
	cmd.Env = g.Env
	var output boundedOutput
	cmd.Stdout = &output
	if err := cmd.Run(); err != nil || output.overflow {
		return Result{}, errors.New("GitHub credential lookup failed; check host gh auth status")
	}
	value := strings.TrimSpace(output.String())
	if value == "" || strings.ContainsAny(value, "\x00\r\n") {
		return Result{}, errors.New("invalid GitHub credential")
	}
	return Result{Value: value}, nil
}
