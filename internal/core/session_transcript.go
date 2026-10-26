package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// MaxSessionTranscriptBytes bounds one managed transcript snapshot. Content is
// streamed, so memory use remains constant below this storage bound.
const (
	MaxSessionTranscriptBytes int64 = 256 << 20
	MaxSessionCaptureLogBytes int64 = 1 << 20
)

var ErrSessionTranscriptTooLarge = errors.New("managed transcript exceeds 256 MiB")

type sessionTranscriptMeta struct {
	SubjectID string `json:"subject_id"`
	RuntimeID string `json:"runtime_id"`
	Bytes     int64  `json:"bytes"`
	Updated   int64  `json:"updated"`
	// Source identity and the digest of the captured tail let the next capture
	// append only the bytes Claude added since, instead of re-copying the whole
	// append-only transcript. Absent (older captures) means copy in full.
	SourceDev uint64 `json:"source_dev,omitempty"`
	SourceIno uint64 `json:"source_ino,omitempty"`
	Anchor    string `json:"anchor,omitempty"`
}

// sessionTranscriptAnchorBytes is how much of the captured tail must still match
// the source for an append to continue the same transcript.
const sessionTranscriptAnchorBytes = 4 << 10

// SessionTranscriptStage is a private, unpublished transcript snapshot. Its
// fields are intentionally opaque outside core: callers can only publish it to
// the subject/runtime destination chosen when staging began, or abort it.
type SessionTranscriptStage struct {
	subjectID string
	runtimeID string
	event     string
	tmpName   string
	size      int64
	finished  bool
	// base > 0 marks a delta stage: tmpName holds only the bytes after base,
	// appended to the published copy (which must still be base bytes long).
	base      int64
	sourceDev uint64
	sourceIno uint64
	// durable stages fsync; tool-boundary deltas skip it (see syncCaptureEvent).
	durable bool
}

// syncCaptureEvent reports whether a capture at event must be durable on disk
// before it is acknowledged. Turn, compaction and session ends are the restore
// points; per-tool snapshots are superseded within seconds, and an fsync on
// each one made every tool call pay a disk flush.
func syncCaptureEvent(event string) bool {
	return event != "PostToolUse" && event != "SubagentStop" && event != "UserPromptSubmit"
}

func sessionTranscriptPaths(subjectID, runtimeID string) (content, metadata, logPath string) {
	content = sessionRecordPath(TranscriptDir(), subjectID, runtimeID, ".jsonl")
	if content == "" {
		return "", "", ""
	}
	return content, content + ".meta", content + ".log"
}

// CaptureSessionTranscript streams an already-open, caller-independent source
// into subject/runtime-scoped daemon storage. Source path validation belongs to
// the daemon before it obtains src; this function never accepts a path.
func CaptureSessionTranscript(subjectID, runtimeID, event string, src io.Reader) (int64, error) {
	stage, err := StageSessionTranscript(context.Background(), subjectID, runtimeID, event, src)
	if err != nil {
		return 0, err
	}
	defer stage.Abort()
	if err := stage.Publish(); err != nil {
		return stage.size, err
	}
	return stage.size, nil
}

