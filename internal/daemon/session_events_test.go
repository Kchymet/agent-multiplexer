package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"amux/internal/access"
	"amux/internal/claudecfg"
	"amux/internal/core"
	"amux/internal/hostprep"
	"amux/internal/runtimeevents"
	"amux/internal/sessionrpc"
	"amux/internal/store"

	"github.com/google/uuid"
	"github.com/kchymet/agent-multiplexer/harnessproto"
)

func TestSessionEventPagerStableMultiSourcePagesAndAfterSequence(t *testing.T) {
	root := t.TempDir()
	writeStructuredEvents(t, filepath.Join(root, "runtime.jsonl"), 260, 24)
	writeJournalEvents(t, filepath.Join(root, "journal.jsonl"), 2)
	set := testEventSourceSet(root,
		testEventSource(runtimeevents.PageSourceStructured, root, "runtime.jsonl"),
		testEventSource(runtimeevents.PageSourceJournal, root, "journal.jsonl"),
	)
	pager := testSessionEventPager(t, set)
	principal := testEventPrincipal("reader")

	body, err := pager.page(context.Background(), principal, set.target,
		map[string]string{core.RuntimeEventsAfterSequenceField: "260"})
	if err != nil {
		t.Fatal(err)
	}
	var got []core.SequencedRuntimeEvent
	for pages := 0; pages < 8; pages++ {
		page := decodeEventPage(t, body)
		if len(body) > sessionrpc.MaxResponseBody {
			t.Fatalf("page bytes = %d", len(body))
		}
		got = append(got, page.Events...)
		if !page.HasMore {
			break
		}
		body, err = pager.page(context.Background(), principal, set.target,
			map[string]string{core.RuntimeEventsCursorField: page.NextCursor})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(got) != 2 || got[0].Sequence != 261 || got[1].Sequence != 262 {
		t.Fatalf("events = %+v", got)
	}
	if got[0].Event.Type != harnessproto.TypeNotice || got[1].Event.Type != harnessproto.TypeNotice {
		t.Fatalf("journal events = %+v", got)
	}
}

func TestSessionEventPagerCursorRetryAndAppend(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "runtime.jsonl")
	writeStructuredEvents(t, path, 300, 32)
	set := testEventSourceSet(root, testEventSource(runtimeevents.PageSourceStructured, root, "runtime.jsonl"))
	pager := testSessionEventPager(t, set)
	principal := testEventPrincipal("reader")

	firstBody, err := pager.page(context.Background(), principal, set.target, nil)
	if err != nil {
		t.Fatal(err)
	}
	first := decodeEventPage(t, firstBody)
	if !first.HasMore || first.NextCursor == "" {
		t.Fatalf("first page = %+v", first)
	}
	secondBody, err := pager.page(context.Background(), principal, set.target,
		map[string]string{core.RuntimeEventsCursorField: first.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	retryBody, err := pager.page(context.Background(), principal, set.target,
		map[string]string{core.RuntimeEventsCursorField: first.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if string(secondBody) != string(retryBody) {
		t.Fatal("same cursor did not return the same page")
	}
	second := decodeEventPage(t, secondBody)
	pager.mu.Lock()
	secondEntry := pager.entries[second.NextCursor]
	pager.mu.Unlock()
	if err := validateCursorSources(secondEntry, set); err != nil {
		t.Fatalf("cursor invalid before append: %v", err)
	}
	appendStructuredEvent(t, path, 301, 32)
	if err := validateCursorSources(secondEntry, set); err != nil {
		t.Fatalf("cursor invalid after append validation: %v", err)
	}
	if _, err := pager.page(context.Background(), principal, set.target,
		map[string]string{core.RuntimeEventsCursorField: second.NextCursor}); err != nil {
		pager.mu.Lock()
		_, present := pager.entries[second.NextCursor]
		pager.mu.Unlock()
		t.Fatalf("append invalidated cursor: %v (first=%q second=%q present=%v)", err, first.NextCursor, second.NextCursor, present)
	}
}

func TestSessionEventPagerRejectsReplacementAndWrongSubject(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "runtime.jsonl")
	writeStructuredEvents(t, path, 300, 16)
	set := testEventSourceSet(root, testEventSource(runtimeevents.PageSourceStructured, root, "runtime.jsonl"))
	pager := testSessionEventPager(t, set)
	firstBody, err := pager.page(context.Background(), testEventPrincipal("reader"), set.target, nil)
	if err != nil {
		t.Fatal(err)
	}
	cursor := decodeEventPage(t, firstBody).NextCursor
	if _, err := pager.page(context.Background(), testEventPrincipal("other"), set.target,
		map[string]string{core.RuntimeEventsCursorField: cursor}); !errors.Is(err, errEventCursorInvalid) {
		t.Fatalf("wrong-subject cursor error = %v", err)
	}
	replacement := filepath.Join(root, "replacement")
	writeStructuredEvents(t, replacement, 300, 16)
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if _, err := pager.page(context.Background(), testEventPrincipal("reader"), set.target,
		map[string]string{core.RuntimeEventsCursorField: cursor}); !errors.Is(err, errEventCursorInvalid) {
		t.Fatalf("replacement cursor error = %v", err)
	}
}

func TestSessionEventPagerReportsRawRecordAndNormalizedEventOversizeSeparately(t *testing.T) {
	t.Run("raw record", func(t *testing.T) {
		root := t.TempDir()
		rawBytes := eventPageReadBytes*2 + 17
		if err := os.WriteFile(filepath.Join(root, "runtime.jsonl"),
			append([]byte(strings.Repeat("x", rawBytes-1)), '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		set := testEventSourceSet(root, testEventSource(runtimeevents.PageSourceStructured, root, "runtime.jsonl"))
		pager := testSessionEventPager(t, set)
		principal := testEventPrincipal("reader")
		body, err := pager.page(context.Background(), principal, set.target, nil)
		if err != nil {
			t.Fatal(err)
		}
		page := decodeEventPage(t, body)
		for attempts := 0; page.Oversized == nil && attempts < 4; attempts++ {
			body, err = pager.page(context.Background(), principal, set.target,
				map[string]string{core.RuntimeEventsCursorField: page.NextCursor})
			if err != nil {
				t.Fatal(err)
			}
			page = decodeEventPage(t, body)
		}
		if page.Oversized == nil || page.Oversized.Kind != "raw_record" || page.Oversized.RawBytes != int64(rawBytes) ||
			page.Oversized.Type != "" || page.Oversized.EncodedBytes != 0 {
			t.Fatalf("oversized = %+v", page.Oversized)
		}
	})

	t.Run("normalized event", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "runtime.jsonl")
		writeStructuredEvents(t, path, 1, eventRawRecordBytes-250)
		set := testEventSourceSet(root, testEventSource(runtimeevents.PageSourceStructured, root, "runtime.jsonl"))
		page := firstTestEventPage(t, set)
		if page.Oversized == nil || page.Oversized.Kind != "event" || page.Oversized.Type != harnessproto.TypeText ||
			page.Oversized.EncodedBytes == 0 || page.Oversized.RawBytes != 0 {
			t.Fatalf("oversized = %+v", page.Oversized)
		}
	})
}

func TestSessionEventPagerBoundsAdmissionExpiryAndCancellation(t *testing.T) {
	root := t.TempDir()
	writeStructuredEvents(t, filepath.Join(root, "runtime.jsonl"), 300, 8)
	set := testEventSourceSet(root, testEventSource(runtimeevents.PageSourceStructured, root, "runtime.jsonl"))
	pager := testSessionEventPager(t, set)
	now := time.Unix(100, 0)
	pager.now = func() time.Time { return now }
	principal := testEventPrincipal("reader")
	var cursors []string
	for after := 0; after < eventCursorPerSubject+3; after++ {
		body, err := pager.page(context.Background(), principal, set.target,
			map[string]string{core.RuntimeEventsAfterSequenceField: strconvForTest(after)})
		if err != nil {
			t.Fatal(err)
		}
		cursors = append(cursors, decodeEventPage(t, body).NextCursor)
	}
	pager.mu.Lock()
	count := 0
	for _, entry := range pager.entries {
		if entry.subject == principal.SubjectID {
			count++
		}
	}
	pager.mu.Unlock()
	if count > eventCursorPerSubject {
		t.Fatalf("subject cursor count = %d", count)
	}
	evicted := 0
	for _, cursor := range cursors {
		if _, err := pager.page(context.Background(), principal, set.target,
			map[string]string{core.RuntimeEventsCursorField: cursor}); errors.Is(err, errEventCursorInvalid) {
			evicted++
		}
	}
	if evicted < len(cursors)-eventCursorPerSubject {
		t.Fatalf("evicted cursors = %d, want at least %d", evicted, len(cursors)-eventCursorPerSubject)
	}

	body, err := pager.page(context.Background(), principal, set.target, nil)
	if err != nil {
		t.Fatal(err)
	}
	expiring := decodeEventPage(t, body).NextCursor
	now = now.Add(eventCursorTTL + time.Second)
	if _, err := pager.page(context.Background(), principal, set.target,
		map[string]string{core.RuntimeEventsCursorField: expiring}); !errors.Is(err, errEventCursorInvalid) {
		t.Fatalf("expired cursor error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	pager.mu.Lock()
	before := len(pager.entries)
	pager.mu.Unlock()
	if _, err := pager.page(ctx, principal, set.target, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v", err)
	}
	pager.mu.Lock()
	afterCancel := len(pager.entries)
	pager.mu.Unlock()
	if afterCancel != before {
		t.Fatalf("cancel changed cache entries: %d -> %d", before, afterCancel)
	}
}

func TestSessionEventPagerFinalReleaseCheckRunsAfterIOAndDropsDeniedPage(t *testing.T) {
	root := t.TempDir()
	writeStructuredEvents(t, filepath.Join(root, "runtime.jsonl"), 1, 8)
	set := testEventSourceSet(root, testEventSource(runtimeevents.PageSourceStructured, root, "runtime.jsonl"))
	resolved := false
	pager, err := newSessionEventPager(func(ctx context.Context, target string) (sessionEventSourceSet, error) {
		resolved = true
		return set, ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pager.close)
	releaseCalls := 0
	body, err := pager.pageForRelease(context.Background(), testEventPrincipal("reader"), set.target, nil,
		func(context.Context) error {
			releaseCalls++
			if !resolved {
				t.Fatal("final scope check ran before source resolution and page I/O")
			}
			pager.mu.Lock()
			prepared := len(pager.entries) != 0
			pager.mu.Unlock()
			if !prepared {
				t.Fatal("final scope check ran before the bounded page was prepared")
			}
			return access.ErrDenied
		})
	if !errors.Is(err, access.ErrDenied) || body != nil || releaseCalls != 1 {
		t.Fatalf("release result body=%q err=%v calls=%d", body, err, releaseCalls)
	}
	if body, err := pager.pageForRelease(context.Background(), testEventPrincipal("reader"), set.target, nil, nil); !errors.Is(err, access.ErrDenied) || body != nil {
		t.Fatalf("nil release check body=%q err=%v", body, err)
	}
}

func TestSessionEventPagerRejectsGrowingDecoderState(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "runtime.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	enc := json.NewEncoder(f)
	for i := 0; i < 8; i++ {
		arguments, _ := json.Marshal(strings.Repeat("x", 48<<10))
		line := map[string]any{
			"type": "response_item",
			"payload": map[string]any{
				"type": "function_call", "name": "tool", "call_id": "call-" + strconv.Itoa(i),
				"arguments": json.RawMessage(arguments),
			},
		}
		if err := enc.Encode(line); err != nil {
			t.Fatal(err)
		}
	}
	_ = f.Close()
	set := testEventSourceSet(root, testEventSource(runtimeevents.PageSourceCodex, root, "runtime.jsonl"))
	pager := testSessionEventPager(t, set)
	principal := testEventPrincipal("reader")
	fields := map[string]string(nil)
	var stateErr error
	for pageNumber := 0; pageNumber < 10; pageNumber++ {
		body, err := pager.page(context.Background(), principal, set.target, fields)
		if err != nil {
			stateErr = err
			break
		}
		page := decodeEventPage(t, body)
		fields = map[string]string{core.RuntimeEventsCursorField: page.NextCursor}
	}
	if !errors.Is(stateErr, errEventStateTooLarge) {
		t.Fatalf("decoder state error = %v", stateErr)
	}
}

func TestSessionEventFieldsRejectAmbiguousOrMalformedValues(t *testing.T) {
	bad := []map[string]string{
		{"other": "1"},
		{core.RuntimeEventsCursorField: strings.Repeat("a", 43), core.RuntimeEventsAfterSequenceField: "1"},
		{core.RuntimeEventsAfterSequenceField: "-1"},
		{core.RuntimeEventsAfterSequenceField: " 1"},
		{core.RuntimeEventsCursorField: "not-a-cursor"},
	}
	for _, fields := range bad {
		if _, _, err := parseSessionEventFields(fields); err == nil {
			t.Fatalf("fields accepted: %#v", fields)
		}
	}
}

func TestTrackedSessionEventSourcesNeverAdoptGlobalTranscript(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	session := store.Session{
		ID: "member", RootID: "root", Agent: harnessproto.RuntimeClaude,
		Dir: store.AgentDir("root", "member"), ClaudeID: id,
	}
	if err := os.MkdirAll(session.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := db.PutSession(session); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// A matching UUID in the user's global home is not a source for this tracked
	// subject. Only its private harness home or daemon-owned journals qualify.
	global := claudecfg.User().TranscriptPath(session.Dir, id)
	writeStructuredEvents(t, global, 1, 8)
	d := New("", nil, time.Hour)
	set, err := d.resolveTrackedSessionEventSources(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range set.sources {
		if source.kind == runtimeevents.PageSourceClaude {
			t.Fatalf("adopted global source: %+v", source)
		}
	}

	privateHome := claudecfg.AgentHome(session.Dir)
	private := claudecfg.At(privateHome).TranscriptPath(session.Dir, id)
	writeStructuredEvents(t, private, 1, 8)
	set, err = d.resolveTrackedSessionEventSources(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	foundPrivate := false
	for _, source := range set.sources {
		if source.kind == runtimeevents.PageSourceClaude {
			foundPrivate = true
			if source.root != session.Dir || filepath.Join(source.root, source.rel) != private {
				t.Fatalf("private source = %+v, want %s", source, private)
			}
		}
	}
	if !foundPrivate {
		t.Fatal("private transcript not resolved")
	}

	if err := os.Remove(private); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(global, private); err != nil {
		t.Fatal(err)
	}
	if _, err := d.resolveTrackedSessionEventSources(context.Background(), session.ID); err == nil || !hostprep.IsUnsafe(err) {
		t.Fatalf("private symlink source error = %v", err)
	}
	if _, err := d.resolveTrackedSessionEventSources(context.Background(), "missing"); !errors.Is(err, access.ErrDenied) {
		t.Fatalf("missing tracked source error = %v", err)
	}
}

func TestTrackedSessionJournalPagesThroughPager(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	session := store.Session{ID: "archived-member", RootID: "root", Agent: harnessproto.RuntimeClaude,
		Dir: store.AgentDir("root", "archived-member"), Archived: true}
	if err := os.MkdirAll(session.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := db.PutSession(session); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if err := core.AppendJournal(session.ID, core.JournalInfo, "bounded handoff"); err != nil {
		t.Fatal(err)
	}
	d := New("", nil, time.Hour)
	pager, err := newDaemonSessionEventPager(d)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pager.close)
	body, err := pager.page(context.Background(), testEventPrincipal("coordinator"), session.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	page := decodeEventPage(t, body)
	if len(page.Events) != 1 || page.Events[0].Event.Type != harnessproto.TypeNotice ||
		!strings.Contains(string(page.Events[0].Event.Payload), "bounded handoff") {
		t.Fatalf("journal page = %+v", page)
	}
}

func testSessionEventPager(t *testing.T, set sessionEventSourceSet) *sessionEventPager {
	t.Helper()
	pager, err := newSessionEventPager(func(ctx context.Context, target string) (sessionEventSourceSet, error) {
		if err := ctx.Err(); err != nil {
			return sessionEventSourceSet{}, err
		}
		if target != set.target {
			return sessionEventSourceSet{}, access.ErrDenied
		}
		return set, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pager.close)
	return pager
}

func testEventSourceSet(root string, sources ...sessionEventSource) sessionEventSourceSet {
	return sessionEventSourceSet{
		target: "target", runtime: harnessproto.RuntimeCodex,
		signature: sha256.Sum256([]byte(root)), sources: sources,
	}
}

func testEventSource(kind runtimeevents.PageSourceKind, root, rel string) sessionEventSource {
	return sessionEventSource{kind: kind, root: root, rel: rel}
}

func testEventPrincipal(subject string) access.Principal {
	return access.Principal{Kind: access.SubjectSession, SubjectID: subject, KeyID: "key", Generation: 1}
}

func writeStructuredEvents(t *testing.T, path string, count, textSize int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	enc := json.NewEncoder(f)
	for i := 1; i <= count; i++ {
		event := harnessproto.RuntimeEvent{
			Type: harnessproto.TypeText, Direction: harnessproto.DirOut,
			Payload: json.RawMessage(`{"text":"` + strings.Repeat("x", textSize) + `"}`),
		}
		if err := enc.Encode(event); err != nil {
			_ = f.Close()
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func appendStructuredEvent(t *testing.T, path string, _ int, textSize int) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	event := harnessproto.RuntimeEvent{Type: harnessproto.TypeText, Direction: harnessproto.DirOut,
		Payload: json.RawMessage(`{"text":"` + strings.Repeat("y", textSize) + `"}`)}
	err = json.NewEncoder(f).Encode(event)
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
}

func writeJournalEvents(t *testing.T, path string, count int) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	enc := json.NewEncoder(f)
	for i := 0; i < count; i++ {
		if err := enc.Encode(core.JournalRecord{Level: core.JournalInfo, Text: "journal"}); err != nil {
			t.Fatal(err)
		}
	}
	_ = f.Close()
}

func firstTestEventPage(t *testing.T, set sessionEventSourceSet) core.RuntimeEventPage {
	t.Helper()
	body, err := testSessionEventPager(t, set).page(context.Background(), testEventPrincipal("reader"), set.target, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) > sessionrpc.MaxResponseBody {
		t.Fatalf("page bytes = %d", len(body))
	}
	return decodeEventPage(t, body)
}

func decodeEventPage(t *testing.T, body []byte) core.RuntimeEventPage {
	t.Helper()
	var page core.RuntimeEventPage
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatal(err)
	}
	return page
}

func strconvForTest(value int) string { return strconv.Itoa(value) }
