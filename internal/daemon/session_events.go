package daemon

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"amux/internal/access"
	"amux/internal/agent"
	"amux/internal/claudecfg"
	"amux/internal/codexapp"
	"amux/internal/core"
	"amux/internal/hostprep"
	"amux/internal/runtimeevents"
	"amux/internal/sessionrpc"
	"amux/internal/store"

	"github.com/google/uuid"
	"github.com/kchymet/agent-multiplexer/harnessproto"
)

const (
	eventPageReadBytes       = 256 << 10
	eventPageRecords         = 2048
	eventPageEvents          = 256
	eventRawRecordBytes      = 64 << 10
	eventReadChunkBytes      = 256
	eventDecoderStateBytes   = 256 << 10
	eventCursorEntries       = 128
	eventCursorPerSubject    = 8
	eventCursorGlobalBytes   = 32 << 20
	eventCursorSubjectBytes  = 2 << 20
	eventCursorTTL           = 5 * time.Minute
	eventSourceWalkEntries   = 8192
	eventSourceAnchorBytes   = 32
	eventCursorEncodedLength = 43 // base64url SHA-256
)

var (
	errEventCursorInvalid = errors.New("runtime event cursor is invalid or expired")
	errEventQueryInvalid  = errors.New("runtime event query is invalid")
	errEventStateTooLarge = errors.New("runtime event decoder state limit exceeded")
)

type sessionEventSource struct {
	kind runtimeevents.PageSourceKind
	root string
	rel  string
}

type sessionEventSourceSet struct {
	target    string
	runtime   string
	signature [sha256.Size]byte
	sources   []sessionEventSource
	bindings  map[string]string
}

type sessionEventSourceResolver func(context.Context, string) (sessionEventSourceSet, error)

type sessionEventPager struct {
	mu      sync.Mutex
	resolve sessionEventSourceResolver
	now     func() time.Time
	key     [sha256.Size]byte
	entries map[string]*sessionEventCursor
	closed  bool
}

type sessionEventCursor struct {
	token      string
	subject    string
	target     string
	signature  [sha256.Size]byte
	runtime    string
	sources    []sessionEventCursorSource
	decoder    *runtimeevents.PageDecoder
	sequence   int64
	after      int64
	source     int
	created    time.Time
	lastUsed   time.Time
	bytes      int
	page       []byte
	nextCursor string
	pageErr    error
}

type sessionEventCursorSource struct {
	spec       sessionEventSource
	offset     int64
	identity   os.FileInfo
	anchor     []byte
	partial    []byte
	discarding bool
	recordSize int64
	pending    []harnessproto.RuntimeEvent
	pendingAt  int
}

func newSessionEventPager(resolve sessionEventSourceResolver) (*sessionEventPager, error) {
	if resolve == nil {
		return nil, errors.New("runtime event source resolver unavailable")
	}
	p := &sessionEventPager{resolve: resolve, now: time.Now, entries: make(map[string]*sessionEventCursor)}
	if _, err := rand.Read(p.key[:]); err != nil {
		return nil, fmt.Errorf("runtime event cursor key: %w", err)
	}
	return p, nil
}

func newDaemonSessionEventPager(d *Daemon) (*sessionEventPager, error) {
	if d == nil {
		return nil, errors.New("runtime event daemon unavailable")
	}
	return newSessionEventPager(d.resolveTrackedSessionEventSources)
}

func (p *sessionEventPager) close() {
	p.mu.Lock()
	p.closed = true
	p.entries = make(map[string]*sessionEventCursor)
	p.mu.Unlock()
}

