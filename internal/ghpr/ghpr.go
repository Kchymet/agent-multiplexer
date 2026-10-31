// Package ghpr answers one question for amux's lifecycle guards: does a branch
// (or a family of branches sharing a prefix) still own an open pull request?
//
// It is deliberately small. The Lister interface is the seam tests inject; the
// default implementation shells out to the GitHub CLI with an explicit
// -R HOST/OWNER/REPO so gh never reads a session-writable worktree's Git config.
package ghpr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"
)

// PR is one open pull request whose head branch matched.
type PR struct {
	Number      int    `json:"number"`
	URL         string `json:"url"`
	HeadRefName string `json:"headRefName"`
	IsDraft     bool   `json:"isDraft"`
}

// Lister lists the open pull requests on repo (a HOST/OWNER/REPO or OWNER/REPO
// slug, see Slug) whose head branch is branch or a suffixed sibling of it (see
// MatchesBranch).
type Lister interface {
	OpenPRs(ctx context.Context, repo, branch string) ([]PR, error)
}

// DefaultTimeout bounds one gh invocation. The guards that use this fail open,
// so a slow GitHub only delays an archive by this much, never blocks it.
const DefaultTimeout = 8 * time.Second

// GH is the gh-CLI Lister.
type GH struct {
	// Timeout bounds each call; zero means DefaultTimeout.
	Timeout time.Duration
	// Limit caps how many open PRs are fetched per repo; zero means 200.
	Limit int
}

// OpenPRs runs `gh pr list --state open` against repo and filters the result
// by head branch. Any failure (gh missing, unauthenticated, network, timeout)
// is returned as an error for the caller to fail open on.
func (g GH) OpenPRs(ctx context.Context, repo, branch string) ([]PR, error) {
	if _, err := exec.LookPath("gh"); err != nil {
		return nil, fmt.Errorf("gh not installed: %w", err)
	}
	timeout := g.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	limit := g.Limit
	if limit <= 0 {
		limit = 200
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "pr", "list", "-R", repo, "--state", "open",
		"--limit", fmt.Sprint(limit), "--json", "number,url,headRefName,isDraft")
	// Run outside any checkout: -R names the repo, so no Git config is consulted.
	cmd.Dir = os.TempDir()
	cmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("gh pr list %s: %w", repo, ctx.Err())
		}
		return nil, fmt.Errorf("gh pr list %s: %w: %s", repo, err, strings.TrimSpace(stderr.String()))
	}
	var all []PR
	if err := json.Unmarshal(out, &all); err != nil {
		return nil, fmt.Errorf("decode gh pr list %s: %w", repo, err)
	}
	return Filter(all, branch), nil
}

// Filter keeps the PRs whose head branch MatchesBranch.
func Filter(prs []PR, branch string) []PR {
	var out []PR
	for _, pr := range prs {
		if MatchesBranch(pr.HeadRefName, branch) {
			out = append(out, pr)
		}
	}
	return out
}

// MatchesBranch reports whether head is branch itself or a sibling an agent
// pushed from the same session, named branch-<suffix> or branch/<suffix> (for
// example amux/wg-member-b). Member ids are fixed-width, so the separator keeps
// amux/wg-a1 from claiming amux/wg-a10.
func MatchesBranch(head, branch string) bool {
	if branch == "" {
		return false
	}
	return head == branch || strings.HasPrefix(head, branch+"-") || strings.HasPrefix(head, branch+"/")
}

// Slug turns a tracked repository source into the -R argument gh accepts:
// OWNER/REPO for a bare GitHub slug, HOST/OWNER/REPO for an https, ssh or scp
// style remote URL. ok is false for anything else (a local path, say), which
// has no pull requests to check.
func Slug(source string) (string, bool) {
	s := strings.TrimSpace(source)
	if s == "" {
		return "", false
	}
	var host, path string
	switch {
	case strings.Contains(s, "://"):
		u, err := url.Parse(s)
		if err != nil || u.Hostname() == "" || u.Scheme == "file" {
			return "", false
		}
		host, path = u.Hostname(), u.Path
	case strings.Contains(s, "@") && strings.Contains(s, ":"):
		// scp-like: git@github.com:owner/repo.git
		at := strings.Index(s, "@")
		colon := strings.Index(s[at:], ":")
		if colon < 0 {
			return "", false
		}
		host, path = s[at+1:at+colon], s[at+colon+1:]
	case !strings.HasPrefix(s, "/") && !strings.HasPrefix(s, ".") && !strings.HasPrefix(s, "~") && strings.Count(s, "/") == 1:
		if _, err := os.Stat(s); err == nil {
			return "", false // a relative local directory, not a slug
		}
		host, path = "github.com", s
	default:
		return "", false
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	parts := strings.Split(path, "/")
	if host == "" || len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", false
	}
	if host == "github.com" {
		return parts[0] + "/" + parts[1], true
	}
	return host + "/" + parts[0] + "/" + parts[1], true
}
