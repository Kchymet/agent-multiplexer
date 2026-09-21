package codexapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"amux/internal/agent"
)

// RestartWork is host-owned evidence of work running before shutdown. A goal's
// identity is retained without copying its objective into the restart journal.
type RestartWork struct {
	ThreadID string `json:"threadId"`
	GoalKey  string `json:"goalKey,omitempty"`
	Continue bool   `json:"continue,omitempty"`
}

type threadGoal struct {
	ThreadID  string `json:"threadId"`
	Objective string `json:"objective"`
	Status    string `json:"status"`
	CreatedAt int64  `json:"createdAt"`
}

func (g *threadGoal) key() string {
	if g == nil {
		return ""
	}
	data, _ := json.Marshal([]any{g.ThreadID, g.Objective, g.CreatedAt})
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

// RestartWork reads observed state even after the transport was retired for
// daemon shutdown. Explicit pauses/clears are observed while it is live.
func (s *Supervisor) RestartWork() RestartWork {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return RestartWork{}
	}
	if s.restartWork != nil {
		return *s.restartWork
	}
	return s.restartWorkLocked()
}

func (s *Supervisor) restartWorkLocked() RestartWork {
	w := RestartWork{ThreadID: s.threadID}
	if s.goal != nil {
		if s.goal.Status == "active" {
			w.GoalKey = s.goal.key()
		}
	} else if s.curTurn != "" && len(s.approvals.open()) == 0 {
		w.Continue = true
	}
	return w
}

func isRPCUnsupported(err error) bool {
	var rpc *rpcError
	return errors.As(err, &rpc) && (rpc.Code == -32601 || strings.Contains(strings.ToLower(rpc.Message), "goals feature is disabled"))
}

func (s *Supervisor) observeGoal(method string, params json.RawMessage) {
	if method != "thread/goal/updated" && method != "thread/goal/cleared" {
		return
	}
	var p struct {
		ThreadID string      `json:"threadId"`
		Goal     *threadGoal `json:"goal"`
	}
	if json.Unmarshal(params, &p) != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.ThreadID != s.threadID || s.closed || s.interrupted {
		return
	}
	s.goal = p.Goal
	s.goalRevision++
}

func (s *Supervisor) restoreGoal(ctx context.Context, resumed bool) error {
	s.mu.Lock()
	revision := s.goalRevision
	s.mu.Unlock()
	raw, err := s.rpc.call(ctx, "thread/goal/get", map[string]any{"threadId": s.ThreadID()})
	if err != nil {
		// Older Codex builds and installations with goals disabled remain usable.
		if !isRPCUnsupported(err) {
			return fmt.Errorf("codexapp read goal: %w", err)
		}
		raw = json.RawMessage(`{"goal":null}`)
	}
	var result struct {
		Goal *threadGoal `json:"goal"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("codexapp decode goal: %w", err)
	}
	s.mu.Lock()
	if s.goalRevision == revision {
		s.goal = result.Goal
	}
	goal := s.goal
	s.mu.Unlock()
	w := s.cfg.RestartWork
	if !resumed || w == nil || w.ThreadID != s.ThreadID() {
		return nil
	}
	if goal != nil {
		if w.GoalKey != "" && w.GoalKey == goal.key() && goal.Status == "paused" {
			// Change status only: preserve the objective, budget and usage. Never
			// reactivate blocked, completed or limited goals, or a different goal.
			if _, err := s.rpc.call(ctx, "thread/goal/set", map[string]any{"threadId": w.ThreadID, "status": "active"}); err != nil {
				return fmt.Errorf("codexapp resume interrupted goal: %w", err)
			}
		}
		// Active goals are continued by Codex itself during thread/resume.
		return nil
	}
	if w.Continue && w.GoalKey == "" {
		_, err := s.rpc.call(ctx, "turn/start", map[string]any{"threadId": w.ThreadID, "input": inputBlocks(agent.ResumeWorkPrompt)})
		if err != nil {
			return fmt.Errorf("codexapp continue interrupted work: %w", err)
		}
	}
	return nil
}
