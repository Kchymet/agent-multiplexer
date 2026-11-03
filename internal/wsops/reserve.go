package wsops

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"amux/internal/core"
	"amux/internal/store"
)

// reserve.go keeps two creations from ever preparing the same session id at
// once. A minted id keys more than its row and its directory: the agent's
// pooled-worktree layout records (gitLayoutPath) and its config seed manifest
// (cfghome) live in the state directory keyed by the id alone, across every
// workgroup. A creation therefore holds an exclusive reservation of its id —
// an exclusive mkdir, so it holds across processes (the daemon and a CLI) —
// from allocation until its row is inserted or its preparation cleaned up.
// Only while it holds the reservation, and only when no row with the id exists,
// may a refused creation remove records keyed by the id.
//
// Limitation: a process that crashes while holding a reservation leaves it
// behind; that id is then never minted again (the id space is 24 bits and
// NewID retries), which is harmless. Session ids minted elsewhere (a repo
// home's id is its repo name) do not reserve; the ownership check below keeps
// a refused creation from removing their records.

func reservationDir() string { return filepath.Join(core.StateDir(), "id-reservations") }

// maxReserveAttempts bounds reserveID's retries.
const maxReserveAttempts = 16

// reserveID mints an id that no row has and no other creation holds, and
// reserves it. release is idempotent.
func reserveID(db *store.DB) (string, func(), error) {
	dir := reservationDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, err
	}
	for range maxReserveAttempts {
		id := newAgentID(db)
		if id == "" || filepath.Base(id) != id || id == "." || id == ".." {
			return "", nil, fmt.Errorf("invalid minted session id %q", id)
		}
		path := filepath.Join(dir, id)
		switch err := os.Mkdir(path, 0o700); {
		case errors.Is(err, os.ErrExist):
			continue // another creation is preparing this id
		case err != nil:
			return "", nil, err
		}
		released := false
		release := func() {
			if !released {
				released = true
				_ = os.Remove(path)
			}
		}
		// A creation that released this id may have inserted it since it was
		// minted; reserving does not make a taken id free.
		cur, err := db.SessionIdentities([]string{id})
		if err != nil {
			release()
			return "", nil, err
		}
		if _, taken := cur[id]; taken {
			release()
			continue
		}
		return id, release, nil
	}
	return "", nil, fmt.Errorf("could not reserve a new session id")
}
