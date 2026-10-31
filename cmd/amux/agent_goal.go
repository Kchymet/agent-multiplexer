package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"amux/internal/claudegoal"
	"amux/internal/core"
	"amux/internal/sessionreport"
)

// goalNow is the clock goal transitions are stamped with (a test seam).
var goalNow = time.Now

// cmdAgentGoal is goal mode for a Claude workgroup coordinator: the two hook
// bindings that establish and continue the goal, and the model's own
// transitions. The state file is the coordinator's own session-local state; the
// user's pause/resume/clear goes through the host `goal` steer verb instead.
func cmdAgentGoal(args []string) error {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
		args = args[1:]
	}
	hook := len(args) == 1 && args[0] == "--hook"
	switch sub {
	case "prompt":
		if !hook {
			return fmt.Errorf("amux agent goal prompt: only the generated Claude hook runs this (--hook)")
		}
		goalPromptHook()
		return nil
	case "stop":
		if !hook {
			return fmt.Errorf("amux agent goal stop: only the generated Claude hook runs this (--hook)")
		}
		goalStopHook()
		return nil
	case "", "show", "status":
		return goalShow(args)
	case "complete", "done":
		return goalMark(claudegoal.StatusComplete, args)
	case "ask", "input":
		return goalMark(claudegoal.StatusInput, args)
	case "wait", "waiting":
		return goalMark(claudegoal.StatusWaiting, args)
	case "blocked":
		return goalMark(claudegoal.StatusBlocked, args)
	case "help", "-h", "--help":
		goalUsage()
		return nil
	default:
		goalUsage()
		return fmt.Errorf("amux agent goal: no goal action %q (show | complete | ask | wait | blocked)", sub)
	}
}

func goalUsage() {
	fmt.Fprint(os.Stderr, `amux agent goal — goal mode for a Claude workgroup coordinator

Every user task becomes the coordinator's active goal, and amux continues the
turn until the goal is complete, the coordinator asks the user for input, it
reports itself blocked, or the user pauses it.

usage: amux agent goal [show [--json]]
       amux agent goal complete "<summary>"    the objective is done and verified
                                               (archive finished members first: PR
                                               merged or closed; it lists idle
                                               unarchived members)
       amux agent goal ask "<question>"        stop and wait for the user's answer
       amux agent goal wait "<waiting on>"     end this turn only; an armed background
                                               wake-up (Monitor / run_in_background)
                                               resumes the goal. Never wait without one.
                                               Name the member/PR awaited and the wake-up;
                                               it must cover every open member PR.
       amux agent goal blocked "<reason>"      a repeating blocker only the user can clear

The user pauses, resumes or clears the goal from the host:
  amux do steer <coordinator> -f verb=goal -f status=paused|active|complete|clear
`)
}

func goalFile() (string, error) {
	path := strings.TrimSpace(os.Getenv(claudegoal.EnvFile))
	if path == "" {
		return "", fmt.Errorf("this session is not in Claude goal mode ($%s is unset; goal mode is a Claude workgroup coordinator's)", claudegoal.EnvFile)
	}
	return path, nil
}

func readGoal(path string) (claudegoal.State, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return claudegoal.State{}, nil
	}
	if err != nil {
		return claudegoal.State{}, err
	}
	return claudegoal.Decode(b)
}

func writeGoal(path string, s claudegoal.State) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".goal-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(claudegoal.Encode(s)); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func goalShow(args []string) error {
	asJSON := len(args) == 1 && args[0] == "--json"
	if len(args) > 0 && !asJSON {
		return fmt.Errorf("amux agent goal show: unexpected argument %q", args[0])
	}
	path, err := goalFile()
	if err != nil {
		return err
	}
	s, err := readGoal(path)
	if err != nil {
		return err
	}
	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(s)
	}
	fmt.Println(claudegoal.Summary(s))
	return nil
}

func goalMark(status string, args []string) error {
	note := strings.TrimSpace(strings.Join(args, " "))
	if note == "" && status != claudegoal.StatusComplete {
		return fmt.Errorf("amux agent goal: say what you need (the question, what you are waiting on, or the blocker)")
	}
	path, err := goalFile()
	if err != nil {
		return err
	}
	s, err := readGoal(path)
	if err != nil {
		return err
	}
	if s, err = claudegoal.Mark(s, status, note, goalNow()); err != nil {
		return err
	}
	if err := writeGoal(path, s); err != nil {
		return err
	}
	switch status {
	case claudegoal.StatusComplete:
		fmt.Println("goal complete: give your final summary, then end the turn. The workgroup stays open.")
		fmt.Print(finishedMemberReminder())
	case claudegoal.StatusInput:
		fmt.Println("goal waiting for input: ask the user your question and end the turn; their reply resumes the goal.")
	case claudegoal.StatusWaiting:
		fmt.Println("goal waiting: end the turn now; the goal resumes in the turn your armed wake-up starts. If no wake-up is armed, arm one before ending the turn.")
	case claudegoal.StatusBlocked:
		fmt.Println("goal blocked: explain the blocker and end the turn; the user's next task or a resume continues.")
	}
	return nil
}

