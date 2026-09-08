package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"sync"
)

// runtimePermissionGate binds permission decisions to one live runtime
// incarnation. A request is claimed while the lock is held and before delivery;
// a delivery with an uncertain outcome therefore cannot be retried.
type runtimePermissionGate struct {
	mu       sync.Mutex
	random   io.Reader
	runtimes map[string]permissionRuntime
}

type permissionRuntime struct {
	identity   string
	generation string
	claimed    map[string]struct{}
}

func newRuntimePermissionGate() *runtimePermissionGate {
	return &runtimePermissionGate{random: rand.Reader, runtimes: make(map[string]permissionRuntime)}
}

func (g *runtimePermissionGate) observe(subject string, runtime any) (string, error) {
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
	g.runtimes[subject] = permissionRuntime{
		identity: identity, generation: generation, claimed: make(map[string]struct{}),
	}
	return generation, nil
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
