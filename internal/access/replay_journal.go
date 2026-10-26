package access

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The replay ledger is a snapshot (replay.json, the format every earlier
// daemon wrote) plus append-only journals. Accepting a request appends one
// checksummed record to one journal shard and syncs it; nothing on that path
// is proportional to the ledger's size. Compaction folds the journals into a
// new snapshot off the hot path.
//
// Invariants:
//   - A request is accepted only after its record is durable in a journal.
//   - Every accepted, unexpired request is in the union of the snapshot and
//     every journal file on disk, at every instant: rotation starts new
//     journals without touching old ones, and old journals are deleted only
//     after a snapshot containing all their records is durable.
//   - Recovery loads that union, so a restart (with its new boot nonce) never
//     forgets an acceptance.
//   - A final record cut short by a crash was never acknowledged (its sync
//     had not returned), so recovery drops it. Any other unreadable record is
//     corruption and fails open, as a corrupt replay.json always has.
const (
	replayShards = 8
	// replayCompactAfter journaled records start a background compaction.
	replayCompactAfter = maxReplayEntries
	// replayJournalLimit journaled records refuse further acceptance
	// (ErrCapacity) until compaction succeeds, bounding disk and recovery.
	replayJournalLimit = 8 * maxReplayEntries
	maxReplayRecord    = 1 << 10
	replayHeader       = 8 // big-endian payload length, then CRC-32C of payload
	replayJournalName  = "replay.journal"
)

var replayCRC = crc32.MakeTable(crc32.Castagnoli)

type replayJournal struct {
	root    string
	dir     *os.File
	sync    func(*os.File) error
	shards  [replayShards]journalShard
	gen     uint64 // current journal generation; guarded by every shard lock
	records atomic.Int64

	compactMu sync.Mutex // one compaction at a time
	wake      chan struct{}
	stop      chan struct{}
	done      chan struct{}
}

type journalShard struct {
	mu sync.Mutex
	f  *os.File
}

func replayKey(rec replayRecord) string {
	return rec.KeyID + ":" + strconv.FormatUint(rec.Generation, 10) + ":" + rec.RequestID
}

func journalPath(root string, gen uint64, shard int) string {
	return filepath.Join(root, fmt.Sprintf("%s.%d.%d", replayJournalName, gen, shard))
}

// journalFiles lists every journal file under root, oldest generation first.
func journalFiles(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	type file struct {
		gen   uint64
		shard int
		name  string
	}
	var files []file
	for _, entry := range entries {
		rest, ok := strings.CutPrefix(entry.Name(), replayJournalName+".")
		if !ok {
			continue
		}
		genText, shardText, ok := strings.Cut(rest, ".")
		gen, genErr := strconv.ParseUint(genText, 10, 64)
		shard, shardErr := strconv.Atoi(shardText)
		if !ok || genErr != nil || shardErr != nil || shard < 0 || shard >= replayShards {
			return nil, fmt.Errorf("unexpected replay journal %q", entry.Name())
		}
		files = append(files, file{gen, shard, entry.Name()})
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].gen != files[j].gen {
			return files[i].gen < files[j].gen
		}
		return files[i].shard < files[j].shard
	})
	names := make([]string, len(files))
	for i, f := range files {
		names[i] = filepath.Join(root, f.name)
	}
	return names, nil
}

// readJournal adds every intact record in path to entries and reports how
// many it read. A torn final record is ignored; anything else unreadable is
// an error.
func readJournal(path string, entries map[string]replayRecord) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, replayJournalLimit*(replayHeader+maxReplayRecord)+1))
	if err != nil {
		return 0, err
	}
	if len(data) > replayJournalLimit*(replayHeader+maxReplayRecord) {
		return 0, ErrCapacity
	}
	// Appends to one file are serialized, each one write then a sync, so a
	// crash can damage only the single in-flight record at the end: at most
	// one record's bytes, possibly zero-filled where the file grew before its
	// data was written. Any other unreadable byte is corruption, never
	// silently dropped: dropping it could forget an acknowledged acceptance.
	torn := func(off int) error {
		if len(data)-off > replayHeader+maxReplayRecord {
			return fmt.Errorf("corrupt replay journal %s at offset %d", filepath.Base(path), off)
		}
		return nil
	}
	n := 0
	for off := 0; off < len(data); {
		if len(data)-off < replayHeader {
			return n, torn(off)
		}
		length := int(binary.BigEndian.Uint32(data[off:]))
		sum := binary.BigEndian.Uint32(data[off+4:])
		if length == 0 && allZero(data[off:]) {
			return n, torn(off)
		}
		if length == 0 || length > maxReplayRecord {
			return n, fmt.Errorf("corrupt replay journal %s at offset %d", filepath.Base(path), off)
		}
		end := off + replayHeader + length
		if end > len(data) {
			return n, torn(off)
		}
		payload := data[off+replayHeader : end]
		var rec replayRecord
		if crc32.Checksum(payload, replayCRC) != sum ||
			json.Unmarshal(payload, &rec) != nil || rec.KeyID == "" || rec.RequestID == "" {
			if end == len(data) {
				return n, nil // the final record, cut short before its sync returned
			}
			return n, fmt.Errorf("corrupt replay journal %s at offset %d", filepath.Base(path), off)
		}
		entries[replayKey(rec)] = rec
		n++
		off = end
	}
	return n, nil
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

