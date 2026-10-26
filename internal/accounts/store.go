// Package accounts owns machine profile metadata and durable workload bindings.
// Secrets stay in the existing private vendor stores. Transactions fence async
// native operations by delivery epoch; neither email nor profile ID is an account.
package accounts

import (
	"amux/internal/amuxcfg"
	"amux/internal/centralauth"
	"amux/internal/core"
	"crypto/rand"
	"crypto/sha256"
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

// pendingLimit bounds allocated launches awaiting their first delivery. The
// daemon does not allocate for a reattach to a live pane, so only launches
// that started (or failed to start) a process occupy it.
const pendingLimit = 16

// Launch identifies one harness process launch. The nonce is handed only to
// that launched process (the Codex supervisor, or the Claude launcher's argv,
// which its exec replaces); metadata keeps only its hash.
type Launch struct {
	Seq   uint64
	Nonce string
}

// PendingLaunch is an allocated launch awaiting its first delivery.
type PendingLaunch struct {
	Seq       uint64 `json:"seq"`
	NonceHash string `json:"nonceHash"`
}

func nonceHash(nonce string) string {
	sum := sha256.Sum256([]byte("amux-account-launch\x00" + nonce))
	return hex.EncodeToString(sum[:])
}

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
	// Launch is the current runtime's launch sequence and LaunchNonce the hash
	// of its nonce. Pending are allocated launches whose process has not
	// received a credential yet, oldest first. Launches allocates sequences.
	Launch      uint64          `json:"launch,omitempty"`
	LaunchNonce string          `json:"launchNonce,omitempty"`
	Pending     []PendingLaunch `json:"pendingLaunches,omitempty"`
	Launches    uint64          `json:"launches,omitempty"`
	// MigratedFrom records the pre-central source of a migrated binding. It is
	// evidence only and never routes a launch.
	MigratedFrom string      `json:"migratedFrom,omitempty"`
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
	// Migrated is the pre-central default of each harness migrated from version
	// 1, kept as evidence. Until that harness's first central login the
	// migration is reported as pending (MigrationPending).
	Migrated map[string]string `json:"migrated,omitempty"`
}

