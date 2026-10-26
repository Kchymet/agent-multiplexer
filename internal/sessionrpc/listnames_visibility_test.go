package sessionrpc

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestHeldDirectoryListingSeesEveryPublishAndClaim probes the scan the server
// relies on: a long-held directory descriptor rewound with listNames must list
// an entry published (temporary + no-replace rename, as the client does) since
// its previous scan, and must stop listing an entry claimed out of it (rename
// into processing, as ServeOnce does). It mirrors the server's open path and
// keeps a small resident queue, over many iterations, so a stale or missed
// listing on any platform shows up as a counted failure rather than a rare CI
// symptom.
func TestHeldDirectoryListingSeesEveryPublishAndClaim(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{RequestsDirName, processingDirName} {
		if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	mailbox, err := openAbsoluteDirNoFollow(root)
	if err != nil {
		t.Fatal(err)
	}
	defer mailbox.Close()
	open := func(name string) *os.File {
		t.Helper()
		dir, err := openDirAt(mailbox, name)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = dir.Close() })
		return dir
	}
	// Server-held scan descriptors, and the client's separate writer.
	requests, processing, writer := open(RequestsDirName), open(processingDirName), open(RequestsDirName)

	const iterations = 3000
	missedPublish, staleClaim, missedClaimed := 0, 0, 0
	var resident []string
	for i := 0; i < iterations; i++ {
		// Scan first so each publish lands after the held stream was used.
		if _, _, err := listNames(requests, MaxQueuedRequests); err != nil {
			t.Fatal(err)
		}
		name := requestFileName(fmt.Sprintf("%032x", i+1))
		if err := atomicWriteAt(writer, name, []byte("{}"), rand.Reader); err != nil {
			t.Fatal(err)
		}
		resident = append(resident, name)
		listed, _, err := listNames(requests, MaxQueuedRequests)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range resident {
			if !slices.Contains(listed, want) {
				missedPublish++
			}
		}
		// Keep up to three queued; claim the oldest beyond that.
		if len(resident) <= 3 {
			continue
		}
		claimed := resident[0]
		resident = resident[1:]
		if err := renameNoReplace(int(requests.Fd()), claimed, int(processing.Fd()), claimed); err != nil {
			t.Fatal(err)
		}
		listed, _, err = listNames(requests, MaxQueuedRequests)
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(listed, claimed) {
			staleClaim++
		}
		inProcessing, _, err := listNames(processing, MaxQueuedRequests)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(inProcessing, claimed) {
			missedClaimed++
		}
		if err := unlinkAt(processing, claimed); err != nil {
			t.Fatal(err)
		}
	}
	if missedPublish != 0 || staleClaim != 0 || missedClaimed != 0 {
		t.Fatalf("held-descriptor scans over %d iterations: %d published entries missed, %d claimed entries still listed, %d claimed entries missing from processing",
			iterations, missedPublish, staleClaim, missedClaimed)
	}
}