// page answers one already-authorized query. The lifecycle owner performs the
// final current-scope policy check immediately before this call and keeps it out
// of the global effect lock; this method independently requires a tracked source.
func (p *sessionEventPager) page(ctx context.Context, principal access.Principal, target string, fields map[string]string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if principal.Kind != access.SubjectSession || strings.TrimSpace(principal.SubjectID) == "" || strings.TrimSpace(target) == "" {
		return nil, access.ErrDenied
	}
	cursorToken, after, err := parseSessionEventFields(fields)
	if err != nil {
		return nil, err
	}
	sources, err := p.resolve(ctx, target)
	if err != nil {
		return nil, err
	}
	if sources.target != target {
		return nil, access.ErrDenied
	}

	if cursorToken == "" {
		state, err := newSessionEventCursor(principal.SubjectID, sources, after, p.now())
		if err != nil {
			return nil, err
		}
		page, next, err := p.computePage(ctx, state, sources)
		if err != nil {
			return nil, err
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.closed {
			return nil, errEventCursorInvalid
		}
		p.admitLocked(next)
		return page, nil
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, errEventCursorInvalid
	}
	p.expireLocked(p.now())
	entry := p.entries[cursorToken]
	if entry == nil || entry.subject != principal.SubjectID || entry.target != target || entry.signature != sources.signature {
		p.mu.Unlock()
		return nil, errEventCursorInvalid
	}
	entry.lastUsed = p.now()
	if len(entry.page) != 0 || entry.pageErr != nil {
		page := append([]byte(nil), entry.page...)
		cachedErr := entry.pageErr
		p.mu.Unlock()
		if cachedErr != nil {
			return nil, cachedErr
		}
		if err := validateCursorSources(entry, sources); err != nil {
			return nil, err
		}
		return page, nil
	}
	attempt := cloneSessionEventCursor(entry)
	p.mu.Unlock()

	page, next, computeErr := p.computePage(ctx, attempt, sources)
	if ctx.Err() != nil {
		return nil, ctx.Err() // clone and successor are discarded
	}
	p.mu.Lock()
	current := p.entries[cursorToken]
	if current == nil || current != entry || p.closed {
		p.mu.Unlock()
		return nil, errEventCursorInvalid
	}
	if len(entry.page) != 0 || entry.pageErr != nil {
		if entry.pageErr != nil {
			p.mu.Unlock()
			return nil, entry.pageErr
		}
		cached := append([]byte(nil), entry.page...)
		p.mu.Unlock()
		if err := validateCursorSources(entry, sources); err != nil {
			return nil, err
		}
		return cached, nil
	}
	if computeErr != nil {
		entry.pageErr = computeErr
		p.mu.Unlock()
		return nil, computeErr
	}
	entry.page = append([]byte(nil), page...)
	entry.nextCursor = next.token
	entry.bytes = cursorMemoryBytes(entry)
	p.admitLocked(next)
	p.enforceLimitsLocked(entry.subject)
	p.mu.Unlock()
	return page, nil
}

func parseSessionEventFields(fields map[string]string) (cursor string, after int64, err error) {
	for key := range fields {
		if key != core.RuntimeEventsCursorField && key != core.RuntimeEventsAfterSequenceField {
			return "", 0, fmt.Errorf("%w: unknown field %q", errEventQueryInvalid, key)
		}
	}
	cursor = strings.TrimSpace(fields[core.RuntimeEventsCursorField])
	afterText, hasAfter := fields[core.RuntimeEventsAfterSequenceField]
	if cursor != "" && hasAfter {
		return "", 0, fmt.Errorf("%w: cursor and after_sequence are mutually exclusive", errEventQueryInvalid)
	}
	if cursor != "" {
		if len(cursor) != eventCursorEncodedLength {
			return "", 0, errEventCursorInvalid
		}
		if _, decodeErr := base64.RawURLEncoding.DecodeString(cursor); decodeErr != nil {
			return "", 0, errEventCursorInvalid
		}
		return cursor, 0, nil
	}
	if !hasAfter {
		return "", 0, nil
	}
	if afterText == "" || strings.TrimSpace(afterText) != afterText {
		return "", 0, fmt.Errorf("%w: invalid after_sequence", errEventQueryInvalid)
	}
	after, err = strconv.ParseInt(afterText, 10, 64)
	if err != nil || after < 0 {
		return "", 0, fmt.Errorf("%w: invalid after_sequence", errEventQueryInvalid)
	}
	return "", after, nil
}

func newSessionEventCursor(subject string, set sessionEventSourceSet, after int64, now time.Time) (*sessionEventCursor, error) {
	kinds := make([]runtimeevents.PageSourceKind, len(set.sources))
	sources := make([]sessionEventCursorSource, len(set.sources))
	for i, source := range set.sources {
		kinds[i] = source.kind
		sources[i].spec = source
	}
	decoder, ok := runtimeevents.NewPageDecoder(set.runtime, kinds)
	if !ok {
		return nil, errors.New("runtime event source kind unsupported")
	}
	return &sessionEventCursor{
		subject: subject, target: set.target, signature: set.signature, runtime: set.runtime,
		sources: sources, decoder: decoder, after: after, created: now, lastUsed: now,
	}, nil
}

func cloneSessionEventCursor(in *sessionEventCursor) *sessionEventCursor {
	out := *in
	out.decoder = in.decoder.Clone()
	out.page = nil
	out.nextCursor = ""
	out.pageErr = nil
	out.sources = make([]sessionEventCursorSource, len(in.sources))
	for i, source := range in.sources {
		out.sources[i] = source
		out.sources[i].anchor = append([]byte(nil), source.anchor...)
		out.sources[i].partial = append([]byte(nil), source.partial...)
		out.sources[i].pending = append([]harnessproto.RuntimeEvent(nil), source.pending...)
	}
	return &out
}

func (p *sessionEventPager) computePage(ctx context.Context, state *sessionEventCursor, set sessionEventSourceSet) ([]byte, *sessionEventCursor, error) {
	if state.signature != set.signature || state.target != set.target || state.runtime != set.runtime {
		return nil, nil, errEventCursorInvalid
	}
	startSequence := state.sequence
	page := core.RuntimeEventPage{
		Target: state.target, Runtime: state.runtime, CursorSequence: startSequence,
		Events: make([]core.SequencedRuntimeEvent, 0),
	}
	readBytes, records, processed := 0, 0, 0
	sweepComplete := len(state.sources) == 0
	for !sweepComplete && readBytes < eventPageReadBytes && records < eventPageRecords && processed < eventPageEvents {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if state.source >= len(state.sources) {
			state.source = 0
			sweepComplete = true
			break
		}
		source := &state.sources[state.source]
		if source.pendingAt < len(source.pending) {
			event := source.pending[source.pendingAt]
			seq := state.sequence + 1
			if seq <= state.after {
				state.sequence = seq
				source.pendingAt++
				processed++
				if source.pendingAt == len(source.pending) {
					source.pending = nil
					source.pendingAt = 0
				}
				continue
			}
			item := core.SequencedRuntimeEvent{Sequence: seq, Event: event}
			candidate := page
			candidate.Events = append(append([]core.SequencedRuntimeEvent(nil), page.Events...), item)
			if pageFits(candidate) {
				page.Events = append(page.Events, item)
				state.sequence = seq
				source.pendingAt++
				processed++
				if source.pendingAt == len(source.pending) {
					source.pending = nil
					source.pendingAt = 0
				}
				continue
			}
			if len(page.Events) != 0 || page.Oversized != nil {
				break
			}
			encoded, _ := json.Marshal(event)
			state.sequence = seq
			processed++
			source.pendingAt++
			if source.pendingAt == len(source.pending) {
				source.pending = nil
				source.pendingAt = 0
			}
			page.Truncated = true
			page.Oversized = &core.OversizedRuntimeEvent{
				Kind: "event", Sequence: seq, Type: event.Type, EncodedBytes: len(encoded), Reason: "event_exceeds_page_limit",
			}
			break
		}

		f, err := openSessionEventSource(source)
		if errors.Is(err, fs.ErrNotExist) {
			if source.identity != nil {
				return nil, nil, errEventCursorInvalid
			}
			state.source++
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		info, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return nil, nil, err
		}
		if source.identity != nil && (!os.SameFile(source.identity, info) || info.Size() < source.offset) {
			_ = f.Close()
			return nil, nil, errEventCursorInvalid
		}
		if source.identity == nil {
			source.identity = info
		}
		if err := validateSourceAnchor(f, source); err != nil {
			_ = f.Close()
			return nil, nil, err
		}
		advanced, hitEOF, rawOversized, err := readEventSource(ctx, f, state, source, set.bindings,
			eventPageReadBytes-readBytes, eventPageRecords-records)
		anchorErr := updateSourceAnchor(f, source)
		_ = f.Close()
		readBytes += advanced.bytes
		records += advanced.records
		if err != nil {
			return nil, nil, err
		}
		if anchorErr != nil {
			return nil, nil, anchorErr
		}
		if rawOversized != nil {
			state.sequence++
			processed++
			if state.sequence <= state.after {
				continue
			}
			rawOversized.Sequence = state.sequence
			page.Truncated = true
			page.Oversized = rawOversized
			break
		}
		if state.decoder.MemoryBytes()+cursorSourceMemoryBytes(state.sources) > eventDecoderStateBytes {
			return nil, nil, errEventStateTooLarge
		}
		if hitEOF && source.offset >= info.Size() {
			state.source++
		}
	}

	if err := refreshSourceAnchors(state); err != nil {
		return nil, nil, err
	}
	page.ScannedSequence = state.sequence
	page.HasMore = !sweepComplete || cursorHasBufferedWork(state)
	state.lastUsed = p.now()
	state.token = p.tokenFor(state)
	page.NextCursor = state.token
	body, err := json.Marshal(page)
	if err != nil {
		return nil, nil, err
	}
	if len(body) > sessionrpc.MaxResponseBody {
		return nil, nil, errors.New("runtime event page exceeds response limit")
	}
	state.bytes = cursorMemoryBytes(state)
	if state.bytes > eventCursorSubjectBytes {
		return nil, nil, errEventStateTooLarge
	}
	return body, state, nil
}

type eventReadProgress struct{ bytes, records int }

func readEventSource(ctx context.Context, f *os.File, state *sessionEventCursor, source *sessionEventCursorSource,
	bindings map[string]string, byteBudget, recordBudget int) (eventReadProgress, bool, *core.OversizedRuntimeEvent, error) {
	var progress eventReadProgress
	if byteBudget <= 0 || recordBudget <= 0 {
		return progress, false, nil, nil
	}
	if _, err := f.Seek(source.offset, io.SeekStart); err != nil {
		return progress, false, nil, err
	}
	counted := &eventCountingReader{reader: io.LimitReader(f, int64(byteBudget))}
	reader := bufio.NewReaderSize(counted, eventReadChunkBytes)
	for progress.bytes < byteBudget && progress.records < recordBudget {
		if err := ctx.Err(); err != nil {
			return progress, false, nil, err
		}
		fragment, err := reader.ReadSlice('\n')
		progress.bytes = int(counted.read)
		if len(fragment) != 0 {
			source.offset += int64(len(fragment))
			source.recordSize += int64(len(fragment))
			if source.discarding || len(source.partial)+len(fragment) > eventRawRecordBytes {
				source.partial = nil
				source.discarding = true
			} else {
				source.partial = append(source.partial, fragment...)
			}
		}
		complete := len(fragment) > 0 && fragment[len(fragment)-1] == '\n'
		if complete {
			progress.records++
			if source.discarding {
				size := source.recordSize
				source.discarding, source.recordSize = false, 0
				source.partial = nil
				return progress, false, &core.OversizedRuntimeEvent{
					Kind: "raw_record", RawBytes: size, Reason: "raw_record_exceeds_decoder_limit",
				}, nil
			}
			line := source.partial
			source.partial = nil
			source.recordSize = 0
			events := state.decoder.Map(state.source, line, bindings)
			source.pending = append(source.pending[:0], events...)
			source.pendingAt = 0
			if len(source.pending) != 0 {
				return progress, false, nil, nil
			}
		}
		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			return progress, true, nil, nil
		case err != nil:
			return progress, false, nil, err
		}
	}
	return progress, false, nil, nil
}

