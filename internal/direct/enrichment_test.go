package direct

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"amux/internal/core"

	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

// enrichedBackend is a fake backend whose daemon publishes PR enrichments.
type enrichedBackend struct {
	*fakeBackend
	kinds []string
}

func (b enrichedBackend) EnrichmentKinds(context.Context) ([]string, error) { return b.kinds, nil }

func enrichedRows() []core.Session {
	enr := hp.Enrichments{{Kind: hp.EnrichmentPullRequest, ID: "o/r#79", Title: "chips", URL: "https://github.com/o/r/pull/79",
		Tone: hp.ToneOK, Label: "#79 ✓ approved", PullRequest: &hp.PullRequestEnrichment{Repo: "o/r", Number: 79, State: hp.PRStateOpen,
			Phase: hp.PRPhaseMerge, Attention: []string{}, CI: hp.PRCI{Status: hp.CIGreen, FailingChecks: []string{}},
			Review: hp.PRReviewSummary{State: hp.ReviewApproved, By: []string{"octocat"}, Requested: []string{}}}}}
	counts := hp.PullRequestCounts{Open: 1, WaitingMerge: 1}
	return []core.Session{
		{ID: "wg1", Title: "wg", IsRoot: true, Role: "coordinator", Section: core.SectionWorkgroups, State: core.StateReady,
			SubState: hp.SubStateWaitingMerge, Status: "waiting for merge · 1 PR",
			Rollup: &hp.Rollup{State: hp.RollupWaitingMerge, Count: 1, Label: "waiting for merge · 1 PR", Enrichments: hp.RollupEnrichments{PullRequest: &counts}}},
		{ID: "m1", Title: "m1", RootID: "wg1", Section: core.SectionWorkgroups, State: core.StateReady, Enrichments: &enr},
	}
}

// Rows carry sub_state, enrichments and rollup with snake_case nested keys,
// and the capabilities advertise the kinds only when the daemon publishes them.
func TestDirectEnrichments(t *testing.T) {
	f := newFake(enrichedRows()...)
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	s := newTestServer(t, a, enrichedBackend{fakeBackend: f, kinds: []string{hp.EnrichmentPullRequest}})
	tok := enroll(t, a, allPerms, allSessions)

	w := do(s, "GET", PathSession, tok.AccessToken, "")
	if w.Code != 200 {
		t.Fatalf("sessions %d %s", w.Code, w.Body)
	}
	body := w.Body.String()
	for _, want := range []string{`"sub_state":"waiting_merge"`, `"rollup":{"state":"waiting_merge"`, `"waiting_merge":1`,
		`"enrichments":[{"kind":"pull_request","id":"o/r#79"`, `"pull_request":{"repo":"o/r"`, `"review":{"state":"approved","by":["octocat"]`} {
		if !strings.Contains(body, want) {
			t.Errorf("sessions body lacks %s: %s", want, body)
		}
	}
	var list SessionsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}

	w = do(s, "GET", PathCapabilities, tok.AccessToken, "")
	if !strings.Contains(w.Body.String(), `"enrichments":{"version":1,"kinds":["pull_request"]}`) {
		t.Fatalf("capabilities: %s", w.Body)
	}

	// A daemon with PR watching off (no kinds), or a backend that cannot say,
	// advertises nothing.
	for _, b := range []Backend{enrichedBackend{fakeBackend: f}, f} {
		s := newTestServer(t, a, b)
		if w := do(s, "GET", PathCapabilities, tok.AccessToken, ""); strings.Contains(w.Body.String(), `"enrichments"`) {
			t.Fatalf("capabilities without enrichments: %s", w.Body)
		}
	}
}
