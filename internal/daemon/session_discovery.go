package daemon

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"amux/internal/access"
	"amux/internal/agent"
	"amux/internal/core"
	"amux/internal/sessionrpc"
)

// Discovery is the deliberate cross-session read exception. Its cursor is only
// a lexical position in the listing; it is never opened as a path or interpreted
// as a session identity. No management query or write policy is relaxed.
func decodeDiscoveryCursor(cursor string) (string, error) {
	if cursor == "" {
		return "", nil
	}
	if len(cursor) > 16<<10 {
		return "", fmt.Errorf("discovery cursor too large")
	}
	key, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || base64.RawURLEncoding.EncodeToString(key) != cursor || strings.Count(string(key), "\x00") != 1 {
		return "", fmt.Errorf("invalid discovery cursor")
	}
	return string(key), nil
}

func discoveryKey(row core.AgentSessionRow) string { return row.Harness + "\x00" + row.Path }

func discoveryPage(rows []core.AgentSessionRow, cursor string) ([]byte, error) {
	after, err := decodeDiscoveryCursor(cursor)
	if err != nil {
		return nil, err
	}
	sort.Slice(rows, func(i, j int) bool { return discoveryKey(rows[i]) < discoveryKey(rows[j]) })
	page := core.AgentSessionsPage{}
	size := 64 // envelope, commas, and field names
	for _, row := range rows {
		key := discoveryKey(row)
		if key <= after {
			continue
		}
		encoded, err := json.Marshal(row)
		if err != nil {
			return nil, err
		}
		next := base64.RawURLEncoding.EncodeToString([]byte(key))
		if len(next) > 16<<10 || len(encoded)+len(next)+64 > sessionrpc.MaxResponseBody {
			return nil, fmt.Errorf("discovery row exceeds transport bound")
		}
		if size+len(encoded)+len(next) > sessionrpc.MaxResponseBody {
			return json.Marshal(page)
		}
		size += len(encoded) + 1
		page.Sessions = append(page.Sessions, row)
		page.NextCursor = next
	}
	page.NextCursor = ""
	return json.Marshal(page)
}

func (r *sessionRuntime) dispatchAgentSessions(ctx context.Context, principal access.Principal, req access.Request) sessionrpc.DispatchResult {
	if err := r.withFinalAdmission(ctx, principal, req); err != nil {
		return rpcDenied("access_denied")
	}
	// Source I/O must not hold the lifecycle effect gate. The listing is live,
	// like directory enumeration: additions before the cursor appear next time.
	body, err := discoveryPage(agent.ListSessionRows(), req.Fields["cursor"])
	if err != nil {
		return rpcFailed("agent_discovery_failed")
	}
	if err := r.withFinalAdmission(ctx, principal, req); err != nil {
		return rpcDenied("access_denied")
	}
	return sessionrpc.DispatchResult{Status: sessionrpc.StatusOK, Body: body}
}