type eventCountingReader struct {
	reader io.Reader
	read   int64
}

func (r *eventCountingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.read += int64(n)
	return n, err
}

func pageFits(page core.RuntimeEventPage) bool {
	page.NextCursor = strings.Repeat("x", eventCursorEncodedLength)
	page.ScannedSequence = int64(^uint64(0) >> 1)
	page.HasMore = true
	body, err := json.Marshal(page)
	return err == nil && len(body) <= sessionrpc.MaxResponseBody
}

func openSessionEventSource(source *sessionEventCursorSource) (*os.File, error) {
	root, err := hostprep.OpenSession(source.spec.root)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.OpenFile(source.spec.rel)
}

func validateSourceAnchor(f *os.File, source *sessionEventCursorSource) error {
	if source.offset == 0 || len(source.anchor) == 0 {
		return nil
	}
	if int64(len(source.anchor)) > source.offset {
		return errEventCursorInvalid
	}
	got := make([]byte, len(source.anchor))
	if _, err := f.ReadAt(got, source.offset-int64(len(got))); err != nil || !hmac.Equal(got, source.anchor) {
		return errEventCursorInvalid
	}
	return nil
}

func refreshSourceAnchors(state *sessionEventCursor) error {
	for i := range state.sources {
		source := &state.sources[i]
		if source.identity == nil || source.offset == 0 {
			continue
		}
		f, err := openSessionEventSource(source)
		if err != nil {
			return errEventCursorInvalid
		}
		info, statErr := f.Stat()
		if statErr != nil || !os.SameFile(source.identity, info) || info.Size() < source.offset {
			_ = f.Close()
			return errEventCursorInvalid
		}
		readErr := updateSourceAnchor(f, source)
		_ = f.Close()
		if readErr != nil {
			return errEventCursorInvalid
		}
	}
	return nil
}

