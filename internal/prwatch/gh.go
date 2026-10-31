package prwatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// Runner runs `gh` with args and returns its stdout. Production uses ExecRunner;
// tests inject a fake so no test touches GitHub.
type Runner func(ctx context.Context, args ...string) ([]byte, error)

// ghTimeout bounds one gh invocation.
const ghTimeout = 60 * time.Second

// ExecRunner runs the gh binary on PATH, with stderr folded into the error so
// auth and rate-limit failures can be classified.
func ExecRunner(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, ghTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", args...)
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && len(exit.Stderr) > 0 {
			return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(exit.Stderr)))
		}
		return nil, err
	}
	return out, nil
}

// prFields is the gh --json field list: everything the PR summary needs.
const prFields = "number,url,state,isDraft,mergeable,mergeStateStatus,statusCheckRollup,reviewDecision,headRefName,latestReviews"

// ghPR is gh's JSON for one PR (the prFields subset).
type ghPR struct {
	Number            int    `json:"number"`
	URL               string `json:"url"`
	State             string `json:"state"`
	IsDraft           bool   `json:"isDraft"`
	Mergeable         string `json:"mergeable"`
	MergeStateStatus  string `json:"mergeStateStatus"`
	HeadRefName       string `json:"headRefName"`
	ReviewDecision    string `json:"reviewDecision"`
	StatusCheckRollup []struct {
		Typename   string `json:"__typename"`
		Name       string `json:"name"`       // CheckRun
		Status     string `json:"status"`     // CheckRun: QUEUED | IN_PROGRESS | COMPLETED ...
		Conclusion string `json:"conclusion"` // CheckRun: SUCCESS | FAILURE | ...
		Context    string `json:"context"`    // StatusContext
		State      string `json:"state"`      // StatusContext: SUCCESS | FAILURE | ERROR | PENDING | EXPECTED
	} `json:"statusCheckRollup"`
	LatestReviews []struct {
		Author struct {
			Login string `json:"login"`
		} `json:"author"`
		State       string `json:"state"`
		SubmittedAt string `json:"submittedAt"`
	} `json:"latestReviews"`
}

// summarize converts gh's PR JSON into the published summary.
func summarize(repo string, g ghPR) harnessproto.SessionPR {
	pr := harnessproto.SessionPR{
		Repo:           repo,
		Number:         g.Number,
		URL:            g.URL,
		Branch:         g.HeadRefName,
		State:          strings.ToLower(g.State),
		Draft:          g.IsDraft,
		Mergeable:      strings.ToLower(g.Mergeable),
		MergeState:     strings.ToLower(g.MergeStateStatus),
		ReviewDecision: strings.ToLower(g.ReviewDecision),
	}
	pending, passed := false, false
	for _, c := range g.StatusCheckRollup {
		name, failed, done := c.Name, false, true
		if c.Typename == "StatusContext" || (c.Context != "" && c.Name == "") {
			name = c.Context
			switch strings.ToUpper(c.State) {
			case "FAILURE", "ERROR":
				failed = true
			case "PENDING", "EXPECTED", "":
				done = false
			}
		} else {
			if strings.ToUpper(c.Status) != "COMPLETED" {
				done = false
			}
			switch strings.ToUpper(c.Conclusion) {
			case "FAILURE", "TIMED_OUT", "CANCELLED", "ACTION_REQUIRED", "STARTUP_FAILURE":
				failed = true
			}
		}
		switch {
		case failed:
			pr.FailedChecks = append(pr.FailedChecks, name)
		case !done:
			pending = true
		default:
			passed = true
		}
	}
	sort.Strings(pr.FailedChecks)
	switch {
	case len(pr.FailedChecks) > 0:
		pr.Checks = "fail"
	case pending:
		pr.Checks = "pending"
	case passed:
		pr.Checks = "pass"
	}
	for _, r := range g.LatestReviews {
		if pr.LastReview == nil || r.SubmittedAt > pr.LastReview.At {
			pr.LastReview = &harnessproto.PRReview{Author: r.Author.Login, State: strings.ToLower(r.State), At: r.SubmittedAt}
		}
	}
	return pr
}