// StageSessionTranscript copies src into an unpublished private file with
// constant working memory. Context cancellation is checked between reads. The
// caller must call Publish or Abort; abandoned stages are not visible to restore.
//
// When src is a regular *os.File continuing the transcript already captured
// (same inode, not shorter, captured tail unchanged), only the new bytes are
// staged and Publish appends them. Truncation, replacement, or any mismatch
// re-copies in full.
func StageSessionTranscript(ctx context.Context, subjectID, runtimeID, event string, src io.Reader) (*SessionTranscriptStage, error) {
	content, metadata, _ := sessionTranscriptPaths(subjectID, runtimeID)
	if content == "" || src == nil {
		return nil, errors.New("managed transcript requires subject, runtime, and source")
	}
	if err := os.MkdirAll(filepath.Dir(content), 0o700); err != nil {
		return nil, err
	}
	var dev, ino uint64
	base := int64(0)
	if f, ok := src.(*os.File); ok {
		if info, err := f.Stat(); err == nil && info.Mode().IsRegular() {
			dev, ino = fileIdentity(info)
			base = appendBase(f, info.Size(), dev, ino, content, metadata, subjectID, runtimeID)
			if base > 0 {
				if _, err := f.Seek(base, io.SeekStart); err != nil {
					base = 0
				}
			}
			if base == 0 {
				if _, err := f.Seek(0, io.SeekStart); err != nil {
					return nil, err
				}
			}
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(content), ".capture-*.tmp")
	if err != nil {
		return nil, err
	}
	tmpName := tmp.Name()
	keep := false
	defer func() {
		_ = tmp.Close()
		if !keep {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return nil, err
	}
	written, err := io.Copy(tmp, io.LimitReader(contextReader{ctx: ctx, r: src}, MaxSessionTranscriptBytes-base+1))
	if err != nil {
		return nil, err
	}
	if base+written > MaxSessionTranscriptBytes {
		return nil, ErrSessionTranscriptTooLarge
	}
	// A full copy replaces the published file by rename and is always synced;
	// a delta is appended at Publish, which decides durability.
	durable := base == 0 || syncCaptureEvent(event)
	if base == 0 {
		if err := tmp.Sync(); err != nil {
			return nil, err
		}
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	keep = true
	return &SessionTranscriptStage{
		subjectID: subjectID, runtimeID: runtimeID, event: event,
		tmpName: tmpName, size: base + written, base: base,
		sourceDev: dev, sourceIno: ino, durable: durable,
	}, nil
}

// appendBase returns how many already-captured bytes src provably continues,
// or 0 when the capture must start over.
func appendBase(src *os.File, srcSize int64, dev, ino uint64, content, metadata, subjectID, runtimeID string) int64 {
	if ino == 0 {
		return 0
	}
	b, err := os.ReadFile(metadata)
	if err != nil {
		return 0
	}
	var meta sessionTranscriptMeta
	if json.Unmarshal(b, &meta) != nil || meta.SubjectID != subjectID || meta.RuntimeID != runtimeID ||
		meta.SourceDev != dev || meta.SourceIno != ino || meta.Anchor == "" ||
		meta.Bytes <= 0 || meta.Bytes > srcSize || meta.Bytes > MaxSessionTranscriptBytes {
		return 0
	}
	info, err := os.Lstat(content)
	if err != nil || !info.Mode().IsRegular() || info.Size() != meta.Bytes {
		return 0 // a crash between append and metadata, or a damaged copy
	}
	anchor, err := tailAnchor(src, meta.Bytes)
	if err != nil || anchor != meta.Anchor {
		return 0 // rewritten in place
	}
	return meta.Bytes
}

// tailAnchor digests the sessionTranscriptAnchorBytes ending at size.
func tailAnchor(r io.ReaderAt, size int64) (string, error) {
	n := int64(sessionTranscriptAnchorBytes)
	if size < n {
		n = size
	}
	buf := make([]byte, n)
	if _, err := r.ReadAt(buf, size-n); err != nil && !(errors.Is(err, io.EOF) && n == 0) {
		return "", err
	}
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:]), nil
}

// Publish atomically makes a completed stage visible with exact provenance.
func (s *SessionTranscriptStage) Publish() error {
	if s == nil || s.finished || s.tmpName == "" {
		return errors.New("managed transcript stage is unavailable")
	}
	content, metadata, logPath := sessionTranscriptPaths(s.subjectID, s.runtimeID)
	if content == "" {
		return errors.New("managed transcript stage identity is incomplete")
	}
	meta := sessionTranscriptMeta{
		SubjectID: s.subjectID, RuntimeID: s.runtimeID, Bytes: s.size,
		Updated: time.Now().UnixMilli(), SourceDev: s.sourceDev, SourceIno: s.sourceIno,
	}
	if s.base > 0 {
		return s.publishDelta(content, metadata, logPath, meta)
	}
	if s.sourceIno != 0 {
		f, err := os.Open(s.tmpName)
		if err != nil {
			return err
		}
		meta.Anchor, err = tailAnchor(f, s.size)
		_ = f.Close()
		if err != nil {
			return err
		}
	}
	b, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	backup, err := moveAsideSessionTranscript(content)
	if err != nil {
		return err
	}
	if err := os.Rename(s.tmpName, content); err != nil {
		if restoreErr := restoreSessionTranscript(content, backup); restoreErr != nil {
			return errors.Join(err, restoreErr)
		}
		return err
	}
	s.finished = true
	if err := writeSessionTranscriptMeta(metadata, b); err != nil {
		if restoreErr := restoreSessionTranscript(content, backup); restoreErr != nil {
			return errors.Join(err, restoreErr)
		}
		return err
	}
	if backup != "" {
		_ = os.Remove(backup)
	}
	appendSessionCaptureLog(logPath, s.event, s.size)
	return nil
}

// publishDelta appends a delta stage to the published copy. If the copy no
// longer has exactly base bytes the stage is refused; the next capture sees the
// mismatch and starts over with a full copy.
func (s *SessionTranscriptStage) publishDelta(content, metadata, logPath string, meta sessionTranscriptMeta) error {
	delta, err := os.Open(s.tmpName)
	if err != nil {
		return err
	}
	defer delta.Close()
	out, err := os.OpenFile(content, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	info, err := out.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != s.base {
		_ = out.Close()
		return errors.Join(errors.New("managed transcript changed during capture"), err)
	}
	if _, err := io.Copy(out, delta); err != nil {
		_ = out.Close()
		return err
	}
	if s.durable {
		if err := out.Sync(); err != nil {
			_ = out.Close()
			return err
		}
	}
	if err := out.Close(); err != nil {
		return err
	}
	f, err := os.Open(content)
	if err != nil {
		return err
	}
	meta.Anchor, err = tailAnchor(f, s.size)
	_ = f.Close()
	if err != nil {
		return err
	}
	b, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	write := writeSessionTranscriptMeta
	if !s.durable {
		write = privateWrite
	}
	if err := write(metadata, b); err != nil {
		return err
	}
	s.finished = true
	_ = os.Remove(s.tmpName)
	appendSessionCaptureLog(logPath, s.event, s.size)
	return nil
}

var writeSessionTranscriptMeta = atomicPrivateWrite

func moveAsideSessionTranscript(content string) (string, error) {
	info, err := os.Lstat(content)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("managed transcript destination is not a regular file")
	}
	tmp, err := os.CreateTemp(filepath.Dir(content), ".previous-*.tmp")
	if err != nil {
		return "", err
	}
	backup := tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(backup)
		return "", err
	}
	if err := os.Remove(backup); err != nil {
		return "", err
	}
	if err := os.Rename(content, backup); err != nil {
		return "", err
	}
	return backup, nil
}

