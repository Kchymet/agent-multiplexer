// Package accounts owns machine profile metadata and durable workload bindings.
// Secrets stay in the existing private vendor stores. Transactions fence async
// native operations by delivery epoch; neither email nor profile ID is an account.
package accounts

import (
	"amux/internal/amuxcfg"
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

type Binding struct {
	DeliveredEpoch    uint64 `json:"deliveredEpoch,omitempty"`
	ProfileID         string `json:"profileId"`
	Harness           string `json:"harness"`
	Source            string `json:"source"`
	ExpectedAccount   string `json:"expectedAccount,omitempty"`
	CredentialVersion uint64 `json:"credentialVersion,omitempty"`
	Revision          uint64 `json:"revision"`
	Origin            string `json:"origin"`
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
}

func ID(h, mode string) string { return h + ":" + mode }
func Path() string             { return filepath.Join(core.AuthDir(), "accounts.json") }
func initial() (State, error) {
	var nonce [16]byte
	if _, e := rand.Read(nonce[:]); e != nil {
		return State{}, e
	}
	s := State{DefaultRevisions: map[string]uint64{"claude": 1, "codex": 1}, Version: 1, MachineID: hex.EncodeToString(nonce[:]), Profiles: map[string]hp.AccountProfile{}, Defaults: map[string]string{}, Bindings: map[string]Binding{}, CredentialVersion: map[string]uint64{}}
	for _, h := range []string{"claude", "codex"} {
		mode, e := amuxcfg.AuthMode(h)
		if e != nil {
			return s, e
		}
		s.Defaults[h] = ID(h, mode)
		for _, m := range amuxcfg.AuthModes(h) {
			p := hp.AccountProfile{ID: ID(h, m), Harness: h, Source: m, Revision: 1, DeliveryEpoch: 1, State: "unconfigured", Identity: hp.AccountIdentity{State: "unknown", Evidence: "external"}, Limits: hp.AccountLimits{State: "unavailable", Source: "unsupported"}, LoginMethod: "external"}
			if m == amuxcfg.AuthCentral {
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
	s, e := read()
	if e != nil {
		return e
	}
	if e = fn(&s); e != nil {
		return e
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
	f, e := os.OpenFile(Path(), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if os.IsNotExist(e) {
		return initial()
	}
	if e != nil {
		return State{}, errors.New("account_store_unavailable")
	}
	defer f.Close()
	fi, e := f.Stat()
	if e != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0077 != 0 {
		return State{}, errors.New("account_store_insecure")
	}
	b, e := io.ReadAll(io.LimitReader(f, 8<<20))
	if os.IsNotExist(e) {
		return initial()
	}
	if e != nil {
		return State{}, e
	}
	var s State
	if json.Unmarshal(b, &s) != nil || s.Version != 1 || s.Profiles == nil || s.Bindings == nil || s.Defaults == nil || s.CredentialVersion == nil {
		return s, errors.New("account_store_invalid")
	}
	if s.DefaultRevisions == nil {
		s.DefaultRevisions = map[string]uint64{"claude": 1, "codex": 1}
	}
	return s, nil
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
func Bind(session, h string) error {
	if h == "" {
		h = "claude"
	}
	if h != "claude" && h != "codex" {
		return nil
	}
	return Update(func(s *State) error {
		if b, ok := s.Bindings[session]; ok {
			if b.Harness != h {
				return ErrBlocked
			}
			return nil
		}
		p, ok := s.Profiles[s.Defaults[h]]
		if !ok {
			return ErrBlocked
		}
		s.Bindings[session] = Binding{ProfileID: p.ID, Harness: h, Source: p.Source, ExpectedAccount: p.Identity.AccountID, CredentialVersion: s.CredentialVersion[p.ID], Revision: 1, Origin: "machine-default"}
		return nil
	})
}
func Mode(session, h string) string {
	s, e := Read()
	if e != nil {
		return "blocked"
	}
	if e == nil {
		if b, ok := s.Bindings[session]; ok && b.Harness == h {
			return b.Source
		}
	}
	m, e := amuxcfg.AuthMode(h)
	if e != nil {
		return amuxcfg.AuthCentral
	}
	return m
}
func Delivery(session, h string) (Binding, hp.AccountProfile, error) {
	if e := Bind(session, h); e != nil {
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

// Accept records the first account, and never rebinds a workload on refresh.
func Accept(session string, revision, epoch uint64, account string, version uint64) error {
	return AcceptAuthorized(session, revision, epoch, account, version, func(commit func() error) error { return commit() })
}

// AcceptAuthorized holds metadata ownership before acquiring an authority lease.
// Admission performs only local validation; it must not wait for a runtime.
func AcceptAuthorized(session string, revision, epoch uint64, account string, version uint64, admit func(func() error) error) error {
	return Update(func(s *State) error {
		return admit(func() error {
			b, ok := s.Bindings[session]
			if !ok || b.Revision != revision {
				return ErrBlocked
			}
			p := s.Profiles[b.ProfileID]
			if p.DeliveryEpoch != epoch || p.State == "logged-out" || p.State == "login-pending" || p.State == "blocked" {
				return ErrStale
			}
			if b.ExpectedAccount != "" && b.ExpectedAccount != account {
				return ErrBlocked
			}
			if b.Harness == "claude" && b.CredentialVersion != 0 && b.CredentialVersion != version {
				return ErrBlocked
			}
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
