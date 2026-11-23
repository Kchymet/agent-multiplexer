package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"amux/internal/core"
	"amux/internal/prwatch"
	"amux/internal/sessionrpc"
)

const agentWaitUsage = "usage: amux agent wait <session-id>... [--until <state>[,<state>...]] [--timeout <duration>] [--json]"

// errWaitTimeout is cmdAgentWait's --timeout expiry (a nonzero exit).
var errWaitTimeout = errors.New("amux agent wait: timed out with no change")

// waitQuery runs one QueryWait call; a variable so tests can fake the daemon.
var waitQuery = func(id, since string, timeoutSeconds int) (core.WaitResult, error) {
	fields := map[string]string{core.WaitTimeoutField: strconv.Itoa(timeoutSeconds)}
	if since != "" {
		fields[core.WaitSinceField] = since
	}
	var res core.WaitResult
	err := restrictedQueryRequest(sessionrpc.Query{Verb: core.QueryWait, ID: id, Fields: fields}, &res)
	return res, err
}

// cmdAgentWait blocks until one of the named sessions changes — its state,
// archived flag or PRs — and prints it. It is the event-driven replacement for
// a sleep loop over `amux agent events` or `gh pr view`: each call parks on the
// daemon's own state broadcast and returns the moment the session moves, then
// re-arms with the returned token, so no change between calls is missed and
// nothing polls. With --until it keeps waiting until a target reaches one of
// the given states (exiting at once if one already has).
func cmdAgentWait(args []string) error {
	var targets, until []string
	var overall time.Duration
	asJSON := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--json":
			asJSON = true
		case a == "--until" && i+1 < len(args):
			until = splitStates(args[i+1])
			i++
		case strings.HasPrefix(a, "--until="):
			until = splitStates(strings.TrimPrefix(a, "--until="))
		case a == "--timeout" && i+1 < len(args):
			d, err := time.ParseDuration(args[i+1])
			if err != nil || d <= 0 {
				return fmt.Errorf("--timeout must be a positive duration like 10m")
			}
			overall = d
			i++
		case strings.HasPrefix(a, "--timeout="):
			d, err := time.ParseDuration(strings.TrimPrefix(a, "--timeout="))
			if err != nil || d <= 0 {
				return fmt.Errorf("--timeout must be a positive duration like 10m")
			}
			overall = d
		case a == "-h" || a == "--help":
			fmt.Fprintln(os.Stderr, agentWaitUsage)
			return nil
		case strings.HasPrefix(a, "-"):
			return fmt.Errorf("unknown wait option %q\n%s", a, agentWaitUsage)
		default:
			targets = append(targets, a)
		}
	}
	if len(targets) == 0 {
		return fmt.Errorf("%s", agentWaitUsage)
	}
	for _, s := range until {
		switch s {
		case core.StateStopped, core.StateReady, core.StateWaiting, core.StateRunning, core.StateUnknown:
		default:
			return fmt.Errorf("--until: unknown state %q (stopped | ready | waiting | running | unknown)", s)
		}
	}
	ctx := context.Background()
	if overall > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, overall)
		defer cancel()
	}
	res, err := waitAny(ctx, targets, until)
	if err != nil {
		return err
	}
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}
	fmt.Println(describeWait(res))
	return nil
}

func splitStates(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			if p == core.StateIdle {
				p = core.StateStopped // the legacy name for a session with no runtime
			}
			out = append(out, p)
		}
	}
	return out
}

type waitOutcome struct {
	res core.WaitResult
	err error
}

// waitAny waits on every target concurrently and returns the first that
// satisfies the wait.
func waitAny(ctx context.Context, targets, until []string) (core.WaitResult, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	out := make(chan waitOutcome, len(targets))
	for _, id := range targets {
		go func(id string) {
			res, err := waitOne(ctx, id, until)
			out <- waitOutcome{res, err}
		}(id)
	}
	var firstErr error
	for range targets {
		o := <-out
		if o.err == nil {
			return o.res, nil
		}
		if errors.Is(o.err, context.Canceled) || errors.Is(o.err, context.DeadlineExceeded) {
			continue
		}
		if firstErr == nil {
			firstErr = o.err
			cancel() // one target failing (denied, daemon too old) fails the wait
		}
	}
	if firstErr != nil {
		return core.WaitResult{}, firstErr
	}
	return core.WaitResult{}, errWaitTimeout
}

// waitOne waits on one target until it changes (or, with until, reaches one of
// those states). Each round trip blocks in the daemon for at most the time the
// overall deadline leaves, capped by core.MaxWaitSeconds.
func waitOne(ctx context.Context, id string, until []string) (core.WaitResult, error) {
	since := ""
	if len(until) > 0 {
		cur, err := waitQuery(id, "", 0)
		if err != nil {
			return cur, waitError(id, err)
		}
		if stateIn(cur.State, until) {
			return cur, nil
		}
		since = cur.Token
	}
	for {
		if err := ctx.Err(); err != nil {
			return core.WaitResult{}, err
		}
		seconds := core.MaxWaitSeconds
		if deadline, ok := ctx.Deadline(); ok {
			left := int(time.Until(deadline) / time.Second)
			if left < 1 {
				return core.WaitResult{}, context.DeadlineExceeded
			}
			seconds = min(seconds, left)
		}
		res, err := waitQuery(id, since, seconds)
		if err != nil {
			return res, waitError(id, err)
		}
		if res.Changed && (len(until) == 0 || stateIn(res.State, until)) {
			return res, nil
		}
		since = res.Token
	}
}

func stateIn(state string, set []string) bool {
	for _, s := range set {
		if s == state {
			return true
		}
	}
	return false
}

func waitError(id string, err error) error {
	var remote *sessionrpc.RemoteError
	if errors.As(err, &remote) && remote.Code == "invalid_operation" {
		return fmt.Errorf("wait on %s: %w (is the daemon older than this CLI? restart it to pick up `amux agent wait`)", id, err)
	}
	return fmt.Errorf("wait on %s: %w", id, err)
}

// describeWait is the plain-text result: id, state, and a PR summary.
func describeWait(res core.WaitResult) string {
	if !res.Present {
		return res.ID + "\tgone"
	}
	line := res.ID + "\t" + res.State
	if res.Archived {
		line += "\tarchived"
	}
	if s := prwatch.Summary(res.PRs); s != "" {
		line += "\t" + s
	}
	return line
}
