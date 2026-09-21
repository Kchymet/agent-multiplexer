package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"amux/internal/credentialbroker"
	"amux/internal/sessionrpc"

	"github.com/cli/go-gh/v2/pkg/auth"
	"github.com/cli/go-gh/v2/pkg/repository"
)

// The protected gh alias obtains credentials on demand. They enter only this
// command's environment, not the harness environment or a plaintext auth file.
func credentialGitHub(args []string) int {
	self, err := os.Executable()
	if err != nil {
		return 1
	}
	native := filepath.Join(filepath.Dir(self), "gh-real")
	if len(args) >= 2 && args[0] == "auth" {
		switch args[1] {
		case "login", "logout", "refresh", "switch", "setup-git":
			fmt.Fprintln(os.Stderr, "amux: manage GitHub authentication in a host terminal; sessions inherit the host's active account")
			return 1
		case "git-credential":
			return githubGitCredential(args[2:], os.Stdin, os.Stdout, readGitHubCredential)
		}
	}
	if githubNeedsCredential(args) {
		host, err := githubCommandHost(args)
		if err != nil {
			fmt.Fprintln(os.Stderr, "amux: cannot determine GitHub host:", err)
			return 1
		}
		token, err := githubToken(host, readGitHubCredential)
		if err != nil {
			fmt.Fprintln(os.Stderr, "amux: GitHub credentials unavailable; check gh auth status in a host terminal and that the amux daemon is running")
			return 1
		}
		if len(args) >= 2 && args[0] == "auth" {
			if args[1] == "token" {
				fmt.Fprintln(os.Stdout, token)
				return 0
			}
			if args[1] == "status" {
				// Other saved accounts are intentionally unavailable to sessions.
				args = append(args, "--active", "--hostname", host)
			}
		}
		key, _ := credentialbroker.GitHubTokenKeys(host)
		os.Setenv(key, token)
		os.Setenv("GH_HOST", host)
	}
	if err := syscall.Exec(native, append([]string{"gh"}, args...), os.Environ()); err != nil {
		fmt.Fprintln(os.Stderr, "amux: cannot execute the host GitHub CLI")
		return 1
	}
	return 0
}

func githubToken(host string, read func(string) (string, error)) (string, error) {
	key, fallback := credentialbroker.GitHubTokenKeys(host)
	for _, name := range []string{key, fallback} {
		if token := os.Getenv(name); token != "" {
			return token, nil
		}
	}
	return read(host)
}

