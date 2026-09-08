package core

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// MaxPermissionObservationJournalBytes bounds diagnostic telemetry for one
// authoritative subject/runtime generation. It is not an approval source and
// must not offer a session an unbounded append-only host write.
const MaxPermissionObservationJournalBytes int64 = 1 << 20

var ErrPermissionObservationJournalFull = errors.New("permission observation journal is full")

// PermissionObservation is an authenticated session's hook observation. It is
// deliberately not PermissionRecord and its directory is never a
// runtimeevents permission source, so it cannot mint an answerable occurrence.
type PermissionObservation struct {
	SubjectID         string   `json:"subject_id"`
	RuntimeID         string   `json:"runtime_id"`
	RuntimeGeneration string   `json:"runtime_generation"`
	RequestID         string   `json:"request_id"`
	Tool              string   `json:"tool,omitempty"`
	Action            string   `json:"action,omitempty"`
	Options           []string `json:"options,omitempty"`
	Decision          string   `json:"decision,omitempty"`
	At                int64    `json:"at"`
}

func (r PermissionObservation) Open() bool { return r.Decision == "" }

func PermissionObservationDir() string { return filepath.Join(StateDir(), "permission-observations") }

func PermissionObservationPath(subjectID, runtimeID, generation string) string {
	if strings.TrimSpace(generation) == "" {
		return ""
	}
	return sessionRecordPath(PermissionObservationDir(), subjectID, runtimeID+"\x00"+generation, ".jsonl")
}

func AppendPermissionObservation(subjectID, runtimeID, generation string, rec PermissionObservation) error {
	path := PermissionObservationPath(subjectID, runtimeID, generation)
	if path == "" || strings.TrimSpace(rec.RequestID) == "" {
		return errors.New("permission observation requires subject, runtime, generation, and request id")
	}
	rec.SubjectID = subjectID
	rec.RuntimeID = runtimeID
	rec.RuntimeGeneration = generation
	if rec.At == 0 {
		rec.At = time.Now().UnixMilli()
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	line := append(b, '\n')
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || info.Size()+int64(len(line)) > MaxPermissionObservationJournalBytes {
		return ErrPermissionObservationJournalFull
	}
	_, err = f.Write(line)
	return err
}

func ReadPermissionObservations(subjectID, runtimeID, generation string) ([]PermissionObservation, error) {
	path := PermissionObservationPath(subjectID, runtimeID, generation)
	if path == "" {
		return nil, errors.New("permission observation identity is incomplete")
	}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []PermissionObservation
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 64*1024)
	for scanner.Scan() {
		var rec PermissionObservation
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			return nil, fmt.Errorf("decode permission observation: %w", err)
		}
		if rec.SubjectID != subjectID || rec.RuntimeID != runtimeID ||
			rec.RuntimeGeneration != generation || strings.TrimSpace(rec.RequestID) == "" {
			return nil, errors.New("permission observation provenance mismatch")
		}
		out = append(out, rec)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func PendingPermissionObservations(subjectID, runtimeID, generation string) ([]PermissionObservation, error) {
	records, err := ReadPermissionObservations(subjectID, runtimeID, generation)
	if err != nil {
		return nil, err
	}
	var open []PermissionObservation
	for _, rec := range records {
		if rec.Open() {
			open = append(open, rec)
			continue
		}
		for i, current := range open {
			if current.RequestID == rec.RequestID {
				open = append(open[:i], open[i+1:]...)
				break
			}
		}
	}
	return open, nil
}