func restoreSessionTranscript(content, backup string) error {
	if err := os.Remove(content); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if backup == "" {
		return nil
	}
	return os.Rename(backup, content)
}

// Abort removes an unpublished stage. It is safe after Publish and idempotent.
func (s *SessionTranscriptStage) Abort() {
	if s == nil || s.finished || s.tmpName == "" {
		return
	}
	_ = os.Remove(s.tmpName)
	s.finished = true
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	default:
		return r.r.Read(p)
	}
}

// privateWrite is atomicPrivateWrite without the fsync, for records that a
// later durable capture supersedes.
func privateWrite(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".record-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func atomicPrivateWrite(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".record-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func appendSessionCaptureLog(path, event string, size int64) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	b, err := json.Marshal(struct {
		At    int64  `json:"at"`
		Event string `json:"event,omitempty"`
		Bytes int64  `json:"bytes"`
	}{At: time.Now().UnixMilli(), Event: event, Bytes: size})
	if err != nil {
		return
	}
	line := append(b, '\n')
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size()+int64(len(line)) > MaxSessionCaptureLogBytes {
		return
	}
	_, _ = f.Write(line)
}

// SessionCapturedTranscript returns only a backup whose daemon-written
// provenance and current size match the exact authoritative subject/runtime.
func SessionCapturedTranscript(subjectID, runtimeID string) (path string, size int64, ok bool) {
	content, metadata, _ := sessionTranscriptPaths(subjectID, runtimeID)
	if content == "" {
		return "", 0, false
	}
	b, err := os.ReadFile(metadata)
	if err != nil {
		return "", 0, false
	}
	var meta sessionTranscriptMeta
	if json.Unmarshal(b, &meta) != nil || meta.SubjectID != subjectID || meta.RuntimeID != runtimeID ||
		meta.Bytes < 0 || meta.Bytes > MaxSessionTranscriptBytes {
		return "", 0, false
	}
	info, err := os.Stat(content)
	if err != nil || !info.Mode().IsRegular() || info.Size() != meta.Bytes {
		return "", 0, false
	}
	return content, meta.Bytes, true
}

// ValidateSessionTranscriptSize gives callers a stable error for a source that
// is known to exceed the capture bound before copying begins.
func ValidateSessionTranscriptSize(size int64) error {
	if size < 0 {
		return fmt.Errorf("invalid transcript size %d", size)
	}
	if size > MaxSessionTranscriptBytes {
		return ErrSessionTranscriptTooLarge
	}
	return nil
}