func readGitHubCredential(host string) (string, error) {
	client, err := openRestrictedSessionRPC()
	if err != nil {
		return "", err
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	o := credentialbroker.Operation{Verb: credentialbroker.Read, Service: "gh:" + host, Account: credentialbroker.GitHubAccount}
	response, err := client.Query(ctx, sessionrpc.Query{Verb: o.Verb, Fields: o.Fields()})
	if err != nil {
		return "", err
	}
	var result credentialbroker.Result
	if json.Unmarshal(response.Body, &result) != nil || result.ExitCode != 0 || result.Value == "" || strings.ContainsAny(result.Value, "\x00\r\n") {
		return "", errors.New("GitHub credential unavailable")
	}
	return result.Value, nil
}

func githubNeedsCredential(args []string) bool {
	if len(args) == 0 {
		return false
	}
	for _, arg := range args {
		if arg == "--help" {
			return false
		}
	}
	switch args[0] {
	case "help", "version", "--version", "-h", "completion", "config", "alias":
		return false
	}
	return true
}

// Match gh's explicit host/repository overrides and its repository remote
// selection using the upstream helper library. gh api/auth use the default
// host rather than the current repository. URL arguments select their own host.
func githubCommandHost(args []string) (string, error) {
	host, _ := auth.DefaultHost()
	var explicit, repo, urlHost string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		name, value, equals := strings.Cut(arg, "=")
		switch name {
		// URL-shaped request bodies, form values, filters and filenames are
		// data, not credential destinations.
		case "--body", "-b", "--body-file", "--title", "-t", "--jq", "-q", "--template", "--json",
			"--field", "-F", "--raw-field", "-f", "--header", "-H", "--method", "-X", "--input", "--output", "-o", "--cache", "--preview":
			if !equals {
				i++
			}
		case "--user", "-u":
			if len(args) > 0 && args[0] == "auth" {
				return "", errors.New("select the active account in a host terminal")
			}
		case "--hostname", "--repo", "-R", "-h":
			if name == "-h" && (len(args) == 0 || args[0] != "auth") {
				continue
			}
			if !equals {
				i++
				if i >= len(args) {
					return "", errors.New("missing host or repository")
				}
				value = args[i]
			}
			if name == "--repo" || name == "-R" {
				repo = value
			} else {
				explicit = value
			}
		default:
			if strings.HasPrefix(arg, "-u") && len(args) > 0 && args[0] == "auth" {
				return "", errors.New("select the active account in a host terminal")
			}
			if strings.HasPrefix(arg, "-R") && len(arg) > 2 {
				repo = arg[2:]
			} else if strings.HasPrefix(arg, "https://") || strings.HasPrefix(arg, "http://") {
				if u, err := url.Parse(arg); err == nil && u.Hostname() != "" {
					next := auth.NormalizeHostname(u.Hostname())
					if urlHost != "" && urlHost != next {
						return "", errors.New("multiple GitHub hosts in one command; run commands separately")
					}
					urlHost = next
				}
			}
		}
	}
	if githubUsesRepository(args) {
		if repo == "" {
			repo = os.Getenv("GH_REPO")
		}
		if len(args) >= 3 && args[0] == "repo" && (args[1] == "clone" || args[1] == "view" || args[1] == "fork") && !strings.HasPrefix(args[2], "-") {
			repo = args[2]
		}
		if repo != "" {
			r, err := repository.Parse(repo)
			if err != nil {
				return "", errors.New("invalid repository override")
			}
			host = r.Host
		} else if r, err := repository.Current(); err == nil {
			host = r.Host
		}
	}
	if explicit != "" {
		host = explicit
	}
	if urlHost != "" {
		if explicit != "" && auth.NormalizeHostname(explicit) != urlHost {
			return "", errors.New("conflicting GitHub hosts")
		}
		host = urlHost
	}
	host = auth.NormalizeHostname(host)
	if _, ok := credentialbroker.GitHubHost(credentialbroker.Operation{Verb: credentialbroker.Read, Service: "gh:" + host, Account: credentialbroker.GitHubAccount}); !ok {
		return "", errors.New("invalid GitHub hostname")
	}
	return host, nil
}

func githubUsesRepository(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "pr", "issue", "release", "run", "workflow", "ruleset", "variable", "secret", "label", "cache", "browse":
		return true
	case "repo":
		return len(args) > 1 && args[1] != "create" && args[1] != "list"
	}
	return false
}

// Implement the small Git credential protocol directly so stdin is not lost
// when dispatching to gh. Host configuration allows only known GitHub hosts;
// HTTPS credentials for other sites are left to Git's other helpers.
func githubGitCredential(args []string, input io.Reader, output io.Writer, read func(string) (string, error)) int {
	if len(args) != 1 {
		return 1
	}
	if args[0] == "store" || args[0] == "erase" {
		return 0 // session operations must never mutate the host login
	}
	if args[0] != "get" {
		return 1
	}
	fields := make(map[string]string)
	scanner := bufio.NewScanner(io.LimitReader(input, 64<<10+1))
	for scanner.Scan() {
		if scanner.Text() == "" {
			break
		}
		key, value, ok := strings.Cut(scanner.Text(), "=")
		if !ok {
			return 1
		}
		fields[key] = value
	}
	if scanner.Err() != nil || fields["protocol"] != "https" {
		return 0
	}
	host := auth.NormalizeHostname(fields["host"])
	if _, ok := credentialbroker.GitHubHost(credentialbroker.Operation{Verb: credentialbroker.Read, Service: "gh:" + host, Account: credentialbroker.GitHubAccount}); !ok {
		return 0
	}
	token, err := githubToken(host, read)
	if err != nil {
		return 0
	}
	_, err = fmt.Fprintf(output, "username=x-access-token\npassword=%s\n\n", token)
	if err != nil {
		return 1
	}
	return 0
}
