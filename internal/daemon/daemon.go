// Package daemon is the always-on core of amux. It owns the single poll
// loop over all sources, holds canonical state, serves that state to rail/dash
// clients over a unix socket, and executes control actions against the isolated
// tmux server. Decoupling polling from rendering means the N side-pane rails
// don't each shell out to `claude agents`.
package daemon

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"amux/internal/core"
	"amux/internal/source"
)

// Daemon polls sources and serves state + actions over a unix socket.
type Daemon struct {
	sources  []source.Source
	interval time.Duration
	self     string // absolute path to the amux binary (for spawning rails)

	mu       sync.RWMutex
	sessions []core.Session

	subsMu sync.Mutex
	subs   map[chan core.Snapshot]struct{}

	pollNow chan struct{}
}

// New builds a daemon. self is the absolute path to this binary.
func New(self string, sources []source.Source, interval time.Duration) *Daemon {
	return &Daemon{
		sources:  sources,
		interval: interval,
		self:     self,
		subs:     map[chan core.Snapshot]struct{}{},
		pollNow:  make(chan struct{}, 1),
	}
}

// Default wires the source set. The rail is a workspace switcher, so the only
// source is the workspace registry (annotated with which are running).
func Default(self string) *Daemon {
	return New(self, []source.Source{source.NewWorkspace()}, 2*time.Second)
}

// Run starts the poll loop and socket server until ctx is cancelled.
func (d *Daemon) Run(ctx context.Context) error {
	if err := os.MkdirAll(core.StateDir(), 0o755); err != nil {
		return err
	}
	sock := core.SocketPath()
	if err := os.MkdirAll(filepath.Dir(sock), 0o755); err != nil {
		return err
	}
	// Clear a stale socket from a previous crash (single-instance is enforced
	// by the caller probing the socket before starting us).
	_ = os.Remove(sock)

	ln, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	defer func() { _ = ln.Close(); _ = os.Remove(sock) }()

	go d.pollLoop(ctx)

	// Close the listener when ctx ends so Accept returns.
	go func() { <-ctx.Done(); _ = ln.Close() }()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go d.serve(ctx, conn)
	}
}

// ---- polling -------------------------------------------------------------

func (d *Daemon) pollLoop(ctx context.Context) {
	t := time.NewTicker(d.interval)
	defer t.Stop()
	d.pollOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.pollOnce(ctx)
		case <-d.pollNow:
			d.pollOnce(ctx)
		}
	}
}

func (d *Daemon) pollOnce(ctx context.Context) {
	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		all []core.Session
	)
	for _, s := range d.sources {
		wg.Add(1)
		go func(s source.Source) {
			defer wg.Done()
			sess, err := s.Poll(ctx)
			if err != nil {
				log.Printf("poll %s: %v", s.Name(), err)
				return
			}
			mu.Lock()
			all = append(all, sess...)
			mu.Unlock()
		}(s)
	}
	wg.Wait()

	// De-dupe: when a richer source (Claude) already represents a tmux window,
	// drop the generic tmux entry for that same window.
	owned := map[string]bool{}
	for _, s := range all {
		if s.Source != "tmux" && s.WindowID != "" {
			owned[s.WindowID] = true
		}
	}
	deduped := all[:0]
	for _, s := range all {
		if s.Source == "tmux" && owned[s.WindowID] {
			continue
		}
		deduped = append(deduped, s)
	}
	all = deduped

	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Source != all[j].Source {
			return all[i].Source < all[j].Source
		}
		return all[i].StartedAt < all[j].StartedAt
	})

	d.mu.Lock()
	d.sessions = all
	d.mu.Unlock()
	d.broadcast()
}

func (d *Daemon) snapshot() core.Snapshot {
	d.mu.RLock()
	defer d.mu.RUnlock()
	sess := make([]core.Session, len(d.sessions))
	copy(sess, d.sessions)
	return core.Snapshot{Type: "snapshot", Sessions: sess, UpdatedAt: time.Now().UnixMilli()}
}

func (d *Daemon) broadcast() {
	snap := d.snapshot()
	d.subsMu.Lock()
	defer d.subsMu.Unlock()
	for ch := range d.subs {
		// Non-blocking: a slow client just misses an intermediate frame.
		select {
		case ch <- snap:
		default:
		}
	}
}

func (d *Daemon) triggerPoll() {
	select {
	case d.pollNow <- struct{}{}:
	default:
	}
}

// ---- connection serving --------------------------------------------------

func (d *Daemon) serve(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	ch := make(chan core.Snapshot, 4)
	d.subsMu.Lock()
	d.subs[ch] = struct{}{}
	d.subsMu.Unlock()
	defer func() {
		d.subsMu.Lock()
		delete(d.subs, ch)
		d.subsMu.Unlock()
	}()

	// Single writer goroutine serializes all frames to this conn.
	out := make(chan any, 8)
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		enc := json.NewEncoder(conn)
		for msg := range out {
			if err := enc.Encode(msg); err != nil {
				return
			}
		}
	}()

	// Send the current state immediately on connect.
	out <- d.snapshot()

	// Push subsequent snapshots.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case snap, ok := <-ch:
				if !ok {
					return
				}
				select {
				case out <- snap:
				case <-writerDone:
					return
				}
			}
		}
	}()

	// Read actions from the client until it disconnects.
	dec := json.NewDecoder(conn)
	for {
		var a core.Action
		if err := dec.Decode(&a); err != nil {
			close(out)
			<-writerDone
			return
		}
		res := d.handle(ctx, a)
		if a.Action != "" {
			select {
			case out <- res:
			case <-writerDone:
				return
			}
		}
	}
}

func (d *Daemon) find(id string) (core.Session, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	for _, s := range d.sessions {
		if s.ID == id {
			return s, true
		}
	}
	return core.Session{}, false
}
