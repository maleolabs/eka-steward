package secrets

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNewFileResolverAcceptsValid0600 is the happy path: a regular, owner-owned,
// 0600 file is read and drives the broker.
func TestNewFileResolverAcceptsValid0600(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.env")
	if err := os.WriteFile(path, []byte("# c\nANTHROPIC_API_KEY=sk-ant-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := NewFileResolver(path)
	if err != nil {
		t.Fatalf("NewFileResolver(valid 0600): %v", err)
	}
	if v, ok := r.Get("ANTHROPIC_API_KEY"); !ok || v != "sk-ant-file" {
		t.Fatalf("Get = %q, %v; want sk-ant-file", v, ok)
	}
}

// TestNewFileResolverMissingIsEmptyNoError pins the unchanged contract: a
// missing file means "no credentials configured", never an error.
func TestNewFileResolverMissingIsEmptyNoError(t *testing.T) {
	r, err := NewFileResolver(filepath.Join(t.TempDir(), "absent.env"))
	if err != nil {
		t.Fatalf("missing file errored: %v", err)
	}
	if _, ok := r.Get("ANTHROPIC_API_KEY"); ok {
		t.Fatal("missing file yielded a credential")
	}
}

// TestNewFileResolverTable covers the deterministic refusals that are portable
// (not unix-uid/mode specific): non-regular, over-size, and malformed content.
func TestNewFileResolverTable(t *testing.T) {
	overSize := make([]byte, maxCredentialFileSize+1)
	for i := range overSize {
		overSize[i] = 'A'
	}
	// A syntactically valid NAME=value of over-size length, still too big.
	copy(overSize, "X=")

	tests := []struct {
		name string
		// setup returns the path to feed NewFileResolver.
		setup func(t *testing.T, dir string) string
		want  error // sentinel to match, or nil
	}{
		{
			name: "directory refused",
			setup: func(t *testing.T, dir string) string {
				sub := filepath.Join(dir, "adir")
				if err := os.Mkdir(sub, 0o700); err != nil {
					t.Fatal(err)
				}
				return sub
			},
			want: ErrCredentialFileNotRegular,
		},
		{
			name: "over-size refused",
			setup: func(t *testing.T, dir string) string {
				path := filepath.Join(dir, "big.env")
				if err := os.WriteFile(path, overSize, 0o600); err != nil {
					t.Fatal(err)
				}
				return path
			},
			want: ErrCredentialFileTooLarge,
		},
		{
			name: "malformed line refused",
			setup: func(t *testing.T, dir string) string {
				path := filepath.Join(dir, "bad.env")
				if err := os.WriteFile(path, []byte("not a kv line\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				return path
			},
			want: nil, // malformed is a plain error, not a hardening sentinel
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.setup(t, t.TempDir())
			_, err := NewFileResolver(path)
			if err == nil {
				t.Fatalf("NewFileResolver(%s): want error, got nil", tc.name)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("error %v does not wrap %v", err, tc.want)
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("error %q does not name path %q", err.Error(), path)
			}
		})
	}
}

// TestNewFileResolverRefusesSymlink: a symlink is refused, so a swap cannot
// redirect the read. Skipped where the platform cannot create symlinks.
func TestNewFileResolverRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.env")
	if err := os.WriteFile(target, []byte("A=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.env")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	_, err := NewFileResolver(link)
	if !errors.Is(err, ErrCredentialFileSymlink) {
		t.Fatalf("symlink: err = %v, want ErrCredentialFileSymlink", err)
	}
	if !strings.Contains(err.Error(), link) {
		t.Errorf("error %q does not name path %q", err.Error(), link)
	}
}

// TestNewStateFileResolver resolves <stateDir>/secrets.env and refuses an empty
// StateDir.
func TestNewStateFileResolver(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, credentialFileName), []byte("OPENAI_API_KEY=sk-openai\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := NewStateFileResolver(dir)
	if err != nil {
		t.Fatalf("NewStateFileResolver: %v", err)
	}
	if v, ok := r.Get("OPENAI_API_KEY"); !ok || v != "sk-openai" {
		t.Fatalf("Get = %q, %v; want sk-openai", v, ok)
	}

	if _, err := NewStateFileResolver(""); !errors.Is(err, ErrCredentialFileEmptyStateDir) {
		t.Fatalf("empty stateDir: err = %v, want ErrCredentialFileEmptyStateDir", err)
	}
}
