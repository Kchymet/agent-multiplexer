package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"amux/internal/claudecfg"
	"amux/internal/sessionreport"
)

const (
	// statusLineRefresh re-sends an unchanged observation so a daemon restart or
	// replaced runtime relearns it within this interval.
	statusLineRefresh = time.Minute
	// statusLineRetry spaces out attempts to deliver the same observation, so
	// renders against a slow daemon do not each start another report.
	statusLineRetry = 15 * time.Second
	// statusLineReportFlag runs one handed-off report (internal; not usage).
	statusLineReportFlag = "--statusline-report"
)

// statusLineReport is what one Claude conversation's status line last
// delivered (…At) and last tried to deliver (…Tried, …TriedAt). It is only a
// client-side rate limit: the daemon authenticates and correlates every report.
type statusLineReport struct {
	Model         string `json:"model,omitempty"`
	ModelAt       int64  `json:"model_at,omitempty"`
	ModelTried    string `json:"model_tried,omitempty"`
	ModelTriedAt  int64  `json:"model_tried_at,omitempty"`
	LimitsHash    string `json:"limits_hash,omitempty"`
	LimitsAt      int64  `json:"limits_at,omitempty"`
	LimitsTried   string `json:"limits_tried,omitempty"`
	LimitsTriedAt int64  `json:"limits_tried_at,omitempty"`
}

var (
	statusLineNow      = time.Now
	statusLineStateDir = func() string {
		dir, err := os.UserCacheDir()
		if err != nil {
			return ""
		}
		return filepath.Join(dir, "amux", "statusline")
	}
	// startStatusLineReport hands a due report to a detached process; tests
	// replace it to run the report inline.
	startStatusLineReport = spawnStatusLineReport
)

// scheduleStatusLineReport decides which observations in payload are due and,
// if any are, records the attempt and hands them off.
func scheduleStatusLineReport(payload claudecfg.StatusLinePayload) {
	path := statusLineStatePath(payload.SessionID)
	last := readStatusLineReport(path)
	now := statusLineNow()
	within := func(at int64, d time.Duration) bool { return at != 0 && now.Sub(time.UnixMilli(at)) < d }
	next := last
	var due []string
	if id := payload.Model.ID; id != "" && (id != last.Model || !within(last.ModelAt, statusLineRefresh)) &&
		(id != last.ModelTried || !within(last.ModelTriedAt, statusLineRetry)) {
		due = append(due, "model")
		next.ModelTried, next.ModelTriedAt = id, now.UnixMilli()
	}
	if len(payload.RateLimits) > 0 {
		hash := limitsHash(payload.RateLimits)
		if (hash != last.LimitsHash || !within(last.LimitsAt, statusLineRefresh)) &&
			(hash != last.LimitsTried || !within(last.LimitsTriedAt, statusLineRetry)) {
			due = append(due, "limits")
			next.LimitsTried, next.LimitsTriedAt = hash, now.UnixMilli()
		}
	}
	if len(due) == 0 {
		return
	}
	writeStatusLineReport(path, next)
	startStatusLineReport(payload, due)
}

// spawnStatusLineReport runs the report in a detached child of this binary,
// passing the few observed values as arguments. The child shares none of the
// status-line command's stdio, so Claude never waits on it, and outlives the
// command (Claude may kill that once the line is drawn); its own report
// timeouts bound it.
func spawnStatusLineReport(payload claudecfg.StatusLinePayload, due []string) {
	self, err := os.Executable()
	if err != nil {
		return
	}
	cmd := exec.Command(self, "agent", "model", statusLineReportFlag,
		strings.Join(due, ","), payload.SessionID, payload.Model.ID, string(payload.RateLimits))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if cmd.Start() == nil {
		_ = cmd.Process.Release()
	}
}

// runStatusLineReport is the detached child: deliver the due observations and
// record each one the daemon accepted.
func runStatusLineReport(args []string) {
	if len(args) != 4 {
		return
	}
	payload := claudecfg.StatusLinePayload{SessionID: args[1], RateLimits: json.RawMessage(args[3])}
	payload.Model.ID = args[2]
	if len(payload.RateLimits) != 0 && !json.Valid(payload.RateLimits) {
		payload.RateLimits = nil
	}
	reportStatusLine(payload, strings.Split(args[0], ","))
}

func reportStatusLine(payload claudecfg.StatusLinePayload, due []string) {
	var delivered statusLineReport
	at := statusLineNow().UnixMilli()
	for _, what := range due {
		switch what {
		case "model":
			if payload.Model.ID != "" && reportWithin(true, sessionreport.Model,
				map[string]string{sessionreport.FieldModel: payload.Model.ID}) == nil {
				delivered.Model, delivered.ModelAt = payload.Model.ID, at
			}
		case "limits":
			if len(payload.RateLimits) > 0 && reportWithin(true, sessionreport.Limits,
				map[string]string{sessionreport.FieldLimits: string(payload.RateLimits)}) == nil {
				delivered.LimitsHash, delivered.LimitsAt = limitsHash(payload.RateLimits), at
			}
		}
	}
	if delivered.ModelAt == 0 && delivered.LimitsAt == 0 {
		return
	}
	path := statusLineStatePath(payload.SessionID)
	current := readStatusLineReport(path)
	if delivered.ModelAt != 0 {
		current.Model, current.ModelAt = delivered.Model, delivered.ModelAt
	}
	if delivered.LimitsAt != 0 {
		current.LimitsHash, current.LimitsAt = delivered.LimitsHash, delivered.LimitsAt
	}
	writeStatusLineReport(path, current)
}

func limitsHash(limits json.RawMessage) string {
	sum := sha256.Sum256(limits)
	return hex.EncodeToString(sum[:])
}

// statusLineStatePath names one conversation's rate-limit record. The Claude
// session id only partitions the cache; it never selects report authority.
func statusLineStatePath(conversation string) string {
	dir := statusLineStateDir()
	if dir == "" || conversation == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(conversation))
	return filepath.Join(dir, hex.EncodeToString(sum[:8])+".json")
}

func readStatusLineReport(path string) statusLineReport {
	var r statusLineReport
	if path != "" {
		if b, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(b, &r)
		}
	}
	return r
}

func writeStatusLineReport(path string, r statusLineReport) {
	if path == "" {
		return
	}
	b, err := json.Marshal(r)
	if err != nil || os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".report-*")
	if err != nil {
		return
	}
	_, werr := tmp.Write(b)
	if cerr := tmp.Close(); werr != nil || cerr != nil || os.Rename(tmp.Name(), path) != nil {
		_ = os.Remove(tmp.Name())
	}
}