func encodeReplayRecord(rec replayRecord) ([]byte, error) {
	payload, err := json.Marshal(rec)
	if err != nil {
		return nil, err
	}
	if len(payload) > maxReplayRecord {
		return nil, ErrCapacity
	}
	buf := make([]byte, replayHeader+len(payload))
	binary.BigEndian.PutUint32(buf, uint32(len(payload)))
	binary.BigEndian.PutUint32(buf[4:], crc32.Checksum(payload, replayCRC))
	copy(buf[replayHeader:], payload)
	return buf, nil
}

// recoverReplay merges every journal into a.replay, folds them into a new
// snapshot, and starts empty journals. It runs in open, before any request.
func (a *FileAuthority) recoverReplay() error {
	files, err := journalFiles(a.root)
	if err != nil {
		return fmt.Errorf("read replay journal: %w", err)
	}
	for _, path := range files {
		if _, err := readJournal(path, a.replay.Entries); err != nil {
			return fmt.Errorf("read replay journal: %w", err)
		}
	}
	// Keep what a request could still match; the union may hold more expired
	// records than the live bound allows.
	a.replay = a.unexpiredReplay()
	j := &replayJournal{
		root: a.root, dir: a.rootDir, sync: a.syncFile, gen: 1,
		wake: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{}),
	}
	if len(files) != 0 {
		// Every journaled acceptance must be in the snapshot before its
		// journal is deleted.
		if _, err := atomicJSONCommit(filepath.Join(a.root, "replay.json"), a.replay, 0o600); err != nil {
			return fmt.Errorf("compact replay journal: %w", err)
		}
		for _, path := range files {
			if err := os.Remove(path); err != nil {
				return fmt.Errorf("compact replay journal: %w", err)
			}
		}
	}
	if err := j.createShards(); err != nil {
		return fmt.Errorf("create replay journal: %w", err)
	}
	a.journal = j
	go a.runReplayCompaction()
	return nil
}

// unexpiredReplay copies the ledger without entries no request could still
// match: an expired request is rejected before the ledger is consulted.
func (a *FileAuthority) unexpiredReplay() replayFile {
	cutoff := a.now().Add(-clockSkew).UnixMilli()
	out := replayFile{Version: ProtocolVersion, Entries: make(map[string]replayRecord, len(a.replay.Entries))}
	for key, rec := range a.replay.Entries {
		if rec.ExpiresAt >= cutoff {
			out.Entries[key] = rec
		}
	}
	return out
}

// createShards opens generation j.gen's journals and makes their names
// durable. The caller holds every shard lock, or no request can run yet.
func (j *replayJournal) createShards() error {
	for i := range j.shards {
		f, err := os.OpenFile(journalPath(j.root, j.gen, i), os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		j.shards[i].f = f
	}
	return j.sync(j.dir)
}

func (j *replayJournal) closeShards() {
	for i := range j.shards {
		if f := j.shards[i].f; f != nil {
			_ = f.Close()
			j.shards[i].f = nil
		}
	}
}

func shardFor(keyID string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(keyID))
	return int(h.Sum32() % replayShards)
}

// append makes rec durable in its key's shard.
func (j *replayJournal) append(rec replayRecord) error {
	buf, err := encodeReplayRecord(rec)
	if err != nil {
		return err
	}
	shard := &j.shards[shardFor(rec.KeyID)]
	shard.mu.Lock()
	defer shard.mu.Unlock()
	if shard.f == nil {
		return ErrAuthorityClosed
	}
	if _, err := shard.f.Write(buf); err != nil {
		return err
	}
	if err := j.sync(shard.f); err != nil {
		return err
	}
	if j.records.Add(1) >= replayCompactAfter {
		select {
		case j.wake <- struct{}{}:
		default:
		}
	}
	return nil
}