func updateSourceAnchor(f *os.File, source *sessionEventCursorSource) error {
	if source.offset == 0 {
		source.anchor = nil
		return nil
	}
	n := eventSourceAnchorBytes
	if int64(n) > source.offset {
		n = int(source.offset)
	}
	anchor := make([]byte, n)
	if _, err := f.ReadAt(anchor, source.offset-int64(n)); err != nil {
		return err
	}
	source.anchor = anchor
	return nil
}

func validateCursorSources(cursor *sessionEventCursor, set sessionEventSourceSet) error {
	if cursor.signature != set.signature || len(cursor.sources) != len(set.sources) {
		return errEventCursorInvalid
	}
	for i := range cursor.sources {
		source := &cursor.sources[i]
		if source.spec != set.sources[i] {
			return errEventCursorInvalid
		}
		if source.identity == nil {
			continue
		}
		f, err := openSessionEventSource(source)
		if err != nil {
			return errEventCursorInvalid
		}
		info, statErr := f.Stat()
		anchorErr := validateSourceAnchor(f, source)
		_ = f.Close()
		if statErr != nil || !os.SameFile(source.identity, info) || info.Size() < source.offset || anchorErr != nil {
			return errEventCursorInvalid
		}
	}
	return nil
}

func cursorHasBufferedWork(state *sessionEventCursor) bool {
	for _, source := range state.sources {
		if source.pendingAt < len(source.pending) || source.discarding {
			return true
		}
	}
	return false
}

