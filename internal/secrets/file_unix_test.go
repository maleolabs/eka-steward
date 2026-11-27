//go:build unix

package secrets

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestNewFileResolverRefusesOpenMode is the unix permission half of the
// hardening: any group/other bit (0644, 0640) is refused. chmod is used so the
// result is immune to the test process umask.
func TestNewFileResolverRefusesOpenMode(t *testing.T) {
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604, 0o606} {
		t.Run(mode.String(), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "secrets.env")
			if err := os.WriteFile(path, []byte("A=1\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
			_, err := NewFileResolver(path)
			if !errors.Is(err, ErrCredentialFileMode) {
				t.Fatalf("mode %04o: err = %v, want ErrCredentialFileMode", mode.Perm(), err)
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("error %q does not name path %q", err.Error(), path)
			}
		})
	}
}

// TestNewFileResolverRefusesFIFO: a FIFO must be refused without blocking.
func TestNewFileResolverRefusesFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.env")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("cannot create FIFO: %v", err)
	}
	_, err := NewFileResolver(path)
	if !errors.Is(err, ErrCredentialFileNotRegular) {
		t.Fatalf("FIFO: err = %v, want ErrCredentialFileNotRegular", err)
	}
}

// TestNewFileResolverRefusesForeignOwner: a file owned by another uid is
// refused. chown to a foreign uid needs privilege, so where the test cannot do
// it (non-root) it still asserts the refusal sentinel exists and that a
// same-uid file passes (proving the owner check does not misfire).
func TestNewFileResolverRefusesForeignOwner(t *testing.T) {
	// The check must exist and be a defined sentinel.
	if ErrCredentialFileOwner == nil {
		t.Fatal("ErrCredentialFileOwner sentinel is nil")
	}

	path := filepath.Join(t.TempDir(), "secrets.env")
	if err := os.WriteFile(path, []byte("A=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A same-uid file must be accepted: the owner check does not reject us.
	if _, err := NewFileResolver(path); err != nil {
		t.Fatalf("same-uid 0600 file refused: %v", err)
	}

	foreign := os.Getuid() + 1
	if err := os.Chown(path, foreign, -1); err != nil {
		t.Skipf("cannot chown to uid %d (not privileged): %v", foreign, err)
	}
	_, err := NewFileResolver(path)
	if !errors.Is(err, ErrCredentialFileOwner) {
		t.Fatalf("foreign owner: err = %v, want ErrCredentialFileOwner", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name path %q", err.Error(), path)
	}
}
