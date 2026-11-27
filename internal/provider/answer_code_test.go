package provider

import (
	"context"
	"net"
	"testing"

	"amux/internal/core"
	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// codedRejection stands in for daemon.RejectedError: a daemon refusal that
// carries a stable failure class.
type codedRejection struct{ msg, code string }

func (e *codedRejection) Error() string      { return e.msg }
func (e *codedRejection) ResultCode() string { return e.code }

// TestSessionResultCarriesRejectionCode pins that a refused relayed answer
// keeps the daemon's failure class (it-fadb4071): the cloud relay tells
// "already answered" from "gone" from "answer in the pane" by code.
func TestSessionResultCarriesRejectionCode(t *testing.T) {
	for _, code := range []string{"answered", "asker_gone", harnessproto.CodeNeedsNewerClient, harnessproto.CodeDeliveryFailed, harnessproto.CodeAnswerInPane, ""} {
		t.Run("code="+code, func(t *testing.T) {
			conns := make(chan net.Conn, 1)
			src := &mutableSource{}
			apply := func(context.Context, core.Action) (string, error) {
				return "", &codedRejection{msg: "refused", code: code}
			}
			p := newFast(Config{Orchestrator: "pipe", Dial: pipeDialer(conns), PublishSessions: true, Sessions: src.poll, ApplyAction: apply})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go p.Run(ctx)
			oc := harnessproto.NewConn(<-conns)
			accept(t, oc, 2, nil, 60)
			caps := core.SessionCaps{Prompt: true, Question: true}
			publishRows(t, oc, src, core.Session{ID: "a1", Caps: &caps})
			if err := oc.WriteMux(harnessproto.MuxMsg{Type: harnessproto.MSessionAction, ReqID: "r9", Action: harnessproto.VerbAnswer, ID: "a1",
				Fields: map[string]string{harnessproto.FieldRequestID: "q-aaaaaaaaaa", harnessproto.FieldAnswer: `{"version":2,"action":"cancel"}`}}); err != nil {
				t.Fatal(err)
			}
			res := readResult(t, oc)
			if res.OK || res.Error != "refused" || res.Code != code {
				t.Fatalf("result = %+v, want error with code %q", res, code)
			}
		})
	}
}