// acceptReplay consumes key for rec, then makes the acceptance durable. The
// decision is taken on the single in-memory ledger under replayMu, so
// journal sharding only spreads the syncs: it cannot let a duplicate in. An
// entry stays consumed even when its append fails, because its durability is
// then unknown and the request must not be accepted again.
func (a *FileAuthority) acceptReplay(rec replayRecord, now time.Time) error {
	key := replayKey(rec)
	a.replayMu.Lock()
	if _, exists := a.replay.Entries[key]; exists {
		a.replayMu.Unlock()
		return ErrReplay
	}
	if len(a.replay.Entries) >= maxReplayEntries {
		// Pruning only at capacity keeps acceptance O(1); expired entries
		// left meanwhile can never match, as their requests are expired too.
		cutoff := now.Add(-clockSkew).UnixMilli()
		for k, entry := range a.replay.Entries {
			if entry.ExpiresAt < cutoff {
				delete(a.replay.Entries, k)
			}
		}
	}
	if len(a.replay.Entries) >= maxReplayEntries || a.journal.records.Load() >= replayJournalLimit {
		a.replayMu.Unlock()
		return ErrCapacity
	}
	a.replay.Entries[key] = rec
	a.replayMu.Unlock()
	if err := a.journal.append(rec); err != nil {
		return fmt.Errorf("persist replay acceptance: %w", err)
	}
	return nil
}

func (a *FileAuthority) runReplayCompaction() {
	j := a.journal
	defer close(j.done)
	for {
		select {
		case <-j.stop:
			return
		case <-j.wake:
			// A failure leaves every journal in place; the next wake retries
			// and acceptance stops at replayJournalLimit meanwhile.
			_ = a.compactReplay()
		}
	}
}

// compactReplay starts a new journal generation, then writes a snapshot
// holding everything the old generations hold and deletes them. Requests are
// paused only for the rotation itself, not for the snapshot write.
func (a *FileAuthority) compactReplay() error {
	j := a.journal
	j.compactMu.Lock()
	defer j.compactMu.Unlock()

	// Rotate with the ledger and every shard locked: every entry in the
	// snapshot below was decided by then, and every append not yet made goes
	// to the new generation, so the union never loses a record.
	a.replayMu.Lock()
	for i := range j.shards {
		j.shards[i].mu.Lock()
	}
	snapshot := a.unexpiredReplay()
	rotated := j.records.Load()
	old := j.gen
	var rotateErr error
	if j.shards[0].f == nil {
		rotateErr = ErrAuthorityClosed
	} else {
		j.closeShards()
		j.gen++
		// On failure the shards left unopened refuse appends (fail closed);
		// the old generations stay on disk and in the next recovery.
		rotateErr = j.createShards()
	}
	for i := len(j.shards) - 1; i >= 0; i-- {
		j.shards[i].mu.Unlock()
	}
	a.replayMu.Unlock()
	if rotateErr != nil {
		return rotateErr
	}

	if _, err := atomicJSONCommit(filepath.Join(a.root, "replay.json"), snapshot, 0o600); err != nil {
		return err
	}
	files, err := journalFiles(a.root)
	if err != nil {
		return err
	}
	for _, path := range files {
		gen, ok := journalGeneration(path)
		if ok && gen <= old {
			if err := os.Remove(path); err != nil {
				return err
			}
		}
	}
	j.records.Add(-rotated)
	return j.sync(j.dir)
}

func journalGeneration(path string) (uint64, bool) {
	rest, ok := strings.CutPrefix(filepath.Base(path), replayJournalName+".")
	if !ok {
		return 0, false
	}
	genText, _, ok := strings.Cut(rest, ".")
	gen, err := strconv.ParseUint(genText, 10, 64)
	return gen, ok && err == nil
}

// closeReplay stops compaction and closes the journals. The caller holds the
// authority's write lock, so no request is accepting.
func (a *FileAuthority) closeReplay() error {
	j := a.journal
	if j == nil {
		return nil
	}
	close(j.stop)
	<-j.done
	j.compactMu.Lock()
	defer j.compactMu.Unlock()
	for i := range j.shards {
		j.shards[i].mu.Lock()
	}
	j.closeShards()
	for i := len(j.shards) - 1; i >= 0; i-- {
		j.shards[i].mu.Unlock()
	}
	return nil
}
