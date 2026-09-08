package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"sync"

	"amux/internal/runtimeevents"
	"amux/internal/store"
)

// runtimePermissionGate binds permission decisions to one live runtime
// incarnation. A request is claimed while the lock is held and before delivery;
// a delivery with an uncertain outcome therefore cannot be retried.
type runtimePermissionGate struct {
	mu       sync.Mutex
	random   io.Reader
	runtimes map[string]permissionRuntime
}

// permissionBaseline snapshots requests already open in durable history before
// a runtime identity is admitted. A replacement runtime must not inherit them.
func (d *Daemon) loadPermissionBaseline(subject string) ([]string, error) {
	db, err := store.Open()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rec, err := d.runtimeRecord(db, subject)
	if err != nil {
		return nil, err
	}
	open := runtimeevents.OpenPermissions(runtimeEventRecord(rec))
	ids := make([]string, 0, len(open))
	for _, pending := range open {
		ids = append(ids, pending.RequestID)
	}
	return ids, nil
}

type permissionRuntime struct {
	identity   string
	generation string
	claimed    map[string]struct{}
	// excluded contains requests already open when this runtime identity was
	// observed. They are historical (or raced the conservative boundary) and
	// must never be relabeled with this incarnation's generation.
	excluded map[string]struct{}
}

func newRuntimePermissionGate() *runtimePermissionGate {
	return &runtimePermissionGate{random: rand.Reader, runtimes: make(map[string]permissionRuntime)}
}

func (g *runtimePermissionGate) observe(subject string, runtime any) (string, error) {
	return g.observeExcluding(subject, runtime, nil)
}

func (g *runtimePermissionGate) observeExcluding(subject string, runtime any, excluded []string) (string, error) {
	if g == nil || strings.TrimSpace(subject) == "" || runtime == nil {
		return "", fmt.Errorf("permission runtime unavailable")
	}
	identity := fmt.Sprintf("%T:%p", runtime, runtime)
	g.mu.Lock()
	defer g.mu.Unlock()
	if current, ok := g.runtimes[subject]; ok && current.identity == identity {
		return current.generation, nil
	}
	nonce := make([]byte, 16)
	if _, err := io.ReadFull(g.random, nonce); err != nil {
		return "", fmt.Errorf("create runtime generation: %w", err)
	}
	generation := hex.EncodeToString(nonce)
	baseline := make(map[string]struct{}, len(excluded))
	for _, requestID := range excluded {
		if strings.TrimSpace(requestID) != "" {
			baseline[requestID] = struct{}{}
		}
	}
	g.runtimes[subject] = permissionRuntime{
		identity: identity, generation: generation, claimed: make(map[string]struct{}), excluded: baseline,
	}
	return generation, nil
}

// bindings returns answerable request->generation tuples from one atomic view
// of the current runtime gate. Requests that predate this incarnation or were
// already consumed are absent; callers must not invent a current-generation
// fallback for them.
func (g *runtimePermissionGate) bindings(subject, expectedGeneration string, open []runtimeevents.Pending) map[string]string {
	out := make(map[string]string)
	if g == nil {
		return out
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	runtime, ok := g.runtimes[subject]
	if !ok || expectedGeneration == "" || runtime.generation != expectedGeneration {
		return out
	}
	for _, pending := range open {
		if pending.RequestID == "" {
			continue
		}
		if _, excluded := runtime.excluded[pending.RequestID]; excluded {
			continue
		}
		if _, claimed := runtime.claimed[pending.RequestID]; claimed {
			continue
		}
		out[pending.RequestID] = runtime.generation
	}
	return out
}

func (g *runtimePermissionGate) generation(subject string) (string, bool) {
	if g == nil {
		return "", false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	runtime, ok := g.runtimes[subject]
	return runtime.generation, ok
}

func (g *runtimePermissionGate) retire(subject string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	delete(g.runtimes, subject)
	g.mu.Unlock()
}

func (g *runtimePermissionGate) consume(subject, generation, requestID string, validate, deliver func() error) error {
	if g == nil || strings.TrimSpace(generation) == "" || strings.TrimSpace(requestID) == "" || validate == nil || deliver == nil {
		return fmt.Errorf("permission requires a live runtime generation and request id")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	runtime, ok := g.runtimes[subject]
	if !ok || runtime.generation != generation {
		return fmt.Errorf("permission runtime generation is stale")
	}
	if _, duplicate := runtime.claimed[requestID]; duplicate {
		return fmt.Errorf("permission request %q was already consumed", requestID)
	}
	// Claim before delivery. Even an error may mean the runtime accepted the
	// decision before its acknowledgement was lost.
	runtime.claimed[requestID] = struct{}{}
	g.runtimes[subject] = runtime
	if err := validate(); err != nil {
		return err
	}
	return deliver()
}