// listOpen lists a repo's open PRs whose head branch starts with prefix: one
// gh call (a search on the head-branch qualifier) for every member branch in
// the repo that shares the prefix.
func listOpen(ctx context.Context, run Runner, repo, prefix string) ([]harnessproto.SessionPR, error) {
	out, err := run(ctx, "pr", "list", "--repo", repo, "--state", "open", "--limit", "200",
		"--search", "head:"+prefix, "--json", prFields)
	if err != nil {
		return nil, err
	}
	var raw []ghPR
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("gh pr list %s: %w", repo, err)
	}
	prs := make([]harnessproto.SessionPR, 0, len(raw))
	for _, g := range raw {
		prs = append(prs, summarize(repo, g))
	}
	return prs, nil
}

// listHead lists a repo's open PRs from exactly one head branch (a member
// branch with no "/" prefix to search on).
func listHead(ctx context.Context, run Runner, repo, branch string) ([]harnessproto.SessionPR, error) {
	out, err := run(ctx, "pr", "list", "--repo", repo, "--state", "open", "--limit", "20",
		"--head", branch, "--json", prFields)
	if err != nil {
		return nil, err
	}
	var raw []ghPR
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("gh pr list %s: %w", repo, err)
	}
	prs := make([]harnessproto.SessionPR, 0, len(raw))
	for _, g := range raw {
		prs = append(prs, summarize(repo, g))
	}
	return prs, nil
}

// view reads one PR (used only for a tracked open PR that left the open list,
// to learn whether it merged or closed).
func view(ctx context.Context, run Runner, repo string, number int) (harnessproto.SessionPR, error) {
	out, err := run(ctx, "pr", "view", fmt.Sprint(number), "--repo", repo, "--json", prFields)
	if err != nil {
		return harnessproto.SessionPR{}, err
	}
	var g ghPR
	if err := json.Unmarshal(out, &g); err != nil {
		return harnessproto.SessionPR{}, fmt.Errorf("gh pr view %s#%d: %w", repo, number, err)
	}
	return summarize(repo, g), nil
}

// errorClass sorts a gh failure: a missing binary or missing auth is a setup
// condition (logged once, polled slowly); a rate limit or anything else is
// transient (backed off).
type errorClass int

const (
	errTransient errorClass = iota
	errSetup
	errRateLimit
)

func classify(err error) errorClass {
	if errors.Is(err, exec.ErrNotFound) {
		return errSetup
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "rate limit"), strings.Contains(msg, "secondary rate"):
		return errRateLimit
	case strings.Contains(msg, "gh auth login"), strings.Contains(msg, "not logged in"),
		strings.Contains(msg, "authentication"), strings.Contains(msg, "executable file not found"),
		strings.Contains(msg, "http 401"), strings.Contains(msg, "bad credentials"):
		return errSetup
	}
	return errTransient
}

// RepoSlug extracts GitHub's owner/name from a repository source: an https or
// ssh GitHub URL, or a bare owner/name. Anything else (a local path, another
// host) yields "", and the watcher skips that repo.
func RepoSlug(source string) string {
	s := strings.TrimSpace(source)
	for _, prefix := range []string{"https://github.com/", "http://github.com/", "ssh://git@github.com/", "git@github.com:", "github.com/"} {
		if strings.HasPrefix(strings.ToLower(s), prefix) {
			s = s[len(prefix):]
			return ownerName(s)
		}
	}
	if strings.Contains(s, "://") || strings.HasPrefix(s, "/") || strings.HasPrefix(s, ".") || strings.HasPrefix(s, "~") || strings.Contains(s, ":") {
		return ""
	}
	return ownerName(s)
}

func ownerName(s string) string {
	s = strings.TrimSuffix(strings.TrimSuffix(s, "/"), ".git")
	parts := strings.Split(s, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return ""
	}
	return parts[0] + "/" + parts[1]
}
