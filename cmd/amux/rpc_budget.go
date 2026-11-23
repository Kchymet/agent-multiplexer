package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"amux/internal/core"
	"amux/internal/logrotate"
)

// queryRPCBudget fetches the daemon's session RPC response budget. ok is false
// for a daemon that predates the query or is unreachable.
func queryRPCBudget() (core.RPCBudget, bool) {
	var b core.RPCBudget
	if sessionContextRestricted() {
		return b, false
	}
	if err := queryRows(core.QueryRPCBudget, &b); err != nil || b.MaxFiles == 0 {
		return b, false
	}
	return b, true
}

// rpcBudgetLines renders the budget for top and doctor: usage against the
// ceiling, then what was refused and by whom.
func rpcBudgetLines(b core.RPCBudget, now time.Time) []string {
	pct := 0
	if b.MaxFiles > 0 {
		pct = b.Files * 100 / b.MaxFiles
	}
	lines := []string{fmt.Sprintf("rpc budget  %d/%d responses (%d%%) · %d sessions · %s · %d admitted · %d refused",
		b.Files, b.MaxFiles, pct, b.Sessions, classCounts(b.ByPriority), b.Admitted, b.Rejected)}
	if b.Rejected == 0 {
		return lines
	}
	last := ""
	if b.LastRejectedAt > 0 {
		last = fmt.Sprintf(", last %s ago", now.Sub(time.UnixMilli(b.LastRejectedAt)).Round(time.Second))
	}
	lines = append(lines, fmt.Sprintf("  refused by class %s%s", topCounts(b.RejectedByPriority, 3), last))
	lines = append(lines, fmt.Sprintf("  refused by verb %s", topCounts(b.RejectedByVerb, 5)))
	lines = append(lines, fmt.Sprintf("  refused by session %s", topCounts(b.RejectedBySession, 5)))
	return lines
}

func classCounts(m map[string]int) string {
	var parts []string
	for _, class := range []string{"control", "normal", "telemetry"} {
		parts = append(parts, fmt.Sprintf("%s %d", class, m[class]))
	}
	return strings.Join(parts, " ")
}

func topCounts(m map[string]uint64, n int) string {
	type kv struct {
		k string
		v uint64
	}
	var all []kv
	for k, v := range m {
		all = append(all, kv{k, v})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].v != all[j].v {
			return all[i].v > all[j].v
		}
		return all[i].k < all[j].k
	})
	if len(all) > n {
		all = all[:n]
	}
	var parts []string
	for _, e := range all {
		parts = append(parts, fmt.Sprintf("%s=%d", e.k, e.v))
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, " ")
}

// daemonLogLines warns when the daemon log and its generations outgrow what
// rotation should allow: rotation not acting, or legacy generations it has not
// pruned yet. Silent when the log is within bounds.
func daemonLogLines(path string) []string {
	live, rotated := logrotate.Usage(path)
	var lines []string
	if live > 2*logrotate.DefaultMaxBytes {
		lines = append(lines, fmt.Sprintf("  ⚠ log       %s is %s (rotation expected at %s; is the daemon run from a terminal?)",
			path, mib(live), mib(logrotate.DefaultMaxBytes)))
	}
	if rotated > logrotate.DefaultMaxTotal+logrotate.DefaultMaxBytes {
		lines = append(lines, fmt.Sprintf("  ⚠ log       rotated daemon logs take %s (bound %s); the daemon trims and compresses them within a minute of start",
			mib(rotated), mib(logrotate.DefaultMaxTotal)))
	}
	return lines
}

func mib(n int64) string { return fmt.Sprintf("%d MiB", n>>20) }
