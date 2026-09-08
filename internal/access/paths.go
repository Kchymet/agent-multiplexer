package access

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"amux/internal/core"

	"golang.org/x/sys/unix"
)

const (
	MailboxDirName    = "control"
	CredentialDirName = "credential"
	ContextFileName   = "context.json"
	MaxQueuedRequests = 128
	MaxResponseAge    = 60 // seconds
)

// SessionAccess is the frozen path contract shared with filesystem launch code.
// CredentialHostDir is daemon-private. Launch code mounts that stable directory
// read-only both at core.SessionAccessDir and, for compatibility, at
// CredentialMountDir. MailboxHostDir contains regular files, never FIFOs.
type SessionAccess struct {
	SubjectID          string
	MailboxHostDir     string
	MailboxMountDir    string
	RequestsHostDir    string
	RequestsMountDir   string
	CredentialHostDir  string
	CredentialMountDir string
}

// SessionContext is written by the daemon into the stable read-only credential
// directory. Namespace launch code bind-mounts that entire directory read-only
// at core.SessionAccessDir, so current and context.json share one stable source.
// Restricted clients read it only through core.SessionContextPath; environment
// variables are optional hints and never select authority.
type SessionContext struct {
	Protocol   int    `json:"protocol"`
	SubjectID  string `json:"subjectId"`
	MailboxDir string `json:"mailboxDir"`
}

// EnsureSession provisions authority and strict regular-file mailbox roots. It
// returns explicit mount data but does not construct sandbox arguments.
func (a *FileAuthority) EnsureSession(_ context.Context, subjectID, sessionDir string) (SessionAccess, error) {
	if !filepath.IsAbs(sessionDir) {
		return SessionAccess{}, fmt.Errorf("session directory must be absolute")
	}
	if err := validateSubject(SubjectSession, subjectID); err != nil {
		return SessionAccess{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.rootDir == nil {
		return SessionAccess{}, ErrAuthorityClosed
	}
	mailboxes, err := ensureDirAt(int(a.rootDir.Fd()), "mailboxes", 0o700)
	if err != nil {
		return SessionAccess{}, fmt.Errorf("open mailbox authority root: %w", err)
	}
	defer mailboxes.Close()
	opaque := subjectKey(SubjectSession, subjectID)
	mailbox, err := ensureDirAt(int(mailboxes.Fd()), opaque, 0o700)
	if err != nil {
		return SessionAccess{}, fmt.Errorf("open daemon-owned session mailbox: %w", err)
	}
	defer mailbox.Close()
	for _, name := range []string{"requests", "responses", CredentialDirName} {
		child, err := ensureDirAt(int(mailbox.Fd()), name, 0o700)
		if err != nil {
			return SessionAccess{}, fmt.Errorf("validate daemon-owned mailbox %s: %w", name, err)
		}
		if err := child.Close(); err != nil {
			return SessionAccess{}, err
		}
	}
	mailboxHost := filepath.Join(a.root, "mailboxes", opaque)
	credDir, err := a.ensureLocked(SubjectSession, subjectID)
	if err != nil {
		return SessionAccess{}, err
	}
	mailboxMount := filepath.Join(sessionDir, ".amux", MailboxDirName)
	contextFile := SessionContext{Protocol: ProtocolVersion, SubjectID: subjectID, MailboxDir: mailboxMount}
	if err := atomicJSON(filepath.Join(credDir, ContextFileName), contextFile, 0o400); err != nil {
		return SessionAccess{}, fmt.Errorf("publish fixed session context: %w", err)
	}
	return SessionAccess{
		SubjectID: subjectID, MailboxHostDir: mailboxHost, MailboxMountDir: mailboxMount,
		RequestsHostDir: filepath.Join(mailboxHost, "requests"), RequestsMountDir: filepath.Join(mailboxMount, "requests"),
		CredentialHostDir: credDir, CredentialMountDir: filepath.Join(mailboxMount, CredentialDirName),
	}, nil
}

func LoadSessionContext() (SessionContext, error) {
	var sessionContext SessionContext
	if err := readJSON(core.SessionContextPath(), &sessionContext); err != nil {
		return SessionContext{}, err
	}
	if sessionContext.Protocol != ProtocolVersion || sessionContext.SubjectID == "" || !filepath.IsAbs(sessionContext.MailboxDir) {
		return SessionContext{}, ErrInvalidCredential
	}
	return sessionContext, nil
}

// ensureDirAt creates or reopens exactly one directory component beneath a
// held parent descriptor. Existing symlinks and non-directories fail; chmod is
// descriptor-relative, so a concurrent replacement cannot redirect it.
func ensureDirAt(parentFD int, name string, mode uint32) (*os.File, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, '/') || strings.ContainsRune(name, 0) {
		return nil, fmt.Errorf("invalid directory component")
	}
	if err := unix.Mkdirat(parentFD, name, mode); err != nil && !errors.Is(err, unix.EEXIST) {
		return nil, err
	}
	fd, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	if err := unix.Fchmod(fd, mode); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}
