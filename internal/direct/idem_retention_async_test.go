package direct

import (
	"fmt"
	"net/http"
	"testing"
)

// Async creation after result retention: the status GET answers 404 (the
// documented expiry behavior, now as soon as retention passes rather than
// after the next admission prunes), and an async or sync POST of the same key
// and body answers the terminal 409 idem_expired. It never re-runs, never
// reports ready with a stale id, and never reports a transient 503. With the
// tombstone set full (cap 0) the record is kept whole and answers the same.
func TestAsyncCreationAfterRetention(t *testing.T) {
	for _, spentCap := range []int{16384, 0} {
		t.Run(fmt.Sprint("cap=", spentCap), func(t *testing.T) {
			old := maxSpentPerGrant
			maxSpentPerGrant = spentCap
			t.Cleanup(func() { maxSpentPerGrant = old })
			r := newRetentionRig(t)
			fields := map[string]string{"agent": "claude"}
			code, st, ec := postAsync(r.s, r.tok.AccessToken, "wg1", VerbAddAgent, fields, "k")
			if code != http.StatusAccepted || st.CreationID == "" {
				t.Fatalf("start: %d %+v %s", code, st, ec)
			}
			for st.Phase != CreatePhaseReady {
				code, st, ec = getStatus(r.s, r.tok.AccessToken, st.CreationID, fmt.Sprintf("wait=5&after=%d", st.Version))
				if code != http.StatusOK || st.Phase == CreatePhaseFailed {
					t.Fatalf("status: %d %+v %s", code, st, ec)
				}
			}
			id := st.CreationID
			r.past()
			r.refresh()
			// No admission in between: GET alone already answers 404.
			if code, _, ec := getStatus(r.s, r.tok.AccessToken, id, ""); code != http.StatusNotFound || ec != CodeNotFound {
				t.Fatalf("status past retention: %d %s", code, ec)
			}
			if code, _, ec := postAsync(r.s, r.tok.AccessToken, "wg1", VerbAddAgent, fields, "k"); code != http.StatusConflict || ec != CodeIdemExpired {
				t.Fatalf("async replay: %d %s", code, ec)
			}
			if code, ec := r.post("wg1", VerbAddAgent, fields, "k"); code != http.StatusConflict || ec != CodeIdemExpired {
				t.Fatalf("sync replay: %d %s", code, ec)
			}
			if code, _, ec := postAsync(r.s, r.tok.AccessToken, "wg1", VerbAddAgent, map[string]string{"agent": "codex"}, "k"); code != http.StatusUnprocessableEntity || ec != CodeIdemConflict {
				t.Fatalf("async changed body: %d %s", code, ec)
			}
			if code, _, _ := getStatus(r.s, r.tok.AccessToken, id, ""); code != http.StatusNotFound {
				t.Fatalf("status after compaction: %d", code)
			}
			if n := r.f.applies.Load(); n != 1 {
				t.Fatalf("daemon called %d times", n)
			}
		})
	}
}
