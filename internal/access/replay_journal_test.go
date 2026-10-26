package access

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var replayNow = time.Unix(1_800_000_000, 0)

func openReplayAuthority(t *testing.T, root string) *FileAuthority {
	t.Helper()
	a, err := open(root, func() time.Time { return replayNow }, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func replayCredential(t *testing.T, a *FileAuthority, subject string) Credential {
	t.Helper()
	dir, err := a.Ensure(context.Background(), SubjectSession, subject)
	if err != nil {
		t.Fatal(err)
	}
	cred, err := LoadCredential(dir)
	if err != nil {
		t.Fatal(err)
	}
	return cred
}

func acceptRequests(t *testing.T, a *FileAuthority, cred Credential, n int) []SignedRequest {
	t.Helper()
	reqs := make([]SignedRequest, n)
	for i := range reqs {
		req, err := SignRequest(cred, a.BootID(), []byte(fmt.Sprintf(`{"n":%d}`, i)), replayNow, rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.Verify(context.Background(), req); err != nil {
			t.Fatalf("accept %d: %v", i, err)
		}
		reqs[i] = req
	}
	return reqs
}

// reopenAsBoot restarts the authority, then (as TestReplayLedgerSurvivesRestart
// does) checks requests under their original boot: the crash window in which
// only the durable ledger stands between a request and a second execution.
func reopenAsBoot(t *testing.T, a *FileAuthority, root string) *FileAuthority {
	t.Helper()
	boot := a.BootID()
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	b := openReplayAuthority(t, root)
	t.Cleanup(func() { _ = b.Close() })
	b.bootID = boot
	return b
}

func expectReplays(t *testing.T, a *FileAuthority, reqs []SignedRequest) {
	t.Helper()
	for i, req := range reqs {
		if _, err := a.Verify(context.Background(), req); !errors.Is(err, ErrReplay) {
			t.Fatalf("request %d after restart = %v, want replay", i, err)
		}
	}
}

func currentJournal(t *testing.T, root string, shard int) string {
	t.Helper()
	files, err := journalFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(files) - 1; i >= 0; i-- {
		if filepath.Base(files[i])[len(filepath.Base(files[i]))-1] == byte('0'+shard) {
			return files[i]
		}
	}
	t.Fatalf("no journal for shard %d", shard)
	return ""
}

func TestReplayRejectedAfterRestartAcrossCompaction(t *testing.T) {
	root := t.TempDir()
	a := openReplayAuthority(t, root)
	cred := replayCredential(t, a, "a1")
	before := acceptRequests(t, a, cred, 20)
	if err := a.compactReplay(); err != nil {
		t.Fatal(err)
	}
	after := acceptRequests(t, a, cred, 20)
	b := reopenAsBoot(t, a, root)
	expectReplays(t, b, append(before, after...))
	// Recovery folded everything into the snapshot and started over.
	b.journal.compactMu.Lock()
	records := b.journal.records.Load()
	b.journal.compactMu.Unlock()
	if records != 0 {
		t.Fatalf("recovered authority starts with %d journaled records", records)
	}
}

func TestReplayJournalTornTailIsDropped(t *testing.T) {
	for _, tail := range []struct {
		name string
		data func([]byte) []byte
	}{
		{"short header", func(record []byte) []byte { return record[:replayHeader-3] }},
		{"short payload", func(record []byte) []byte { return record[:len(record)-5] }},
		{"zero-filled extension", func(record []byte) []byte { return make([]byte, len(record)) }},
		{"bad checksum", func(record []byte) []byte {
			torn := append([]byte(nil), record...)
			torn[len(torn)-2] ^= 0xff
			return torn
		}},
	} {
		t.Run(tail.name, func(t *testing.T) {
			root := t.TempDir()
			a := openReplayAuthority(t, root)
			cred := replayCredential(t, a, "a1")
			accepted := acceptRequests(t, a, cred, 5)
			// A sixth record whose write the crash cut short.
			record, err := encodeReplayRecord(replayRecord{KeyID: cred.KeyID, Generation: cred.Generation,
				RequestID: "ffffffffffffffffffffffffffffffff", BodyDigest: "00", ExpiresAt: replayNow.Add(MaxRequestAge).UnixMilli()})
			if err != nil {
				t.Fatal(err)
			}
			path := currentJournal(t, root, shardFor(cred.KeyID))
			f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.Write(tail.data(record)); err != nil {
				t.Fatal(err)
			}
			_ = f.Close()
			b := reopenAsBoot(t, a, root)
			expectReplays(t, b, accepted)
			if _, ok := b.replay.Entries[cred.KeyID+":"+fmt.Sprint(cred.Generation)+":ffffffffffffffffffffffffffffffff"]; ok {
				t.Fatal("torn record was adopted")
			}
		})
	}
}

func TestReplayJournalCorruptionBeforeTailFailsOpen(t *testing.T) {
	root := t.TempDir()
	a := openReplayAuthority(t, root)
	cred := replayCredential(t, a, "a1")
	acceptRequests(t, a, cred, 3)
	path := currentJournal(t, root, shardFor(cred.KeyID))
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[replayHeader+1] ^= 0xff // inside the first of three records
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := open(root, func() time.Time { return replayNow }, rand.Reader); err == nil {
		_ = b.Close()
		t.Fatal("authority opened over a corrupt replay journal")
	}
	// An oversized length that is not at the tail is corruption too.
	binary.BigEndian.PutUint32(data, maxReplayRecord+1)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := open(root, func() time.Time { return replayNow }, rand.Reader); err == nil {
		_ = b.Close()
		t.Fatal("authority opened over a corrupt replay journal length")
	}
	// More unreadable bytes than one in-flight record can leave is never a
	// torn tail.
	garbage := make([]byte, 2*(replayHeader+maxReplayRecord))
	for i := range garbage {
		garbage[i] = 0xa5
	}
	if err := os.WriteFile(path, garbage, 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := open(root, func() time.Time { return replayNow }, rand.Reader); err == nil {
		_ = b.Close()
		t.Fatal("authority opened over a long unreadable replay journal tail")
	}
}

// A crash after a compaction's snapshot but before it deleted the old
// generations leaves both; recovery takes their union.
func TestReplayRecoveryMergesStaleGenerations(t *testing.T) {
	root := t.TempDir()
	a := openReplayAuthority(t, root)
	cred := replayCredential(t, a, "a1")
	first := acceptRequests(t, a, cred, 10)
	stale := currentJournal(t, root, shardFor(cred.KeyID))
	saved, err := os.ReadFile(stale)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.compactReplay(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, saved, 0o600); err != nil { // resurrect gen 1
		t.Fatal(err)
	}
	second := acceptRequests(t, a, cred, 10)
	b := reopenAsBoot(t, a, root)
	expectReplays(t, b, append(first, second...))
}

func TestReplayCompactionDuringConcurrentVerifies(t *testing.T) {
	root := t.TempDir()
	a := openReplayAuthority(t, root)
	const subjects, each = 8, 60
	creds := make([]Credential, subjects)
	reqs := make([][]SignedRequest, subjects)
	for s := range creds {
		creds[s] = replayCredential(t, a, fmt.Sprintf("s%d", s))
		for i := 0; i < each; i++ {
			req, err := SignRequest(creds[s], a.BootID(), []byte(fmt.Sprintf(`{"n":%d}`, i)), replayNow, rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			reqs[s] = append(reqs[s], req)
		}
	}
	stop := make(chan struct{})
	var compactions atomic.Int32
	compacted := make(chan struct{})
	go func() {
		defer close(compacted)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := a.compactReplay(); err != nil {
				t.Error(err)
				return
			}
			compactions.Add(1)
		}
	}()
	var wg sync.WaitGroup
	for s := range reqs {
		wg.Add(1)
		go func(s int) {
			defer wg.Done()
			for i, req := range reqs[s] {
				if _, err := a.Verify(context.Background(), req); err != nil {
					t.Errorf("subject %d request %d: %v", s, i, err)
					return
				}
			}
		}(s)
	}
	wg.Wait()
	close(stop)
	<-compacted
	if compactions.Load() == 0 {
		t.Fatal("no compaction overlapped the verifies")
	}
	var all []SignedRequest
	for _, r := range reqs {
		all = append(all, r...)
	}
	expectReplays(t, a, all) // in memory
	b := reopenAsBoot(t, a, root)
	expectReplays(t, b, all) // durable through every rotation
	t.Logf("%d compactions overlapped %d accepted requests", compactions.Load(), len(all))
}

// The decision is taken once under the ledger lock: of concurrent copies of
// one request exactly one is accepted.
func TestConcurrentDuplicatesAcceptOnce(t *testing.T) {
	a := openReplayAuthority(t, t.TempDir())
	t.Cleanup(func() { _ = a.Close() })
	cred := replayCredential(t, a, "a1")
	req, err := SignRequest(cred, a.BootID(), []byte(`{"q":1}`), replayNow, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var accepted, replayed atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch _, err := a.Verify(context.Background(), req); {
			case err == nil:
				accepted.Add(1)
			case errors.Is(err, ErrReplay):
				replayed.Add(1)
			default:
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if accepted.Load() != 1 || replayed.Load() != 31 {
		t.Fatalf("accepted %d, replayed %d", accepted.Load(), replayed.Load())
	}
}

// Until compaction succeeds the journal is bounded: acceptance fails closed.
func TestReplayJournalLimitFailsClosed(t *testing.T) {
	a := openReplayAuthority(t, t.TempDir())
	t.Cleanup(func() { _ = a.Close() })
	cred := replayCredential(t, a, "a1")
	a.journal.records.Store(replayJournalLimit)
	req, _ := SignRequest(cred, a.BootID(), []byte(`{"q":1}`), replayNow, rand.Reader)
	if _, err := a.Verify(context.Background(), req); !errors.Is(err, ErrCapacity) {
		t.Fatalf("verify over the journal limit = %v, want capacity", err)
	}
}

// syncLog records, in order, every sync the journal performs.
type syncLog struct {
	mu    sync.Mutex
	names []string
	fail  func(*os.File) error
}

func (l *syncLog) install(a *FileAuthority) {
	real := a.journal.sync
	a.journal.sync = func(f *os.File) error {
		l.mu.Lock()
		l.names = append(l.names, f.Name())
		fail := l.fail
		l.mu.Unlock()
		if fail != nil {
			if err := fail(f); err != nil {
				return err
			}
		}
		return real(f)
	}
}

// A synced record in a file whose directory entry was never synced can be
// lost in a crash, so a new generation's files must be durable in the
// directory before the first record in them is acknowledged (a record is
// acknowledged only after its own file sync returns).
func TestJournalDirectorySyncPrecedesFirstAcknowledgement(t *testing.T) {
	root := t.TempDir()
	a := openReplayAuthority(t, root)
	t.Cleanup(func() { _ = a.Close() })
	cred := replayCredential(t, a, "a1")
	acceptRequests(t, a, cred, 2)
	var log syncLog
	log.install(a)
	if err := a.compactReplay(); err != nil {
		t.Fatal(err)
	}
	acceptRequests(t, a, cred, 1)
	log.mu.Lock()
	defer log.mu.Unlock()
	dirSync, firstRecord := -1, -1
	gen := fmt.Sprintf("%s.%d.", replayJournalName, a.journal.gen)
	for i, name := range log.names {
		switch {
		case name == a.rootDir.Name() && dirSync < 0:
			dirSync = i
		case strings.Contains(filepath.Base(name), gen) && firstRecord < 0:
			firstRecord = i
		}
	}
	if dirSync < 0 || firstRecord < 0 || dirSync > firstRecord {
		t.Fatalf("directory sync at %d, first new-generation record sync at %d: %q", dirSync, firstRecord, log.names)
	}
}

// If the new generation's directory entries cannot be made durable, nothing
// is acknowledged into it; acceptance resumes once a later rotation succeeds.
func TestFailedJournalDirectorySyncAcknowledgesNothing(t *testing.T) {
	root := t.TempDir()
	a := openReplayAuthority(t, root)
	t.Cleanup(func() { _ = a.Close() })
	cred := replayCredential(t, a, "a1")
	var log syncLog
	log.install(a)
	log.fail = func(f *os.File) error {
		if f.Name() == a.rootDir.Name() {
			return errors.New("injected directory sync failure")
		}
		return nil
	}
	if err := a.compactReplay(); err == nil {
		t.Fatal("rotation succeeded without a durable directory")
	}
	req, _ := SignRequest(cred, a.BootID(), []byte(`{"q":1}`), replayNow, rand.Reader)
	if _, err := a.Verify(context.Background(), req); err == nil {
		t.Fatal("request acknowledged into a journal whose directory entry is not durable")
	}
	// Its durability was unknown, so it stays consumed.
	if _, err := a.Verify(context.Background(), req); !errors.Is(err, ErrReplay) {
		t.Fatalf("retry of an unacknowledged request = %v, want replay", err)
	}
	log.mu.Lock()
	log.fail = nil
	log.mu.Unlock()
	if err := a.compactReplay(); err != nil {
		t.Fatal(err)
	}
	acceptRequests(t, a, cred, 3)
}
