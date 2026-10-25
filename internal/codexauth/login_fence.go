package codexauth

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// snapshotAuth runs while the provider owns its operation lock. A cancelled
// native browser login may already have written auth.json: retain the prior
// file privately so cancellation restores it only after the writer is reaped.
func (p *Provider) snapshotAuth() ([]byte, bool, error) {
	f, e := os.OpenFile(filepath.Join(p.cfg.Home, authFile), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(e, os.ErrNotExist) {
		return nil, false, nil
	}
	if e != nil {
		return nil, false, ErrInsecureHome
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
		return nil, false, ErrInsecureHome
	}
	b, e := io.ReadAll(io.LimitReader(f, maxAuthFile+1))
	if e != nil || len(b) > maxAuthFile {
		return nil, false, errMalformed
	}
	return b, true, nil
}
func (p *Provider) restoreAuth(b []byte, existed bool) error {
	path := filepath.Join(p.cfg.Home, authFile)
	if !existed {
		e := os.Remove(path)
		if errors.Is(e, os.ErrNotExist) {
			return nil
		}
		return e
	}
	f, e := os.CreateTemp(p.cfg.Home, ".auth-restore-*")
	if e != nil {
		return e
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, e = f.Write(b); e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	return os.Rename(tmp, path)
}
