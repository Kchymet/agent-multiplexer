package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	err := restrictedQueryRequest(sessionrpc.Query{
		Verb: core.QueryRuntimeEvents, ID: target, Fields: fields,
	}, &page)
	if cursorSet && isEventCursorInvalid(err) {
		// Cursors live in daemon memory: they expire after five idle minutes,
		// are evicted under pressure, and die with a daemon restart or a source
		// change. A cursor this client was handed records where its page ended,
		// so resume the read from there instead of failing the caller.
		resume, ok := rememberedEventCursor(target, cursor)
		if !ok {
			return fmt.Errorf("query runtime events for %s: %w (the cursor expired; resume with --after <last sequence you processed>)", target, err)
		}
		fmt.Fprintf(os.Stderr, "amux: events cursor expired; resumed after sequence %d\n", resume)
		page = core.RuntimeEventPage{}
		err = restrictedQueryRequest(sessionrpc.Query{
			Verb: core.QueryRuntimeEvents, ID: target,
			Fields: map[string]string{core.RuntimeEventsAfterSequenceField: strconv.FormatInt(resume, 10)},
		}, &page)
	}
	if err != nil {
		return fmt.Errorf("query runtime events for %s: %w", target, err)
	}
	if page.NextCursor != "" {
		rememberEventCursor(target, page.NextCursor, page.ScannedSequence)
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

func isEventCursorInvalid(err error) bool {
	var remote *sessionrpc.RemoteError
	return errors.As(err, &remote) && remote.Code == "runtime_events_cursor_invalid"
}

// eventCursorMemory is how many issued cursors per target the client keeps.
const eventCursorMemory = 64

// rememberedCursor is one cursor this client printed and the last sequence its
// page had assigned, i.e. where an after_sequence read resumes.
type rememberedCursor struct {
	Cursor string `json:"cursor"`
	After  int64  `json:"after"`
}

var eventCursorDir = func() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "amux", "events")
}

func eventCursorPath(target string) string {
	dir := eventCursorDir()
	if dir == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(target))
	return filepath.Join(dir, hex.EncodeToString(sum[:8])+".json")
}

func readEventCursors(path string) []rememberedCursor {
	var cursors []rememberedCursor
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &cursors)
	}
	return cursors
}

// rememberEventCursor records a cursor this client was issued. Best-effort:
// failing to record only means a later expiry is reported instead of resumed.
func rememberEventCursor(target, cursor string, after int64) {
	path := eventCursorPath(target)
	if path == "" || after < 0 {
		return
	}
	cursors := []rememberedCursor{{Cursor: cursor, After: after}}
	for _, c := range readEventCursors(path) {
		if c.Cursor != cursor && len(cursors) < eventCursorMemory {
			cursors = append(cursors, c)
		}
	}
	b, err := json.Marshal(cursors)
	if err != nil || os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".cursors-*")
	if err != nil {
		return
	}
	_, werr := tmp.Write(b)
	if cerr := tmp.Close(); werr != nil || cerr != nil || os.Rename(tmp.Name(), path) != nil {
		_ = os.Remove(tmp.Name())
	}
}

func rememberedEventCursor(target, cursor string) (int64, bool) {
	path := eventCursorPath(target)
	if path == "" {
		return 0, false
	}
	for _, c := range readEventCursors(path) {
		if c.Cursor == cursor && c.After >= 0 {
			return c.After, true
		}
	}
	return 0, false
}
