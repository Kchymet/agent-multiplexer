package direct

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"amux/internal/core"
	"amux/internal/daemon"
)

// Interjection shares prompt's trust boundary but must retain its distinct
// daemon verb: the runtime owns queueing/steering and stopped-session refusal.
func TestInterjectActions(t *testing.T) {
	for _, tc := range []struct {
		name     string
		text     string
		fields   map[string]string
		reader   bool
		sid      string
		mutate   func([]core.Session)
		rejected bool
		status   int
		code     string
		applies  int64
	}{
		{name: "delivered once", text: "Also check this.\nKeep the original task.", status: http.StatusOK, applies: 1},
		{name: "interject capability independent of prompt", text: "next", mutate: func(rows []core.Session) { rows[1].Caps = &core.SessionCaps{Interject: true} }, status: http.StatusOK, applies: 1},
		{name: "read only", text: "next", reader: true, status: http.StatusForbidden, code: CodeDenied},
		{name: "out of scope", text: "next", sid: "d1", status: http.StatusNotFound, code: CodeNotFound},
		{name: "console", text: "next", sid: ConsoleID, status: http.StatusNotFound, code: CodeNotFound},
		{name: "archived target", text: "next", mutate: func(rows []core.Session) { rows[1].Archived = true }, status: http.StatusConflict, code: CodeArchived},
		{name: "archived parent", text: "next", mutate: func(rows []core.Session) { rows[0].Archived = true }, status: http.StatusConflict, code: CodeArchived},
		{name: "missing capability", text: "next", mutate: func(rows []core.Session) { rows[1].Caps = &core.SessionCaps{Prompt: true} }, status: http.StatusConflict, code: CodeUnsupportedVerb},
		{name: "nil capabilities", text: "next", mutate: func(rows []core.Session) { rows[1].Caps = nil }, status: http.StatusConflict, code: CodeUnsupportedVerb},
		{name: "empty text", status: http.StatusBadRequest, code: CodeBadRequest},
		{name: "blank text", text: " \n\t", status: http.StatusBadRequest, code: CodeBadRequest},
		{name: "oversized text", text: strings.Repeat("x", MaxPromptBytes+1), status: http.StatusBadRequest, code: CodeBadRequest},
		{name: "text at limit", text: strings.Repeat("x", MaxPromptBytes), status: http.StatusOK, applies: 1},
		{name: "extra field", fields: map[string]string{"text": "next", "decision": "allow"}, status: http.StatusBadRequest, code: CodeBadRequest},
		{name: "runtime refuses no active turn", text: "next", rejected: true, status: http.StatusUnprocessableEntity, code: CodeRejected, applies: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := testRows()
			if tc.mutate != nil {
				tc.mutate(rows)
			}
			f := newFake(rows...)
			if tc.rejected {
				f.applyErr = &daemon.RejectedError{Msg: "no active turn"}
			}
			a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
			s := newTestServer(t, a, f)
			perms := []string{PermRead, PermSteer}
			if tc.reader {
				perms = []string{PermRead}
			}
			tok := enroll(t, a, perms, ResourceScope{Kind: ResourceWorkgroups, Workgroups: []string{"wg1"}})
			sid := tc.sid
			if sid == "" {
				sid = "a1"
			}
			fields := tc.fields
			if fields == nil {
				fields = map[string]string{"text": tc.text}
			}
			req := ActionRequest{Verb: VerbInterject, Fields: fields, IdemKey: "interjection"}
			body := string(mustJSON(t, req))
			for range 2 {
				w := do(s, "POST", PathSession+"/"+sid+"/actions", tok.AccessToken, body)
				if w.Code != tc.status {
					t.Fatalf("status %d, want %d: %s", w.Code, tc.status, w.Body)
				}
				if tc.code != "" && apiCode(t, w) != tc.code {
					t.Fatalf("error: %s", w.Body)
				}
			}
			if f.applies.Load() != tc.applies {
				t.Fatalf("Apply calls=%d, want %d", f.applies.Load(), tc.applies)
			}
			if tc.reader || tc.status == http.StatusBadRequest {
				if f.total() != 0 {
					t.Fatal("invalid request reached backend")
				}
			}
			if tc.applies == 1 {
				got := <-f.applied
				want := core.Action{Action: core.ActionSteer, ID: sid, Fields: map[string]string{core.SteerVerb: core.SteerInterject, core.SteerText: tc.text}}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("unexpected daemon action: %+v", got)
				}
				// Reusing this key for a prompt must conflict, not start another turn.
				req.Verb = VerbPrompt
				w := do(s, "POST", PathSession+"/"+sid+"/actions", tok.AccessToken, string(mustJSON(t, req)))
				if apiCode(t, w) != CodeIdemConflict || f.applies.Load() != 1 {
					t.Fatalf("verb conflict: %d %s", w.Code, w.Body)
				}
			}
		})
	}
}

func TestInterjectCapabilityInventoryAndWatch(t *testing.T) {
	for _, reader := range []bool{false, true} {
		t.Run(map[bool]string{false: "steer", true: "read only"}[reader], func(t *testing.T) {
			rows := testRows()
			rows[1].Caps = &core.SessionCaps{Interject: true} // not inferred from Prompt
			rows[4].Caps = &core.SessionCaps{Prompt: true}    // Prompt does not imply Interject
			f := newFake(rows...)
			a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
			s := newTestServer(t, a, f)
			perms := []string{PermRead, PermSteer}
			if reader {
				perms = []string{PermRead}
			}
			tok := enroll(t, a, perms, allSessions)
			w := do(s, "GET", PathSession, tok.AccessToken, "")
			var inventory SessionsResponse
			if err := json.Unmarshal(w.Body.Bytes(), &inventory); err != nil || w.Code != 200 {
				t.Fatalf("inventory: %v %s", err, w.Body)
			}
			ts := httptest.NewServer(s)
			defer ts.Close()
			conn := dialWatch(t, ts, testOrigin)
			defer conn.Close()
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			if err := conn.WriteJSON(WatchAuth{Type: "auth", AccessToken: tok.AccessToken}); err != nil {
				t.Fatal(err)
			}
			var welcome WatchWelcome
			if err := conn.ReadJSON(&welcome); err != nil {
				t.Fatal(err)
			}
			var snap WatchSnapshot
			if err := conn.ReadJSON(&snap); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(inventory.Sessions, snap.Sessions) {
				t.Fatal("HTTP and watch inventory differ")
			}
			if len(inventory.Sessions) != 5 {
				t.Fatalf("inventory: %+v", inventory)
			}
			for _, row := range inventory.Sessions {
				want := !reader && (row.ID == "wg1" || row.ID == "a1")
				if row.Caps.Interject != want {
					t.Errorf("%s interject=%v, want %v", row.ID, row.Caps.Interject, want)
				}
			}
			if reader {
				return
			}
			// A fresh capability change must also reach an already connected client.
			rows = append([]core.Session(nil), rows...)
			rows[1].Caps = &core.SessionCaps{Prompt: true}
			f.setRows(rows...)
			if err := conn.ReadJSON(&snap); err != nil {
				t.Fatal(err)
			}
			for _, row := range snap.Sessions {
				if row.ID == "a1" && row.Caps.Interject {
					t.Fatal("watch retained revoked interject capability")
				}
			}
		})
	}
}