func ID(h, mode string) string { return h + ":" + mode }
func Path() string             { return filepath.Join(core.AuthDir(), "accounts.json") }
func initial() (State, error) {
	var nonce [16]byte
	if _, e := rand.Read(nonce[:]); e != nil {
		return State{}, e
	}
	s := State{DefaultRevisions: map[string]uint64{"claude": 1, "codex": 1}, Version: StoreVersion, MachineID: hex.EncodeToString(nonce[:]), Profiles: map[string]hp.AccountProfile{}, Defaults: map[string]string{}, Bindings: map[string]Binding{}, CredentialVersion: map[string]uint64{}, Migrated: map[string]string{}}
	for _, h := range []string{"claude", "codex"} {
		// A legacy claude.auth / codex.auth setting is migrated once: the
		// default is always the central profile, and the old selection is
		// recorded so the pending central login is reported.
		mode, e := amuxcfg.AuthMode(h)
		if e != nil {
			return s, e
		}
		s.Defaults[h] = ID(h, amuxcfg.AuthCentral)
		if mode != amuxcfg.AuthCentral {
			s.Migrated[h] = mode
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
	if s.Migrated == nil {
		s.Migrated = map[string]string{}
	}
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

// migrateV1 moves every default and binding to the harness's central profile,
// once. No credential is copied: host and shared logins hold rotating refresh
// tokens, and environment credentials are not amux's to move. A harness whose
// central profile was never configured reports the migration as pending until
// its first central login; launches meanwhile use the central login flow.
// Delivered evidence of central runtimes is preserved. Idempotent by version.
func (s *State) migrateV1() {
	if s.DefaultRevisions == nil {
		s.DefaultRevisions = map[string]uint64{"claude": 1, "codex": 1}
	}
	if s.Migrated == nil {
		s.Migrated = map[string]string{}
	}
	for _, h := range []string{"claude", "codex"} {
		central := ID(h, amuxcfg.AuthCentral)
		if d, ok := s.Profiles[s.Defaults[h]]; ok && d.Source != amuxcfg.AuthCentral {
			s.Migrated[h] = d.Source
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
		if b.Source != amuxcfg.AuthCentral {
			b.MigratedFrom = b.Source
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

// MigrationPending names the pre-central source of a harness migrated from
// version 1 whose central profile has not been configured yet, or "".
func (s State) MigrationPending(h string) string {
	if s.centralConfigured(h) {
		return ""
	}
	return s.Migrated[h]
}

func Read() (State, error) { return read() }
func (s State) Snapshot() hp.AccountSnapshot {
	out := hp.AccountSnapshot{DefaultRevisions: s.DefaultRevisions, Version: 1, Defaults: s.Defaults, Profiles: []hp.AccountProfile{}}
	for _, h := range []string{"claude", "codex"} {
		if from := s.MigrationPending(h); from != "" {
			if out.MigrationPending == nil {
				out.MigrationPending = map[string]string{}
			}
			out.MigrationPending[h] = from
		}
	}
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
func Bind(session, h string) error { return BindAll(map[string]string{session: h}) }

// BindExisting records a binding for a workload discovered without one. Every
// binding follows central; there is no first-login or unknown-history state.
func BindExisting(session, h string) error { return Bind(session, h) }

// BindAll binds many workloads (session -> harness) in one transaction.
func BindAll(sessions map[string]string) error {
	return Update(func(s *State) error {
		for session, h := range sessions {
			if e := s.bind(session, h); e != nil {
				return e
			}
		}
		return nil
	})
}

func (s *State) bind(session, h string) error {
	if h == "" {
		h = "claude"
	}
	if h != "claude" && h != "codex" {
		return nil
	}
	// Bootstrap the opaque version of a credential stored before the account
	// service so that it counts as configured central state. A corrupt record
	// stays unconfigured (delivery refuses it; login replaces it). An insecure
	// store fails closed until its permissions are fixed.
	if h == "claude" && s.CredentialVersion[ID(h, "central")] == 0 {
		if c, err := centralauth.ClaudeStore().Load(); err == nil {
			s.CredentialVersion[ID(h, "central")] = c.Generation
		} else if errors.Is(err, centralauth.ErrInsecureStore) {
			return err
		}
	}
	if b, ok := s.Bindings[session]; ok {
		if b.Harness != h {
			return ErrBlocked
		}
		return nil
	}
	s.Bindings[session] = Binding{ProfileID: ID(h, amuxcfg.AuthCentral), Harness: h, Source: amuxcfg.AuthCentral, Revision: 1, Origin: "machine-default"}
	return nil
}

// FixtureOrigin marks a binding written by a native runtime test fixture.
const FixtureOrigin = "synthetic-runtime-fixture"

// fixtureSource is the test-only routing seam (fixture_amuxfixture.go). In a
// release build it is never set and every binding follows central.
var fixtureSource = func(Binding) string { return "" }

// FixtureSourcesEnabled reports whether this build includes the seam.
func FixtureSourcesEnabled() bool {
	return fixtureSource(Binding{Origin: FixtureOrigin, Source: amuxcfg.AuthEnvironment}) != ""
}

// Mode is the source a launch of this workload uses. Every binding follows
// central; "blocked" reports unreadable account metadata.
func Mode(session, h string) string {
	s, e := Read()
	if e != nil {
		return "blocked"
	}
	if b, ok := s.Bindings[session]; ok && b.Harness == h {
		if src := fixtureSource(b); src != "" {
			return src
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

// BeginLaunch allocates a launch for a new harness process. The process's
// first delivery pins the newest central credential to this launch and
// retires every older launch; after that, deliveries to it must match.
func BeginLaunch(session, h string) (Launch, error) {
	if e := BindExisting(session, h); e != nil {
		return Launch{}, e
	}
	var raw [24]byte
	if _, e := rand.Read(raw[:]); e != nil {
		return Launch{}, e
	}
	// Every launch gets its own nonce, so a killed process's late request can
	// never claim the permit of the process that replaced it.
	l := Launch{Nonce: hex.EncodeToString(raw[:])}
	e := Update(func(s *State) error {
		b, ok := s.Bindings[session]
		if !ok || b.Harness != h {
			return ErrBlocked
		}
		b.Launches = max(b.Launches, b.Launch) + 1
		l.Seq = b.Launches
		b.Pending = append(b.Pending, PendingLaunch{Seq: l.Seq, NonceHash: nonceHash(l.Nonce)})
		if len(b.Pending) > pendingLimit {
			b.Pending = append([]PendingLaunch(nil), b.Pending[len(b.Pending)-pendingLimit:]...)
		}
		s.Bindings[session] = b
		return nil
	})
	return l, e
}

// RetireLaunch withdraws an allocated launch whose argv started no process (the
// engine reused a running pane, or the spawn failed). Unknown launches are a no-op.
func RetireLaunch(session string, l Launch) error {
	hash := nonceHash(l.Nonce)
	return Update(func(s *State) error {
		b, ok := s.Bindings[session]
		if !ok {
			return nil
		}
		var keep []PendingLaunch
		for _, q := range b.Pending {
			if q.Seq != l.Seq || q.NonceHash != hash {
				keep = append(keep, q)
			}
		}
		b.Pending = keep
		s.Bindings[session] = b
		return nil
	})
}

// Accept records a delivery to the runtime of one process launch.
func Accept(session string, revision, epoch uint64, account string, version uint64, l Launch) error {
	return AcceptAuthorized(session, revision, epoch, account, version, func(commit func() error) error { return commit() }, l)
}

// AcceptAuthorized holds metadata ownership before acquiring an authority lease.
// Admission performs only local validation; it must not wait for a runtime.
//
// The first delivery of an allocated launch pins whatever the central profile
// currently holds: mode, account and version may all differ from the previous
// runtime. Later deliveries to the same launch must keep its mode and account
// (same-account Codex refresh), and an opaque or API credential its version.
// A repeated request therefore never selects a replacement; only a new process
// launch does. An older launch is superseded; an unallocated one is refused.
func AcceptAuthorized(session string, revision, epoch uint64, account string, version uint64, admit func(func() error) error, l Launch) error {
	hash := nonceHash(l.Nonce)
	return Update(func(s *State) error {
		return admit(func() error {
			b, ok := s.Bindings[session]
			if !ok || b.Source != amuxcfg.AuthCentral {
				return ErrBlocked
			}
			if b.Revision != revision {
				return ErrStale // retryable: the binding changed during delivery
			}
			p := s.Profiles[b.ProfileID]
			if p.DeliveryEpoch != epoch || p.State == "logged-out" || p.State == "login-pending" || p.State == "blocked" {
				return ErrStale
			}
			mode := p.Mode
			if mode == "" {
				mode = "subscription"
			}
			pending := false
			for _, q := range b.Pending {
				pending = pending || (q.Seq == l.Seq && q.NonceHash == hash)
			}
			switch {
			case l.Seq < b.Launch:
				return ErrSuperseded
			case l.Seq == b.Launch && b.LaunchNonce == hash && b.DeliveredEpoch != 0:
				// A replacement login never retargets this running process.
				if b.CredentialMode != "" && b.CredentialMode != mode {
					return ErrSuperseded
				}
				if b.ExpectedAccount != "" && b.ExpectedAccount != account {
					return ErrSuperseded
				}
				if (b.Harness == "claude" || mode == "api") && b.CredentialVersion != 0 && b.CredentialVersion != version {
					return ErrSuperseded
				}
			case pending:
				if b.DeliveredEpoch != 0 {
					b.History = append(b.History, Delivered{Launch: b.Launch, Epoch: b.DeliveredEpoch, Mode: b.CredentialMode, Account: b.ExpectedAccount, Version: b.CredentialVersion})
					if len(b.History) > historyLimit {
						b.History = append([]Delivered(nil), b.History[len(b.History)-historyLimit:]...)
					}
				}
				// Launches are monotonic: an older pending launch belongs to a
				// process that was replaced (or never started) and is retired.
				b.Launch, b.LaunchNonce = l.Seq, hash
				var keep []PendingLaunch
				for _, q := range b.Pending {
					if q.Seq > l.Seq {
						keep = append(keep, q)
					}
				}
				b.Pending = keep
			default:
				return ErrBlocked // not a launch the daemon allocated
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
