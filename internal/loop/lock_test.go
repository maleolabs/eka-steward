package loop

import (
	"errors"
	"path/filepath"
	"testing"
)

// TestSingleInstanceLock asserts a second Acquire on the same path is refused
// with ErrLocked, and that a release frees the lock for re-acquisition. It runs
// on both the unix flock path and the windows exclusive-create path.
func TestSingleInstanceLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "steward.lock")

	first, err := Acquire(path)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}

	second, err := Acquire(path)
	if !errors.Is(err, ErrLocked) {
		if second != nil {
			_ = second.Release()
		}
		t.Fatalf("second Acquire err = %v, want ErrLocked", err)
	}

	if err := first.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	third, err := Acquire(path)
	if err != nil {
		t.Fatalf("re-Acquire after release: %v", err)
	}
	if err := third.Release(); err != nil {
		t.Fatalf("re-Release: %v", err)
	}
}

// TestLockReleaseIsIdempotent asserts a nil/again release is a no-op.
func TestLockReleaseIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "steward.lock")
	l, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("second Release: %v", err)
	}
	var nilLock *Lock
	if err := nilLock.Release(); err != nil {
		t.Fatalf("nil Release: %v", err)
	}
}
