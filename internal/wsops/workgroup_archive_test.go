package wsops

import (
	"context"
	"reflect"
	"testing"

	"amux/internal/core"
	"amux/internal/store"
)

func TestWorkgroupArchiveAndSelectiveRestore(t *testing.T) {
	for _, verb := range []string{core.ActionArchive, core.ActionSetArchived} {
		t.Run(verb, func(t *testing.T) {
			isolateStore(t)
			db, err := store.Open()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			sessions := []store.Session{
				{ID: "root", Scope: store.ScopeWork, Dir: store.CoordinatorDir("root"), ClaudeID: "coordinator-history"},
				{ID: "active", RootID: "root", Dir: "/preserved/member", Branch: "amux/root-active", ClaudeID: "member-history"},
				{ID: "done", RootID: "root", Archived: true, ArchivedAt: 123},
				{ID: "foreign", Scope: store.ScopeWork},
				{ID: "moved", RootID: "foreign"},
			}
			for _, s := range sessions {
				if err := db.PutSession(s); err != nil {
					t.Fatal(err)
				}
			}
			action := core.Action{Action: verb, ID: "root", Fields: map[string]string{"archived": "true"}}
			if _, err := Dispatch(context.Background(), action, nil); err != nil {
				t.Fatalf("archive: %v", err)
			}
			for _, original := range sessions {
				got := getSession(t, original.ID)
				want := original
				if original.ID == "root" || original.ID == "active" {
					want.Archived = true
					want.ArchivedAt = got.ArchivedAt
					if got.ArchivedAt == 0 {
						t.Errorf("%s has no archive timestamp", got.ID)
					}
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("archive changed unexpected state: got %+v want %+v", got, want)
				}
			}
			if err := SetArchived(context.Background(), "active", false); err == nil {
				t.Fatal("restored member under archived root")
			}
			if err := SetArchived(context.Background(), "root", false); err != nil {
				t.Fatal(err)
			}
			if getSession(t, "root").Archived {
				t.Fatal("root not restored")
			}
			for _, id := range []string{"active", "done"} {
				if !getSession(t, id).Archived {
					t.Fatalf("root restore revived member %s", id)
				}
			}
			if err := SetArchived(context.Background(), "active", false); err != nil {
				t.Fatal(err)
			}
			if getSession(t, "active").Archived || getSession(t, "active").ArchivedAt != 0 {
				t.Fatal("explicit member restore failed")
			}
			if got := getSession(t, "done"); !got.Archived || got.ArchivedAt != 123 {
				t.Fatalf("previously completed member changed: %+v", got)
			}
		})
	}
}
