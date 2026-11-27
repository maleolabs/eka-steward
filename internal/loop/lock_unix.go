//go:build unix

package loop

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// Lock is a single-instance advisory lock backed by flock(2). The lock is held
// on the open file description, so it is released automatically by the kernel
// when the process exits or crashes — a killed steward does not wedge a
// restart.
type Lock struct {
	f *os.File
}

// Acquire opens (creating if needed) path and takes an exclusive, non-blocking
// flock on it. It returns ErrLocked when another process already holds the
// lock. The lock file is left in place on release (flock, not its existence,
// is the lock).
func Acquire(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("loop: open lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("loop: lock %s: %w", path, err)
	}
	return &Lock{f: f}, nil
}

// Release unlocks and closes the lock file. It is idempotent.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	f := l.f
	l.f = nil
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
		f.Close()
		return fmt.Errorf("loop: unlock: %w", err)
	}
	return f.Close()
}