// goalHookPayload is the subset of the UserPromptSubmit / Stop hook JSON goal
// mode reads: the submitted prompt.
type goalHookPayload struct {
	HookEventName string `json:"hook_event_name"`
	Prompt        string `json:"prompt"`
}

// oversizedTask stands in for a task too large for the bounded hook input. The
// full text is in Claude's conversation either way.
const oversizedTask = "(the user's task is too long to restate here; see the conversation)"

// goalPromptHook establishes the user's task as the goal. Like every generated
// hook it never disrupts the agent: any failure leaves the goal as it was.
func goalPromptHook() {
	path, err := goalFile()
	if err != nil {
		return
	}
	b, overflow, err := readBoundedHookInput()
	if err != nil {
		return
	}
	var payload goalHookPayload
	prompt := oversizedTask
	if !overflow {
		if decodeHookJSON(b, &payload) != nil {
			return
		}
		prompt = payload.Prompt
	}
	s, err := readGoal(path)
	if err != nil {
		return
	}
	_ = writeGoal(path, claudegoal.OnPrompt(s, prompt, goalNow()))
}

// goalStopHook decides whether Claude may end its turn. While the goal is
// active it prints Claude's block decision with the continuation prompt; in
// every case it reports the activity the turn now has, replacing the plain Stop
// status hook (claudecfg.goalHooks). A failure lets the turn end: a broken goal
// loop must never trap the agent in an unending turn.
func goalStopHook() {
	state := core.StateReady
	defer func() {
		// Bounded like every hook report: Claude waits for this blocking hook,
		// so a busy daemon must not hold the turn end for the full CLI timeout.
		_ = reportWithin(true, sessionreport.Activity, map[string]string{sessionreport.FieldState: state})
	}()
	path, err := goalFile()
	if err != nil {
		return
	}
	if _, _, err := readBoundedHookInput(); err != nil {
		return
	}
	s, err := readGoal(path)
	if err != nil {
		return
	}
	next, reason, cont := claudegoal.OnStop(s, goalNow())
	if next != s {
		if err := writeGoal(path, next); err != nil {
			return
		}
	}
	if !cont {
		return
	}
	out, err := json.Marshal(map[string]string{"decision": "block", "reason": reason})
	if err != nil {
		return
	}
	state = core.StateRunning
	fmt.Println(string(out))
}

// finishedMemberReminder is what `goal complete` adds when the coordinator
// still has idle, unarchived direct members: finished members should be
// archived before the task is reported complete. It reads the coordinator's own
// scoped snapshot; any failure prints nothing, because a reminder must never
// block or fail completion.
func finishedMemberReminder() string {
	if !sessionContextRestricted() {
		return ""
	}
	var sessions []core.Session
	if err := queryRows(core.QuerySnapshot, &sessions); err != nil {
		return ""
	}
	return idleMemberReminder(sessions)
}

// idleMemberReminder lists the direct members in a coordinator's scoped
// snapshot that are neither archived nor running. The scoped snapshot holds
// only this workgroup, so a row with a root is one of its members. Members the
// daemon's PR watcher still sees owning an open PR are listed apart, with no
// archive command: archiving one orphans its PR.
func idleMemberReminder(sessions []core.Session) string {
	var archivable, openPR []core.Session
	for _, s := range sessions {
		if s.IsRoot || s.RootID == "" || s.Archived || s.State == core.StateRunning {
			continue
		}
		if ownsOpenPR(s) {
			openPR = append(openPR, s)
		} else {
			archivable = append(archivable, s)
		}
	}
	if len(archivable) == 0 && len(openPR) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\nreminder: %d member(s) are not running but still unarchived.\n", len(archivable)+len(openPR))
	if len(archivable) > 0 {
		b.WriteString("Archive each one that is finished: its PR is merged or closed (or its task was\n")
		b.WriteString("delivered with no PR) and you won't reassign it:\n")
		for _, s := range archivable {
			fmt.Fprintf(&b, "  %-10s %-8s %s\n", s.ID, memberState(s), s.Title)
			fmt.Fprintf(&b, "    amux do set-archived %s -f archived=true\n", s.ID)
		}
	}
	if len(openPR) > 0 {
		b.WriteString("Do not archive these: each still owns an open PR. Keep them babysitting it\n")
		b.WriteString("until it is merged or closed:\n")
		for _, s := range openPR {
			fmt.Fprintf(&b, "  %-10s %-8s %s  (%s)\n", s.ID, memberState(s), s.Title, strings.Join(openPRURLs(s), ", "))
		}
	}
	b.WriteString("Keep a finished member only if you expect to reuse it, and say why in your final summary.\n")
	return b.String()
}

func memberState(s core.Session) string {
	if s.State == "" {
		return core.StateIdle
	}
	return s.State
}

func ownsOpenPR(s core.Session) bool { return len(openPRURLs(s)) > 0 }

func openPRURLs(s core.Session) []string {
	if s.PRs == nil {
		return nil
	}
	var urls []string
	for _, pr := range *s.PRs {
		if pr.State == "open" {
			urls = append(urls, pr.URL)
		}
	}
	return urls
}
