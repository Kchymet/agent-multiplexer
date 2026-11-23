package daemon

import (
	"log"
	"sync"
)

// maxLoggedNotices bounds how many distinct notices the daemon remembers having
// logged, so a session cannot grow the set without limit.
const maxLoggedNotices = 64

var noticeLog struct {
	mu   sync.Mutex
	seen map[string]bool
}

// logNotice records a runtime notification amux does not recognize — today a
// Claude Notification hook whose notification_type is new — in the daemon log,
// once per distinct notice per daemon run. It changes no state: an unrecognized
// notification is deliberately not read as "waiting" (see
// claudecfg.ClassifyNotification), and this line is how a new type gets noticed
// and classified.
func logNotice(sessionID, notice string) {
	noticeLog.mu.Lock()
	defer noticeLog.mu.Unlock()
	if noticeLog.seen[notice] {
		return
	}
	if noticeLog.seen == nil {
		noticeLog.seen = map[string]bool{}
	}
	if len(noticeLog.seen) >= maxLoggedNotices {
		return
	}
	noticeLog.seen[notice] = true
	log.Printf("amux: session %s: unrecognized runtime notification %q (state unchanged; classify it in claudecfg.ClassifyNotification)", sessionID, notice)
}
