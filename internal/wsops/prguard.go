package wsops

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"amux/internal/core"
	"amux/internal/ghpr"
	"amux/internal/store"
)

// FieldForce on set-archived (archived=true), delete or kill skips the open
// pull-request guard. Who may send it is decided by access policy: the host and
// the console always; a coordinator only for its direct members. An agent
// completing itself (`amux agent done`) can never force.
const FieldForce = "force"

// PRLister is the open-PR source the guard consults. Tests replace it.
var PRLister ghpr.Lister = ghpr.GH{}

// prGuardBudget bounds the whole guard (all repos and members, in parallel).
// Anything still pending when it lapses fails open.
var prGuardBudget = 12 * time.Second

// OpenPRError is the guard's refusal. Its message names only the sessions,
// branches and PR URLs involved plus how to proceed, so it is safe to return
// verbatim to a restricted session caller.
type OpenPRError struct {
	Verb string // "archive" | "delete"
	ID   string
	PRs  []OwnedPR
}

// OwnedPR is an open pull request and the session whose branch heads it.
type OwnedPR struct {
	SessionID string
	ghpr.PR
}

func (e *OpenPRError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "refusing to %s %s: it still owns %d open pull request(s):\n", e.Verb, e.ID, len(e.PRs))
	for _, pr := range e.PRs {
		draft := ""
		if pr.IsDraft {
			draft = " (draft)"
		}
		fmt.Fprintf(&b, "  #%d %s%s  [%s on %s]\n", pr.Number, pr.URL, draft, pr.SessionID, pr.HeadRefName)
	}
	b.WriteString("  finish them first: babysit each PR until it merges, or close it. To proceed anyway\n")
	b.WriteString("  (the PR stays open with no agent behind it), the host or the workgroup coordinator can force:\n")
	if e.Verb == "delete" {
		fmt.Fprintf(&b, "    amux workgroup rm %s --force", e.ID)
	} else {
		fmt.Fprintf(&b, "    amux workgroup archive %s --force   (or: amux do set-archived %s -f archived=true -f force=true)", e.ID, e.ID)
	}
	return b.String()
}

// guardedVerb reports which guarded verb a is ("" when unguarded). Restore,
// toggle-archive (the host TUI's one key), and every other verb are not
// guarded; neither is a forced request.
func guardedVerb(a core.Action) string {
	if a.Fields[FieldForce] == "true" {
		return ""
	}
	switch a.Action {
	case core.ActionSetArchived:
		if a.Fields["archived"] == "true" {
			return "archive"
		}
	case core.ActionDelete, core.ActionKill:
		return "delete"
	}
	return ""
}

// GuardOpenPRs refuses to archive or delete a session that still owns an open
// pull request: an agent whose branch (amux/<wg>-<member>, or a suffixed sibling
// such as amux/<wg>-<member>-b) heads one, or a workgroup any of whose members
// does. It fails open: an unreadable store row, an untracked or non-GitHub repo,
// and any gh failure or timeout are logged and allowed, so a GitHub outage
// never blocks archival.
func GuardOpenPRs(ctx context.Context, a core.Action) error {
	verb := guardedVerb(a)
	if verb == "" || a.ID == "" {
		return nil
	}
	prs, err := openPRsFor(ctx, a.ID)
	if err != nil {
		log.Printf("open-PR guard for %s %s skipped: %v", a.Action, a.ID, err)
		return nil
	}
	if len(prs) == 0 {
		return nil
	}
	return &OpenPRError{Verb: verb, ID: a.ID, PRs: prs}
}

type prQuery struct{ session, repo, branch string }

func openPRsFor(ctx context.Context, id string) ([]OwnedPR, error) {
	db, err := store.OpenReadOnly()
	if err != nil {
		return nil, err
	}
	s, ok, err := db.GetSession(id)
	if err != nil || !ok {
		db.Close()
		return nil, err // missing: let the action report it
	}
	sessions := []store.Session{s}
	if s.IsRoot() {
		children, err := db.Children(id)
		if err != nil {
			db.Close()
			return nil, err
		}
		sessions = append(sessions, children...)
	}
	var queries []prQuery
	seen := map[prQuery]bool{}
	for _, m := range sessions {
		// Only a stored branch is trusted. A blank one (legacy import) would
		// mean amux/<root>, a prefix of every sibling's branch.
		if m.Branch == "" || !strings.HasPrefix(m.Branch, core.BranchPrefix) {
			continue
		}
		for _, name := range store.SplitRepos(m.Repo) {
			repo, ok, err := db.Repo(name)
			if err != nil || !ok {
				continue
			}
			slug, ok := ghpr.Slug(repo.Source)
			if !ok {
				continue
			}
			q := prQuery{session: m.ID, repo: slug, branch: m.Branch}
			if !seen[q] {
				seen[q] = true
				queries = append(queries, q)
			}
		}
	}
	db.Close()
	if len(queries) == 0 {
		return nil, nil
	}

	ctx, cancel := context.WithTimeout(ctx, prGuardBudget)
	defer cancel()
	var (
		mu  sync.Mutex
		out []OwnedPR
		wg  sync.WaitGroup
	)
	for _, q := range queries {
		wg.Add(1)
		go func(q prQuery) {
			defer wg.Done()
			prs, err := PRLister.OpenPRs(ctx, q.repo, q.branch)
			if err != nil {
				log.Printf("open-PR guard: %s %s on %s: %v (allowing)", q.session, q.branch, q.repo, err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, pr := range prs {
				out = append(out, OwnedPR{SessionID: q.session, PR: pr})
			}
		}(q)
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool {
		if out[i].URL != out[j].URL {
			return out[i].URL < out[j].URL
		}
		return out[i].SessionID < out[j].SessionID
	})
	return out, nil
}
