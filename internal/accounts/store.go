// Package accounts owns machine profile metadata and durable workload bindings.
// Secrets stay in the existing private vendor stores. Transactions fence async
// native operations by delivery epoch; neither email nor profile ID is an account.
package accounts

import (
	"amux/internal/amuxcfg"
	"amux/internal/centralauth"
	"amux/internal/core"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	hp "github.com/kchymet/agent-multiplexer/harnessproto"
	"io"
	"os"
	"path/filepath"
	"sort"
	"syscall"
)

var ErrStale = errors.New("account_revision_changed")
var ErrBlocked = errors.New("account_binding_blocked")

// ErrSuperseded rejects delivery to a runtime that a newer launch replaced.
var ErrSuperseded = errors.New("account_runtime_superseded")

// StoreVersion 2 makes every workload follow its harness's central profile.
// Version 1 stores are migrated forward in place (see migrate); newer versions
// are refused without rewriting them.
const StoreVersion = 2

// historyLimit bounds the per-binding record of earlier runtimes' deliveries.
const historyLimit = 8

// Delivered is what one runtime (one launched harness process) received.
// It is evidence, never a credential.
type Delivered struct {
	Launch  uint64 `json:"launch"`
	Epoch   uint64 `json:"epoch"`
	Mode    string `json:"mode,omitempty"`
	Account string `json:"account,omitempty"`
	Version uint64 `json:"version,omitempty"`
}

// Binding is a workload's durable routing. Since version 2 it always names the
// harness's central profile; the delivered fields describe the current runtime
// only and are re-pinned by the next process launch.
type Binding struct {
	DeliveredEpoch    uint64 `json:"deliveredEpoch,omitempty"`
	ProfileID         string `json:"profileId"`
	Harness           string `json:"harness"`
	Source            string `json:"source"`
	CredentialMode    string `json:"credentialMode,omitempty"` // subscription | api of the current runtime
	ExpectedAccount   string `json:"expectedAccount,omitempty"`
	CredentialVersion uint64 `json:"credentialVersion,omitempty"`
	Revision          uint64 `json:"revision"`
	Origin            string `json:"origin"`
	// Launch is the current runtime's launch sequence; Launches allocates them.
	Launch   uint64 `json:"launch,omitempty"`
	Launches uint64 `json:"launches,omitempty"`
	// LegacySource is the pre-central login this workload keeps using until the
	// central profile is first configured. It is cleared, never restored, then.
	LegacySource string      `json:"legacySource,omitempty"`
	History      []Delivered `json:"history,omitempty"`
}

type State struct {
	DefaultRevisions map[string]uint64            `json:"defaultRevisions"`
	Version          int                          `json:"version"`
	MachineID        string                       `json:"machineId"`
	Profiles         map[string]hp.AccountProfile `json:"profiles"`
	Defaults         map[string]string            `json:"defaults"`
	Bindings         map[string]Binding           `json:"bindings"`
	// CredentialVersion fences opaque Claude replacement independently of epochs.
	CredentialVersion map[string]uint64 `json:"credentialVersion"`
	// LegacyFallback is the legacy source new workloads start on while a
	// harness's central profile has never been configured.
	LegacyFallback map[string]string `json:"legacyFallback,omitempty"`
}

// LegacyUsable reports whether a legacy source can still authenticate a new
// process. Harness packages register it; an unregistered check means unusable,
// which routes the launch to the central login flow.
var LegacyUsable = map[string]func(source string) bool{}

func ID(h, mode string) string { return h + ":" + mode }
func Path() string             { return filepath.Join(core.AuthDir(), "accounts.json") }
func initial() (State, error) {
	var nonce [16]byte
	if _, e := rand.Read(nonce[:]); e != nil {
		return State{}, e
	}
	s := State{DefaultRevisions: map[string]uint64{"claude": 1, "codex": 1}, Version: StoreVersion, MachineID: hex.EncodeToString(nonce[:]), Profiles: map[string]hp.AccountProfile{}, Defaults: map[string]string{}, Bindings: map[string]Binding{}, CredentialVersion: map[string]uint64{}, LegacyFallback: map[string]string{}}
	for _, h := range []string{"claude", "codex"} {
		// A legacy claude.auth / codex.auth setting only seeds the transition
		// fallback; the default is always the central profile.
		mode, e := amuxcfg.AuthMode(h)
		if e != nil {
			return s, e
		}
		s.Defaults[h] = ID(h, amuxcfg.AuthCentral)
		if mode != amuxcfg.AuthCentral {
			s.LegacyFallback[h] = mode
		}
		for _, m := range amuxcfg.AuthModes(h) {
			p := hp.AccountProfile{ID: ID(h, m), Harness: h, Source: m, Revision: 1, DeliveryEpoch: 1, State: "unconfigured", Identity: hp.AccountIdentity{State: "unknown", Evidence: "external"}, Limits: hp.AccountLimits{State: "unavailable", Source: "unsupported"}, LoginMethod: "external"}
			if m == amuxcfg.AuthCentral {
				p.Mode = "subscription"
				if h == "claude" {
					p.LoginMethod = "setup-token"
					p.Identity.Evidence = "opaque-token"
					p.Limits.Source = "claude-statusline"
				} else {
					p.LoginMethod = "browser"
					p.Identity.Evidence = "native"
					p.Limits.Source = "codex-native"
				}
			} else {
				p.State = "ready"
			}
			s.Profiles[p.ID] = p
		}
	}
	return s, nil
}

