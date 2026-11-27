//go:build windows

package loop

import (
	"errors"
	"fmt"
	"os"
)

// Lock is a single-instance lock backed by exclusive file creation on
// Windows. Unlike the unix flock path, the OS does not release it on process
// death: a crashed steward leaves the lock file behind, so a restart after a
// crash refuses until the file is removed manually. This limitation is
// documented in docs/control-loop.md.
type Lock struct {
	path string
	f    *os.File
}

// Acquire creates path exclusively (O_CREATE|O_EXCL). It returns ErrLocked
// when the file already exists (another steward holds the lock).
func Acquire(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("loop: create lock %s: %w", path, err)
	}
	_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
	return &Lock{path: path, f: f}, nil
}

// Release closes and removes the lock file. It is idempotent.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	f := l.f
	l.f = nil
	cerr := f.Close()
	rerr := os.Remove(l.path)
	if cerr != nil {
		return cerr
	}
	return rerr
}
