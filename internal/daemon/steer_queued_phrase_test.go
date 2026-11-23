package daemon

import (
	"strings"
	"testing"
)

// TestSteerQueuedPhraseMatchesDaemon: `amux send` (cmd/amux/verbs.go,
// steerQueuedPhrase) reports a steer queued behind a busy composer as success
// by recognizing this phrase in the error. Changing the wording breaks it.
func TestSteerQueuedPhraseMatchesDaemon(t *testing.T) {
	err := &composerBusyError{reason: "a draft is in the input box", queued: true}
	if !strings.Contains(err.Error(), "the message is queued") {
		t.Fatalf("queued composer_busy error %q no longer says %q", err.Error(), "the message is queued")
	}
	if strings.Contains((&composerBusyError{reason: "x"}).Error(), "the message is queued") {
		t.Fatal("an unqueued busy error claims the message is queued")
	}
}
