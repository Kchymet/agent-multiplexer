package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"amux/internal/core"
	"amux/internal/sessionrpc"
)

func cmdAgentEvents(args []string) error {
	var target, cursor, after string
	var cursorSet, afterSet bool
	asJSON := false
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--json":
			asJSON = true
		case args[i] == "--cursor" && i+1 < len(args):
			if cursorSet {
				return fmt.Errorf("--cursor may be specified only once")
			}
			cursorSet = true
			cursor = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--cursor="):
			if cursorSet {
				return fmt.Errorf("--cursor may be specified only once")
			}
			cursorSet = true
			cursor = strings.TrimPrefix(args[i], "--cursor=")
		case args[i] == "--after" && i+1 < len(args):
			if afterSet {
				return fmt.Errorf("--after may be specified only once")
			}
			afterSet = true
			after = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--after="):
			if afterSet {
				return fmt.Errorf("--after may be specified only once")
			}
			afterSet = true
			after = strings.TrimPrefix(args[i], "--after=")
		case strings.HasPrefix(args[i], "-"):
			return fmt.Errorf("unknown events option %q", args[i])
		case target == "":
			target = args[i]
		default:
			return fmt.Errorf("usage: amux agent events [<session-id>] [--after <sequence> | --cursor <token>] [--json]")
		}
	}
	if cursorSet && afterSet {
		return fmt.Errorf("--cursor and --after are mutually exclusive")
	}
	if cursorSet && !canonicalRuntimeEventCursor(cursor) {
		return fmt.Errorf("--cursor must be a non-empty canonical token")
	}
	if afterSet {
		value, err := strconv.ParseInt(after, 10, 64)
		if err != nil || value < 0 || strings.TrimSpace(after) != after {
			return fmt.Errorf("--after must be a non-negative integer")
		}
	}
	if target == "" {
		ctx, err := loadAgentSessionContext()
		if err != nil || strings.TrimSpace(ctx.SubjectID) == "" {
			return fmt.Errorf("%s", notInsideAgent("amux agent events", "amux agent events <session-id>"))
		}
		target = ctx.SubjectID
	}
	fields := make(map[string]string)
	if cursorSet {
		fields[core.RuntimeEventsCursorField] = cursor
	}
	if afterSet {
		fields[core.RuntimeEventsAfterSequenceField] = after
	}
	var page core.RuntimeEventPage
	if err := restrictedQueryRequest(sessionrpc.Query{
		Verb: core.QueryRuntimeEvents, ID: target, Fields: fields,
	}, &page); err != nil {
		return fmt.Errorf("query runtime events for %s: %w", target, err)
	}
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(page)
	}
	for _, item := range page.Events {
		payload := strings.TrimSpace(string(item.Event.Payload))
		if payload == "" {
			payload = "{}"
		}
		fmt.Printf("%d\t%s\t%s\t%s\n", item.Sequence, item.Event.Type, item.Event.Direction, payload)
	}
	if page.Oversized != nil {
		switch page.Oversized.Kind {
		case "raw_record":
			fmt.Printf("%d\t[omitted raw record: %d bytes; %s]\n",
				page.Oversized.Sequence, page.Oversized.RawBytes, page.Oversized.Reason)
		default:
			fmt.Printf("%d\t[omitted %s event: %d encoded bytes; %s]\n",
				page.Oversized.Sequence, page.Oversized.Type, page.Oversized.EncodedBytes, page.Oversized.Reason)
		}
	}
	if page.NextCursor != "" {
		fmt.Printf("next cursor: %s\n", page.NextCursor)
	}
	return nil
}

func canonicalRuntimeEventCursor(cursor string) bool {
	if cursor == "" || strings.TrimSpace(cursor) != cursor ||
		len(cursor) != base64.RawURLEncoding.EncodedLen(sha256.Size) {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(cursor)
	return err == nil && len(decoded) == sha256.Size && base64.RawURLEncoding.EncodeToString(decoded) == cursor
}
