package codexauth

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAPIKeyNativeOwnerReplacesAndRollsBack(t *testing.T) {
	p, home, stderr := newFake(t)
	login(t, p)
	before, err := p.Tokens(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	key := "synthetic-api-SECRET"
	if err := p.LoginAPIKey(ctxT(t), key, func() error { return errors.New("retired") }); err == nil {
		t.Fatal("retired login committed")
	}
	got, err := p.Tokens(ctxT(t))
	if err != nil || got.APIKey || got.AccountID != before.AccountID {
		t.Fatal("rollback did not restore subscription", err)
	}
	if err := p.LoginAPIKey(ctxT(t), key); err != nil {
		t.Fatal(err)
	}
	got, err = p.SessionToken(ctxT(t), "workload", false, "")
	if err != nil || !got.APIKey || got.AccessToken != key || got.AccountID != "" || got.PlanType != "" {
		t.Fatal("API credential invented identity", err)
	}
	if _, err = p.SessionToken(ctxT(t), "workload", false, "prior-account"); !errors.Is(err, ErrAccountChanged) {
		t.Fatal("subscription identity accepted API key", err)
	}
	inv, _ := os.ReadFile(filepath.Join(home, "fake-invocation.json"))
	for _, surface := range []string{string(inv), stderr.String(), fmt.Sprintf("%+v %#v", got, got)} {
		if strings.Contains(surface, key) {
			t.Fatal("API key leaked outside delivery/store")
		}
	}
	login(t, p)
	got, err = p.Tokens(ctxT(t))
	if err != nil || got.APIKey || got.AccountID == "" {
		t.Fatal("subscription replacement failed", err)
	}
}

func TestAPIKeyCommitCannotExposePriorCachedKey(t *testing.T) {
	p, _, _ := newFake(t)
	if err := p.LoginAPIKey(ctxT(t), "first-synthetic-key"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Tokens(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	if err := p.LoginAPIKey(ctxT(t), "second-synthetic-key", func() error {
		// This is the exact point the daemon makes the new epoch deliverable.
		if _, cached, err := p.cached(); err != nil || cached {
			t.Fatalf("prior key remained cached at commit: cached=%v err=%v", cached, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got, err := p.Tokens(ctxT(t))
	if err != nil || got.AccessToken != "second-synthetic-key" {
		t.Fatal("committed credential unavailable", err)
	}
}
