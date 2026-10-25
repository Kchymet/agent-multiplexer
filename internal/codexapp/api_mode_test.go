package codexapp

import (
	"context"
	"encoding/json"
	"testing"
)

func TestAPIKeyInjectedWithoutSubscriptionIdentityOrEvents(t *testing.T) {
	sup, fs, client := newAuthPair(t, providerFunc(func(context.Context, TokenRequest) (ChatGPTToken, error) {
		return ChatGPTToken{AccessToken: firstToken, APIKey: true}, nil
	}))
	defer fs.close()
	defer sup.Close()
	attach(t, sup, client)
	login, ok := fs.sawCall(methodLoginStart)
	if !ok {
		t.Fatal("missing API login")
	}
	var params map[string]any
	if json.Unmarshal(login.Params, &params) != nil || params["type"] != "apiKey" || params["apiKey"] != firstToken || len(params) != 2 {
		t.Fatal("API login claimed subscription identity")
	}
	if sup.auth.boundAccount() != "" {
		t.Fatal("API login claimed account ID")
	}
	if _, err := sup.auth.refresh(t.Context()); err == nil {
		t.Fatal("API mode accepted ChatGPT refresh")
	}
	assertNoSecrets(t, sup)
}

func TestAPIKeyCannotClaimSubscriptionIdentity(t *testing.T) {
	for _, tok := range []ChatGPTToken{{AccessToken: firstToken, APIKey: true, AccountID: "invented"}, {AccessToken: firstToken, APIKey: true, PlanType: "pro"}} {
		auth := newTokenAuth(providerFunc(func(context.Context, TokenRequest) (ChatGPTToken, error) { return tok, nil }), "test", "")
		if _, err := auth.fetch(t.Context(), false, ""); err == nil {
			t.Fatal("API credential claimed subscription identity")
		}
	}
}
