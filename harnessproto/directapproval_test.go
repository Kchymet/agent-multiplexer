package harnessproto

import (
	"encoding/json"
	"strings"
	"testing"
)

// The direct-approval-v1 wire names are a cross-repo contract (Harness and
// Android implement them byte for byte): pin them.
func TestDirectApprovalWireNames(t *testing.T) {
	approval := DirectApprovalPrefix + strings.Repeat("A", 43)
	js := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	for _, c := range []struct {
		got, want string
	}{
		{js(HarnessMsg{Type: HDirectApprovalVerify, ReqID: "r1", DirectApproval: &DirectApprovalVerify{Approval: approval, ServerID: "sid", CodeChallenge: "ch"}}),
			`"type":"direct-approval-verify"`},
		{js(HarnessMsg{Type: HDirectApprovalVerify, ReqID: "r1", DirectApproval: &DirectApprovalVerify{Approval: approval, ServerID: "sid", CodeChallenge: "ch"}}),
			`"reqId":"r1"`},
		{js(HarnessMsg{DirectApproval: &DirectApprovalVerify{Approval: approval, ServerID: "sid", CodeChallenge: "ch"}}),
			`"directApproval":{"approval":"` + approval + `","serverId":"sid","codeChallenge":"ch"}`},
		{js(MuxMsg{Type: MDirectApprovalResult, ReqID: "r1", DirectApprovalResult: &DirectApprovalResult{Requester: "u", Permissions: []string{"read", "steer"},
			Resource: &DirectResource{Kind: DirectResourceWorkgroups, Workgroups: []string{"771951"}}, Label: "Chrome", Handoff: DirectHandoffDeviceTicket, ExpiresAt: 1793000300}}),
			`"type":"direct-approval-result"`},
		{js(MuxMsg{DirectApprovalResult: &DirectApprovalResult{Requester: "u", Permissions: []string{"read", "steer"},
			Resource: &DirectResource{Kind: DirectResourceWorkgroups, Workgroups: []string{"771951"}}, Label: "Chrome", Handoff: DirectHandoffDeviceTicket, ExpiresAt: 1793000300}}),
			`"directApprovalResult":{"requester":"u","permissions":["read","steer"],"resource":{"kind":"workgroups","workgroups":["771951"]},"label":"Chrome","handoff":"device_ticket","expiresAt":1793000300}`},
		{js(MuxMsg{DirectApprovalResult: &DirectApprovalResult{Error: DirectApprovalErrUsed}}), `"directApprovalResult":{"error":"approval_used"}`},
		{js(MuxMsg{Type: MRegistered, WebOrigin: "https://harness.example.dev"}), `"webOrigin":"https://harness.example.dev"`},
		{js(DirectCapabilities{ServerID: "sid", Endpoints: []string{"https://h.example.ts.net:8443"}, Issuance: true, Handoff: true, Approval: true,
			Ceiling: &DirectCeiling{Permissions: []string{"read", "steer"}, Resource: DirectResourceAllSessions}}),
			`{"serverId":"sid","endpoints":["https://h.example.ts.net:8443"],"issuance":true,"handoff":true,"approval":true,"ceiling":{"permissions":["read","steer"],"resource":"all_sessions"}}`},
	} {
		if !strings.Contains(c.got, c.want) {
			t.Errorf("wire %s\n  lacks %s", c.got, c.want)
		}
	}
	if DirectApprovalV1 != "direct-approval-v1" {
		t.Fatal(DirectApprovalV1)
	}
	for code, want := range map[string]string{
		DirectApprovalErrUnknown: "approval_unknown", DirectApprovalErrExpired: "approval_expired", DirectApprovalErrUsed: "approval_used",
		DirectApprovalErrMismatch: "approval_mismatch", DirectApprovalErrOwnerChange: "owner_changed", DirectApprovalErrUnavailable: "unavailable",
	} {
		if code != want {
			t.Errorf("%s != %s", code, want)
		}
	}
	// Older hosts: nothing new on the wire.
	if got := js(DirectCapabilities{ServerID: "sid", Endpoints: []string{"https://e"}, Issuance: true}); strings.Contains(got, "approval") || strings.Contains(got, "ceiling") {
		t.Fatalf("absent fields leak: %s", got)
	}
}

func TestValidDirectApproval(t *testing.T) {
	for s, ok := range map[string]bool{
		DirectApprovalPrefix + strings.Repeat("A", 43):        true,
		DirectApprovalPrefix + strings.Repeat("-_", 21) + "a": true,
		DirectApprovalPrefix + strings.Repeat("A", 42):        false,
		DirectApprovalPrefix + strings.Repeat("A", 44):        false,
		DirectApprovalPrefix + strings.Repeat("A", 42) + "=":  false,
		"hdax_" + strings.Repeat("A", 43):                     false,
		"":                                                    false,
	} {
		if ValidDirectApproval(s) != ok {
			t.Errorf("ValidDirectApproval(%q) != %v", s, ok)
		}
	}
}