func cursorSourceMemoryBytes(sources []sessionEventCursorSource) int {
	n := len(sources) * 256
	for _, source := range sources {
		n += len(source.spec.root) + len(source.spec.rel) + len(source.anchor) + len(source.partial)
		for _, event := range source.pending[source.pendingAt:] {
			n += 96 + len(event.Type) + len(event.ItemID) + len(event.Direction) + len(event.Payload)
		}
	}
	return n
}

func cursorMemoryBytes(cursor *sessionEventCursor) int {
	if cursor == nil {
		return 0
	}
	return 512 + len(cursor.token) + len(cursor.subject) + len(cursor.target) + len(cursor.runtime) +
		cursorSourceMemoryBytes(cursor.sources) + cursor.decoder.MemoryBytes() + len(cursor.page) + len(cursor.nextCursor)
}

func (p *sessionEventPager) tokenFor(cursor *sessionEventCursor) string {
	mac := hmac.New(sha256.New, p.key[:])
	_, _ = mac.Write([]byte(cursor.subject))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(cursor.target))
	_, _ = mac.Write(cursor.signature[:])
	var numbers [24]byte
	binary.BigEndian.PutUint64(numbers[0:8], uint64(cursor.sequence))
	binary.BigEndian.PutUint64(numbers[8:16], uint64(cursor.after))
	binary.BigEndian.PutUint64(numbers[16:24], uint64(cursor.source))
	_, _ = mac.Write(numbers[:])
	for _, source := range cursor.sources {
		binary.BigEndian.PutUint64(numbers[0:8], uint64(source.offset))
		binary.BigEndian.PutUint64(numbers[8:16], uint64(source.recordSize))
		_, _ = mac.Write(numbers[:16])
		_, _ = mac.Write(source.anchor)
	}
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (p *sessionEventPager) admitLocked(cursor *sessionEventCursor) {
	if cursor == nil || p.closed {
		return
	}
	cursor.bytes = cursorMemoryBytes(cursor)
	if cursor.bytes > eventCursorSubjectBytes {
		return
	}
	if old := p.entries[cursor.token]; old != nil {
		old.lastUsed = cursor.lastUsed
		return
	}
	p.entries[cursor.token] = cursor
	p.enforceLimitsLocked(cursor.subject)
}

