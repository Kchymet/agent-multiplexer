// Package gh is a thin wrapper over the GitHub CLI (`gh`), used to authenticate
// and to fuzzy-find / clone repositories by owner (the user's account and the
// orgs they belong to) with GitHub's own auth.
package gh

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Repo is a remote repository as reported by `gh repo list`.
type Repo struct {
	NameWithOwner string `json:"nameWithOwner"`
	SSHURL        string `json:"sshUrl"`
	URL           string `json:"url"`
}

// Installed reports whether the gh binary is on PATH.
func Installed() bool {
	_, err := exec.LookPath("gh")
	return err == nil
}

// Authed reports whether gh is authenticated.
func Authed(ctx context.Context) bool {
	if !Installed() {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "gh", "auth", "status").Run() == nil
}

// Login runs the interactive `gh auth login` flow, inheriting the terminal.
func Login(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "gh", "auth", "login")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// Owners returns the selectable repository owners: the authenticated user's
// login first, then the orgs they belong to.
func Owners(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	owners := []string{}
	if login := strings.TrimSpace(apiField(ctx, "user", ".login")); login != "" {
		owners = append(owners, login)
	}
	for _, org := range apiLines(ctx, "user/orgs", ".[].login") {
		if org != "" {
			owners = append(owners, org)
		}
	}
	return owners, nil
}

// ListReposFor returns the repositories owned by owner (a user or org).
func ListReposFor(ctx context.Context, owner string) ([]Repo, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gh", "repo", "list", owner,
		"--limit", "1000",
		"--json", "nameWithOwner,sshUrl,url",
	).Output()
	if err != nil {
		return nil, err
	}
	var repos []Repo
	if err := json.Unmarshal(out, &repos); err != nil {
		return nil, err
	}
	return repos, nil
}

// CloneBare clones a remote repo (OWNER/REPO) as a bare clone at gitDir, using
// gh's auth (works for private repos).
func CloneBare(ctx context.Context, nameWithOwner, gitDir string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	return exec.CommandContext(ctx, "gh", "repo", "clone", nameWithOwner, gitDir, "--", "--bare").Run()
}

// apiField runs `gh api <path> --jq <jq>` and returns the single-line result.
func apiField(ctx context.Context, path, jq string) string {
	out, err := exec.CommandContext(ctx, "gh", "api", path, "--jq", jq).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// apiLines runs `gh api <path> --jq <jq>` and returns non-empty output lines.
func apiLines(ctx context.Context, path, jq string) []string {
	out, err := exec.CommandContext(ctx, "gh", "api", "--paginate", path, "--jq", jq).Output()
	if err != nil {
		return nil
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}