// Update holds a process-shared lock through validation and atomic persistence.
// Callers may perform a short credential store write inside fn, never network IO.
func Update(fn func(*State) error) error {
	dir := core.AuthDir()
	if e := os.MkdirAll(dir, 0700); e != nil {
		return e
	}
	if fi, e := os.Lstat(dir); e != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 || fi.Mode().Perm()&0077 != 0 {
		return errors.New("account_store_insecure")
	}
	f, e := os.OpenFile(filepath.Join(dir, "accounts.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if e != nil {
		return e
	}
	defer f.Close()
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX); e != nil {
		return e
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	s, from, e := load()
	if e != nil {
		return e
	}
	if e = fn(&s); e != nil {
		return e
	}
	s.normalize()
	if from == 1 {
		if e = backupV1(dir); e != nil {
			return e
		}
	}
	b, e := json.Marshal(s)
	if e != nil {
		return e
	}
	tmp, e := os.CreateTemp(dir, ".accounts-*")
	if e != nil {
		return e
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, e = tmp.Write(b); e == nil {
		e = tmp.Sync()
	}
	ce := tmp.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	if e = os.Rename(name, Path()); e != nil {
		return e
	}
	d, e := os.Open(dir)
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}
func read() (State, error) {
	s, _, e := load()
	return s, e
}

// load returns the current state migrated to StoreVersion and the version it
// was stored as (0 when absent). Unknown, newer or malformed metadata is an
// explicit error: it is never reinitialized or rewritten.
func load() (State, int, error) {
	f, e := os.OpenFile(Path(), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if os.IsNotExist(e) {
		s, e := initial()
		return s, 0, e
	}
	if e != nil {
		return State{}, 0, errors.New("account_store_unavailable")
	}
	defer f.Close()
	fi, e := f.Stat()
	if e != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0077 != 0 {
		return State{}, 0, errors.New("account_store_insecure")
	}
	b, e := io.ReadAll(io.LimitReader(f, 8<<20))
	if e != nil {
		return State{}, 0, e
	}
	var s State
	if json.Unmarshal(b, &s) != nil || s.Profiles == nil || s.Bindings == nil || s.Defaults == nil || s.CredentialVersion == nil {
		return s, 0, errors.New("account_store_invalid")
	}
	from := s.Version
	switch from {
	case 1:
		s.migrateV1()
	case StoreVersion:
	default:
		return s, from, errors.New("account_store_version_unsupported")
	}
	if s.DefaultRevisions == nil {
		s.DefaultRevisions = map[string]uint64{"claude": 1, "codex": 1}
	}
	if s.LegacyFallback == nil {
		s.LegacyFallback = map[string]string{}
	}
	s.normalize()
	return s, from, nil
}

// backupV1 keeps the original version 1 metadata once, beside the store, before
// the first migrated write replaces it. An existing backup is never replaced.
func backupV1(dir string) error {
	b, e := os.ReadFile(Path())
	if e != nil {
		return e
	}
	f, e := os.OpenFile(filepath.Join(dir, "accounts.v1.json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if os.IsExist(e) {
		return nil
	}
	if e != nil {
		return e
	}
	if _, e = f.Write(b); e == nil {
		e = f.Sync()
	}
	if ce := f.Close(); e == nil {
		e = ce
	}
	return e
}

// migrateV1 moves every default and binding to the harness's central profile.
// A non-central default or binding keeps its source as the transition fallback
// unless central was already configured. Delivered evidence of the current
// runtime is preserved; the next launch re-pins it. Idempotent by version.
func (s *State) migrateV1() {
	if s.DefaultRevisions == nil {
		s.DefaultRevisions = map[string]uint64{"claude": 1, "codex": 1}
	}
	if s.LegacyFallback == nil {
		s.LegacyFallback = map[string]string{}
	}
	for _, h := range []string{"claude", "codex"} {
		central := ID(h, amuxcfg.AuthCentral)
		if d, ok := s.Profiles[s.Defaults[h]]; ok && d.Source != amuxcfg.AuthCentral && !s.centralConfigured(h) {
			s.LegacyFallback[h] = d.Source
		}
		if s.Defaults[h] != central {
			s.Defaults[h] = central
			s.DefaultRevisions[h]++
		}
	}
	for id, b := range s.Bindings {
		if b.Harness != "claude" && b.Harness != "codex" {
			continue
		}
		if b.Source != amuxcfg.AuthCentral && b.Source != "" && !s.centralConfigured(b.Harness) {
			b.LegacySource = b.Source
		}
		if b.Source != amuxcfg.AuthCentral {
			// Legacy runtimes never received central evidence.
			b.DeliveredEpoch, b.CredentialMode, b.ExpectedAccount, b.CredentialVersion = 0, "", "", 0
		}
		b.ProfileID = ID(b.Harness, amuxcfg.AuthCentral)
		b.Source = amuxcfg.AuthCentral
		b.Origin = "migrated"
		b.Revision++
		s.Bindings[id] = b
	}
	s.Version = StoreVersion
}

// centralConfigured reports whether the central profile has ever committed a
// credential (a ready profile, or a positive credential generation).
func (s *State) centralConfigured(h string) bool {
	id := ID(h, amuxcfg.AuthCentral)
	return s.Profiles[id].State == "ready" || s.CredentialVersion[id] != 0
}

// normalize retires the transition fallback once central is configured: the
// newest successfully configured login then serves every next launch.
func (s *State) normalize() {
	for _, h := range []string{"claude", "codex"} {
		if !s.centralConfigured(h) {
			continue
		}
		delete(s.LegacyFallback, h)
		for id, b := range s.Bindings {
			if b.Harness == h && b.LegacySource != "" {
				b.LegacySource = ""
				b.Revision++
				s.Bindings[id] = b
			}
		}
	}
}

func Read() (State, error) { return read() }
func (s State) Snapshot() hp.AccountSnapshot {
	out := hp.AccountSnapshot{DefaultRevisions: s.DefaultRevisions, Version: 1, Defaults: s.Defaults, Profiles: []hp.AccountProfile{}}
	for _, p := range s.Profiles {
		out.Profiles = append(out.Profiles, p)
	}
	sort.Slice(out.Profiles, func(i, j int) bool { return out.Profiles[i].ID < out.Profiles[j].ID })
	return out
}
func (s *State) Profile(id string, rev uint64) (hp.AccountProfile, error) {
	p, ok := s.Profiles[id]
	if !ok {
		return p, errors.New("account_profile_unknown")
	}
	if rev != 0 && rev != p.Revision {
		return p, ErrStale
	}
	return p, nil
}

// Bind records a new workload, or preserves its already recorded binding.
func Bind(session, h string) error { return bind(session, h) }

// BindExisting records a binding for a workload discovered without one. Every
// binding follows central; there is no first-login or unknown-history state.
func BindExisting(session, h string) error { return bind(session, h) }

func bind(session, h string) error {
	if h == "" {
		h = "claude"
	}
	if h != "claude" && h != "codex" {
		return nil
	}
	return Update(func(s *State) error {
		// Bootstrap the opaque version of a credential stored before the
		// account service so that it counts as configured central state.
		if h == "claude" && s.CredentialVersion[ID(h, "central")] == 0 {
			if c, err := centralauth.ClaudeStore().Load(); err == nil {
				s.CredentialVersion[ID(h, "central")] = c.Generation
			} else if !errors.Is(err, centralauth.ErrNotConfigured) {
				return err
			}
		}
		if b, ok := s.Bindings[session]; ok {
			if b.Harness != h {
				return ErrBlocked
			}
			return nil
		}
		b := Binding{ProfileID: ID(h, amuxcfg.AuthCentral), Harness: h, Source: amuxcfg.AuthCentral, Revision: 1, Origin: "machine-default"}
		if !s.centralConfigured(h) {
			b.LegacySource = s.LegacyFallback[h]
		}
		s.Bindings[session] = b
		return nil
	})
}

// Mode is the source the next launch of this workload uses: central, or its
// still-usable legacy login while central has never been configured. An
// unusable legacy login routes to central, whose login flow then applies.
func Mode(session, h string) string {
	s, e := Read()
	if e != nil {
		return "blocked"
	}
	legacy := s.LegacyFallback[h]
	if b, ok := s.Bindings[session]; ok && b.Harness == h {
		legacy = b.LegacySource
	}
	if legacy != "" && !s.centralConfigured(h) {
		if usable := LegacyUsable[h]; usable != nil && usable(legacy) {
			return legacy
		}
	}
	return amuxcfg.AuthCentral
}

// GlobalMode is Mode for a launch with no workload, such as host diagnostics.
func GlobalMode(h string) string { return Mode("", h) }

func Delivery(session, h string) (Binding, hp.AccountProfile, error) {
	if e := BindExisting(session, h); e != nil {
		return Binding{}, hp.AccountProfile{}, e
	}
	s, e := Read()
	if e != nil {
		return Binding{}, hp.AccountProfile{}, e
	}
	b := s.Bindings[session]
	p := s.Profiles[b.ProfileID]
	if b.Source != amuxcfg.AuthCentral || p.State == "logged-out" || p.State == "login-pending" || p.State == "blocked" {
		return b, p, ErrBlocked
	}
	return b, p, nil
}

// BeginLaunch allocates the launch sequence for a new harness process. The
// process's first delivery pins the newest credential to this launch; any
// delivery for an older launch is then refused.
func BeginLaunch(session, h string) (uint64, error) {
	if e := BindExisting(session, h); e != nil {
		return 0, e
	}
	var seq uint64
	e := Update(func(s *State) error {
		b, ok := s.Bindings[session]
		if !ok || b.Harness != h {
			return ErrBlocked
		}
		b.Launches = max(b.Launches, b.Launch) + 1
		seq = b.Launches
		s.Bindings[session] = b
		return nil
	})
	return seq, e
}

// Accept records a delivery to a runtime. Without a launch argument the
// delivery is itself a new process launch (the Claude launcher execs once per
// credential); a supervised Codex process passes its BeginLaunch sequence.
func Accept(session string, revision, epoch uint64, account string, version uint64, launch ...uint64) error {
	return AcceptAuthorized(session, revision, epoch, account, version, func(commit func() error) error { return commit() }, launch...)
}

// AcceptAuthorized holds metadata ownership before acquiring an authority lease.
// Admission performs only local validation; it must not wait for a runtime.
//
// A newer launch pins whatever the central profile currently holds: mode,
// account and version may all differ from the previous runtime. The current
// launch must keep its mode and account (same-account Codex refresh), and an
// opaque or API credential its version. An older launch is superseded.
func AcceptAuthorized(session string, revision, epoch uint64, account string, version uint64, admit func(func() error) error, launch ...uint64) error {
	var seq uint64
	allocate := len(launch) == 0
	if !allocate {
		seq = launch[0]
	}
	return Update(func(s *State) error {
		return admit(func() error {
			b, ok := s.Bindings[session]
			if !ok || b.Revision != revision || b.Source != amuxcfg.AuthCentral {
				return ErrBlocked
			}
			p := s.Profiles[b.ProfileID]
			if p.DeliveryEpoch != epoch || p.State == "logged-out" || p.State == "login-pending" || p.State == "blocked" {
				return ErrStale
			}
			mode := p.Mode
			if mode == "" {
				mode = "subscription"
			}
			if allocate {
				b.Launches = max(b.Launches, b.Launch) + 1
				seq = b.Launches
			}
			switch {
			case seq < b.Launch:
				return ErrSuperseded
			case seq == b.Launch && b.DeliveredEpoch != 0:
				if b.CredentialMode != "" && b.CredentialMode != mode {
					return ErrBlocked
				}
				if b.ExpectedAccount != "" && b.ExpectedAccount != account {
					return ErrBlocked
				}
				if (b.Harness == "claude" || mode == "api") && b.CredentialVersion != 0 && b.CredentialVersion != version {
					return ErrBlocked
				}
			default:
				if b.DeliveredEpoch != 0 {
					b.History = append(b.History, Delivered{Launch: b.Launch, Epoch: b.DeliveredEpoch, Mode: b.CredentialMode, Account: b.ExpectedAccount, Version: b.CredentialVersion})
					if len(b.History) > historyLimit {
						b.History = append([]Delivered(nil), b.History[len(b.History)-historyLimit:]...)
					}
				}
				b.Launch = seq
				b.Launches = max(b.Launches, seq)
			}
			b.CredentialMode = mode
			b.DeliveredEpoch = epoch
			if b.Harness == "claude" && s.CredentialVersion[p.ID] == 0 {
				s.CredentialVersion[p.ID] = version
				p.State = "ready"
				s.Profiles[p.ID] = p
			}
			b.ExpectedAccount = account
			b.CredentialVersion = version
			s.Bindings[session] = b
			return nil
		})
	})
}