func (p *sessionEventPager) expireLocked(now time.Time) {
	for token, entry := range p.entries {
		if now.Sub(entry.lastUsed) > eventCursorTTL {
			delete(p.entries, token)
		}
	}
}

func (p *sessionEventPager) enforceLimitsLocked(subject string) {
	p.expireLocked(p.now())
	for {
		count, subjectBytes, globalBytes := 0, 0, 0
		for _, entry := range p.entries {
			globalBytes += entry.bytes
			if entry.subject == subject {
				count++
				subjectBytes += entry.bytes
			}
		}
		if count <= eventCursorPerSubject && subjectBytes <= eventCursorSubjectBytes &&
			len(p.entries) <= eventCursorEntries && globalBytes <= eventCursorGlobalBytes {
			return
		}
		var oldest *sessionEventCursor
		for _, entry := range p.entries {
			if (count > eventCursorPerSubject || subjectBytes > eventCursorSubjectBytes) && entry.subject != subject {
				continue
			}
			if oldest == nil || entry.lastUsed.Before(oldest.lastUsed) {
				oldest = entry
			}
		}
		if oldest == nil {
			return
		}
		delete(p.entries, oldest.token)
	}
}

func (d *Daemon) resolveTrackedSessionEventSources(ctx context.Context, target string) (sessionEventSourceSet, error) {
	if err := ctx.Err(); err != nil {
		return sessionEventSourceSet{}, err
	}
	db, err := store.OpenReadOnly()
	if err != nil {
		return sessionEventSourceSet{}, err
	}
	session, ok, err := db.GetSession(target)
	_ = db.Close()
	if err != nil {
		return sessionEventSourceSet{}, err
	}
	if !ok || session.ID != target {
		return sessionEventSourceSet{}, access.ErrDenied
	}
	return d.sessionEventSourcesForTracked(ctx, session)
}

