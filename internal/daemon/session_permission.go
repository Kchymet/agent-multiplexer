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
	rec, err := d.runtimeRecordUnbound(db, subject)
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
	requests   map[string]struct{}
	claimed    map[string]struct{}
	// excluded contains requests already open when this runtime identity was
	// observed. They are historical (or raced the conservative boundary) and
	// must never be relabeled with this incarnation's generation.
	excluded map[string]struct{}
}

type permissionRuntimeToken struct {
	identity   string
	generation string
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
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.observeLocked(subject, runtime, excluded)
}

func (g *runtimePermissionGate) observeLocked(subject string, runtime any, excluded []string) (string, error) {
	identity := permissionRuntimeIdentity(runtime)
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
		identity: identity, generation: generation,
		requests: make(map[string]struct{}), claimed: make(map[string]struct{}), excluded: baseline,
	}
	return generation, nil
}

// publish makes runtime visibility and generation rotation one boundary. The
// creator runs while the same gate lock that guards consumption is held, so no
// old-generation decision can cross Engine.Ensure/AppServer publication before
// the replacement generation is installed.
func (g *runtimePermissionGate) publish(subject string, create func() (any, error)) (any, string, error) {
	return g.publishWithBaseline(subject, nil, create)
}

// publishWithBaseline also records every unresolved durable request that
// predates runtime creation. Baseline capture, creation visibility, and
// generation publication occur under the one gate lock, so a request cannot
// fall into a gap and later be relabeled as belonging to the replacement.
func (g *runtimePermissionGate) publishWithBaseline(subject string, baseline func() ([]string, error), create func() (any, error)) (any, string, error) {
	if g == nil || strings.TrimSpace(subject) == "" || create == nil {
		return nil, "", fmt.Errorf("permission runtime unavailable")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	var excluded []string
	var err error
	if baseline != nil {
		excluded, err = baseline()
		if err != nil {
			return nil, "", fmt.Errorf("capture permission boundary: %w", err)
		}
	}
	runtime, err := create()
	if err != nil {
		return nil, "", err
	}
	if runtime == nil {
		return nil, "", fmt.Errorf("permission runtime unavailable")
	}
	generation, err := g.observeLocked(subject, runtime, excluded)
	return runtime, generation, err
}

func (d *Daemon) publishPermissionRuntime(subject string, create func() (any, error)) (any, string, error) {
	var baseline func() ([]string, error)
	if d.permissionBaseline != nil {
		baseline = func() ([]string, error) { return d.permissionBaseline(subject) }
	}
	return d.permissions.publishWithBaseline(subject, baseline, create)
}

func permissionRuntimeIdentity(runtime any) string {
	return fmt.Sprintf("%T:%p", runtime, runtime)
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

func (g *runtimePermissionGate) token(subject string) (permissionRuntimeToken, bool) {
	if g == nil {
		return permissionRuntimeToken{}, false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	runtime, ok := g.runtimes[subject]
	return permissionRuntimeToken{identity: runtime.identity, generation: runtime.generation}, ok
}

func (g *runtimePermissionGate) matches(subject string, token permissionRuntimeToken) bool {
	if g == nil || token.identity == "" || token.generation == "" {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	runtime, ok := g.runtimes[subject]
	return ok && runtime.identity == token.identity && runtime.generation == token.generation
}

func (g *runtimePermissionGate) retireTokenAnd(subject string, token permissionRuntimeToken, stop func()) bool {
	if g == nil || token.identity == "" || token.generation == "" {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	runtime, ok := g.runtimes[subject]
	if !ok || runtime.identity != token.identity || runtime.generation != token.generation {
		return false
	}
	delete(g.runtimes, subject)
	if stop != nil {
		stop()
	}
	return true
}

func (g *runtimePermissionGate) retire(subject string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	delete(g.runtimes, subject)
	g.mu.Unlock()
}

// retireAnd makes a runtime disappear while consumption is excluded. stop must
// publish no replacement; replacement is performed separately through publish.
func (g *runtimePermissionGate) retireAnd(subject string, stop func()) {
	if g == nil {
		if stop != nil {
			stop()
		}
		return
	}
	g.mu.Lock()
	delete(g.runtimes, subject)
	if stop != nil {
		stop()
	}
	g.mu.Unlock()
}

// bindRequest is the sole generation-publication seam. Event producers call it
// only for a permission request proven open on the exact live runtime handle;
// historical replay is therefore never stamped with a replacement runtime's
// current generation.
func (g *runtimePermissionGate) bindRequest(subject, requestID string, handle any, validate func() error) (string, error) {
	if g == nil || strings.TrimSpace(requestID) == "" || handle == nil || validate == nil {
		return "", fmt.Errorf("permission request requires a live runtime and request id")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	runtime, ok := g.runtimes[subject]
	if !ok || runtime.identity != permissionRuntimeIdentity(handle) {
		return "", fmt.Errorf("permission runtime was replaced")
	}
	if _, excluded := runtime.excluded[requestID]; excluded {
		return "", fmt.Errorf("permission request %q predates this runtime", requestID)
	}
	if _, claimed := runtime.claimed[requestID]; claimed {
		return "", fmt.Errorf("permission request %q was already consumed", requestID)
	}
	if err := validate(); err != nil {
		return "", err
	}
	runtime.requests[requestID] = struct{}{}
	g.runtimes[subject] = runtime
	return runtime.generation, nil
}

func (g *runtimePermissionGate) retireRequest(subject, requestID string, handle any) {
	if g == nil || strings.TrimSpace(requestID) == "" || handle == nil {
		return
	}
	g.mu.Lock()
	runtime, ok := g.runtimes[subject]
	if ok && runtime.identity == permissionRuntimeIdentity(handle) {
		delete(runtime.requests, requestID)
		g.runtimes[subject] = runtime
	}
	g.mu.Unlock()
}

func (g *runtimePermissionGate) consume(subject, generation, requestID string, handle any, validate, deliver func() error) error {
	if g == nil || strings.TrimSpace(generation) == "" || strings.TrimSpace(requestID) == "" || handle == nil || validate == nil || deliver == nil {
		return fmt.Errorf("permission requires a live runtime generation and request id")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	runtime, ok := g.runtimes[subject]
	if !ok || runtime.generation != generation || runtime.identity != permissionRuntimeIdentity(handle) {
		return fmt.Errorf("permission runtime generation is stale")
	}
	if _, observed := runtime.requests[requestID]; !observed {
		return fmt.Errorf("permission request %q was not observed on this runtime", requestID)
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
