package core

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
)

const managedRecordsDir = "managed-v1"

// sessionRecordPath scopes daemon-owned runtime observations by both the
// authoritative amux subject and its stored runtime identity. Hashing produces
// fixed safe path components without allowing either identifier to alias a
// sibling through sanitization collisions.
func sessionRecordPath(root, subjectID, runtimeID, suffix string) string {
	if strings.TrimSpace(subjectID) == "" || strings.TrimSpace(runtimeID) == "" {
		return ""
	}
	subject := sha256.Sum256([]byte(subjectID))
	runtime := sha256.Sum256([]byte(runtimeID))
	return filepath.Join(root, managedRecordsDir, hex.EncodeToString(subject[:]), hex.EncodeToString(runtime[:])+suffix)
}
