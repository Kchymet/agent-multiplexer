package main

import (
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"time"

	"amux/internal/direct"
)

// directConnsRecent is how many recent attempts `amux device status` prints
// (the JSON report carries all of them).
const directConnsRecent = 5

// printDirectConnections prints the running listener's connection telemetry
// for `amux device status`: counters since it started, the listen address
// check, refresh outcomes and the latest recorded attempts.
func printDirectConnections(st *direct.ConnStats) { writeDirectConnections(os.Stdout, st) }

func writeDirectConnections(w io.Writer, st *direct.ConnStats) {
	if st == nil {
		return // an older server: no telemetry
	}
	fmt.Fprintf(w, "          connections since %s: %s\n", time.Unix(st.Since, 0).Format(time.RFC3339), st)
	if len(st.StreamEnds) > 0 {
		fmt.Fprintf(w, "          stream ends: %s\n", countsLine(st.StreamEnds))
	}
	if l := st.Listen; l.Addr != "" && !l.Present {
		fmt.Fprintf(w, "          ⚠ listen address %s is not assigned to this host (network changed at %s); clients cannot reach it\n",
			l.Addr, time.Unix(l.ChangedAt, 0).Format(time.RFC3339))
	}
	rf := st.Refresh
	fmt.Fprintf(w, "          refresh ok %d · grace retries %d · reuse revoked %d · expired %d · unknown %d\n",
		rf.OK, rf.GraceRetry, rf.Reused, rf.Expired, rf.Unknown)
	recent := st.Recent
	if len(recent) > directConnsRecent {
		recent = recent[len(recent)-directConnsRecent:]
	}
	for _, a := range recent {
		fmt.Fprintf(w, "          %s %s\n", time.UnixMilli(a.AtMS).Format(time.RFC3339), a)
	}
}

// countsLine renders a reason→count map in taxonomy order, then any other key.
func countsLine(m map[string]int64) string {
	s, seen := "", map[string]bool{}
	add := func(k string, n int64) {
		if n == 0 {
			return
		}
		if s != "" {
			s += ", "
		}
		s += fmt.Sprintf("%s %d", k, n)
	}
	for _, r := range direct.FailureReasons {
		seen[r] = true
		add(r, m[r])
	}
	for _, k := range slices.Sorted(maps.Keys(m)) {
		if !seen[k] {
			add(k, m[k])
		}
	}
	return s
}