func (d *Daemon) sessionEventSourcesForTracked(ctx context.Context, session store.Session) (sessionEventSourceSet, error) {
	runtime := agent.Canonical(session.Agent)
	set := sessionEventSourceSet{target: session.ID, runtime: runtime}
	if runtime == harnessproto.RuntimeCodex && d.structuredResolvable(session.ID) {
		path := codexapp.EventLogPathFor(session.ID)
		set.sources = append(set.sources, sourceBelow(runtimeevents.PageSourceStructured, core.DataDir(), path))
	} else {
		private, err := privateRuntimeEventSource(ctx, session, runtime)
		if err != nil {
			return sessionEventSourceSet{}, err
		}
		if private != nil {
			set.sources = append(set.sources, *private)
		}
		if runtime == harnessproto.RuntimeClaude && session.ClaudeID != "" {
			set.sources = append(set.sources, sourceBelow(runtimeevents.PageSourcePermission,
				core.StateDir(), core.PermissionJournalPath(session.ClaudeID)))
		}
		set.sources = append(set.sources, sourceBelow(runtimeevents.PageSourceJournal,
			core.StateDir(), core.JournalPath(session.ID)))
	}
	for _, source := range set.sources {
		if source.root == "" || source.rel == "" {
			return sessionEventSourceSet{}, errors.New("runtime event source is not rooted")
		}
	}
	set.bindings = d.bindRuntimeRecord(session.ID, core.RuntimeRecord{Runtime: runtime}).PermissionBindings
	set.signature = eventSourceSignature(session, set)
	return set, nil
}

func privateRuntimeEventSource(ctx context.Context, session store.Session, runtime string) (*sessionEventSource, error) {
	if session.Dir == "" || session.ClaudeID == "" || !validRuntimeConversationID(session.ClaudeID) {
		return nil, nil
	}
	harness := agent.HarnessFor(session.Agent)
	spec, ok := harness.Config(session)
	if !ok || spec.Root == "" || spec.Dir == "" || filepath.Clean(spec.Root) != filepath.Clean(session.Dir) {
		return nil, nil
	}
	root, err := hostprep.OpenSession(spec.Root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer root.Close()
	homeRel, err := root.Rel(spec.Dir)
	if err != nil {
		return nil, err
	}
	switch runtime {
	case harnessproto.RuntimeClaude:
		path := claudecfg.At(spec.Dir).TranscriptPath(session.Dir, session.ClaudeID)
		rel, err := root.Rel(path)
		if err != nil {
			return nil, err
		}
		if _, err := root.StatFile(rel); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, nil
			}
			return nil, err
		}
		return &sessionEventSource{kind: runtimeevents.PageSourceClaude, root: spec.Root, rel: rel}, nil
	case harnessproto.RuntimeCodex:
		sessionsRel := filepath.Join(homeRel, "sessions")
		var found string
		suffix := "-" + session.ClaudeID + ".jsonl"
		err := root.WalkFilesBounded(ctx, sessionsRel, eventSourceWalkEntries, func(name string, _ fs.FileInfo) error {
			if strings.HasPrefix(filepath.Base(name), "rollout-") && strings.HasSuffix(filepath.Base(name), suffix) {
				found = filepath.Join(sessionsRel, name)
				return fs.SkipAll
			}
			return nil
		})
		if errors.Is(err, fs.SkipAll) {
			err = nil
		}
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if found == "" {
			return nil, nil
		}
		return &sessionEventSource{kind: runtimeevents.PageSourceCodex, root: spec.Root, rel: found}, nil
	default:
		return nil, nil
	}
}

func validRuntimeConversationID(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed.String() == strings.ToLower(value)
}

func sourceBelow(kind runtimeevents.PageSourceKind, root, path string) sessionEventSource {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil || rel == "." || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return sessionEventSource{kind: kind}
	}
	return sessionEventSource{kind: kind, root: filepath.Clean(root), rel: rel}
}

func eventSourceSignature(session store.Session, set sessionEventSourceSet) [sha256.Size]byte {
	h := sha256.New()
	for _, value := range []string{session.ID, session.RootID, session.Agent, session.Dir, session.ClaudeID, set.runtime} {
		_, _ = h.Write([]byte(value))
		_, _ = h.Write([]byte{0})
	}
	for _, source := range set.sources {
		_, _ = h.Write([]byte(source.kind))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(source.root))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(source.rel))
		_, _ = h.Write([]byte{0})
	}
	var out [sha256.Size]byte
	copy(out[:], h.Sum(nil))
	return out
}
