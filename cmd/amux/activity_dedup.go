package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// activityRefresh re-sends an unchanged hook activity state at most this
// often. PostToolUse reports "running" after every tool call; repeating it
// changes nothing in the daemon, but each repeat cost a mailbox call, and 30
// busy agents' repeats were most of the 2026-11-22 saturation.
const activityRefresh = 20 * time.Second

// activityReport is the last activity state one Claude conversation's hooks
// delivered. It is only a client-side rate limit, keyed by conversation so a
// replaced runtime starts fresh.
type activityReport struct {
	State string `json:"state"`
	At    int64  `json:"at"`
}

func activityStatePath(conversation string) string {
	dir := statusLineStateDir()
	if dir == "" || conversation == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(conversation))
	return filepath.Join(dir, "activity-"+hex.EncodeToString(sum[:8])+".json")
}

// activityUnchanged reports whether conversation's hooks already delivered
// state within activityRefresh, so this report can be skipped.
func activityUnchanged(conversation, state string) bool {
	path := activityStatePath(conversation)
	if path == "" {
		return false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var last activityReport
	if json.Unmarshal(b, &last) != nil || last.State != state || last.At == 0 {
		return false
	}
	age := statusLineNow().Sub(time.UnixMilli(last.At))
	return age >= 0 && age < activityRefresh
}

// recordActivity remembers a delivered state. Best effort.
func recordActivity(conversation, state string) {
	path := activityStatePath(conversation)
	if path == "" {
		return
	}
	b, err := json.Marshal(activityReport{State: state, At: statusLineNow().UnixMilli()})
	if err != nil || os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if os.WriteFile(tmp, b, 0o600) != nil {
		return
	}
	if os.Rename(tmp, path) != nil {
		_ = os.Remove(tmp)
	}
}
