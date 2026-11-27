package secrets

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// credentialFileName is the fixed file name a StateDir credential source uses.
const credentialFileName = "secrets.env"

// maxCredentialFileSize caps a credential file at 64 KiB. A secrets.env is a
// handful of NAME=value lines; the cap bounds a read from an attacker-influenced
// path and is refused before the file is parsed.
const maxCredentialFileSize = 64 << 10

// Deterministic refusal classes for the hardened credential file. Every refusal
// wraps exactly one of these sentinels (plus human-readable detail naming the
// path), so a caller classifies with errors.Is. No timestamp or ambient data is
// included, so the same violation always yields the same error.
var (
	// ErrCredentialFileSymlink: the path is a symlink. Refused so a swap cannot
	// redirect the read to a file the steward did not intend to open.
	ErrCredentialFileSymlink = errors.New("secrets: credential file is a symlink")
	// ErrCredentialFileNotRegular: the path is not a regular file (directory,
	// FIFO, socket or device).
	ErrCredentialFileNotRegular = errors.New("secrets: credential file is not a regular file")
	// ErrCredentialFileTooLarge: the file exceeds maxCredentialFileSize.
	ErrCredentialFileTooLarge = errors.New("secrets: credential file exceeds size limit")
	// ErrCredentialFileOwner: the file is not owned by the current user (unix).
	ErrCredentialFileOwner = errors.New("secrets: credential file is not owned by the current user")
	// ErrCredentialFileMode: the file grants group/other access (unix).
	ErrCredentialFileMode = errors.New("secrets: credential file permissions are too open")
	// ErrCredentialFileEmptyStateDir: NewStateFileResolver got an empty StateDir.
	ErrCredentialFileEmptyStateDir = errors.New("secrets: empty state directory")
)

// FileResolver resolves credentials from a StateDir-based dotenv file: one
// `NAME=value` per line, blank lines and `#` comments ignored. A missing file
// yields no credentials (Get returns false), never an error; a malformed line is
// an error at construction. The production caller passes a path under the
// steward's StateDir (e.g. <StateDir>/secrets.env).
//
// The file is opened under a hardening contract (see openCredentialFile): the
// path must be a regular file (not a symlink, directory, FIFO or socket), owned
// by the current user, mode 0600, and no larger than maxCredentialFileSize. Any
// violation is a deterministic refusal — errors.Is against the
// ErrCredentialFile* sentinels.
//
// The trust boundary is honest: this hardening defends against a DIFFERENT
// user who could plant a symlink, swap the file, loosen its permissions, or
// point the path at an over-size target. It does NOT stop a process running as
// the SAME user — such a process can already read any file that user owns, so
// the OS user itself is the boundary. Only running the steward under a separate
// service account narrows that.
type FileResolver struct {
	values map[string]string
}

// NewFileResolver reads path once under the hardening contract described on
// FileResolver. A missing file is not an error (no credentials configured).
func NewFileResolver(path string) (*FileResolver, error) {
	f, err := openCredentialFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &FileResolver{values: map[string]string{}}, nil
		}
		return nil, err
	}
	defer f.Close()

	values := map[string]string{}
	sc := bufio.NewScanner(f)
	for line := 1; sc.Scan(); line++ {
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		i := strings.IndexByte(text, '=')
		if i <= 0 {
			return nil, fmt.Errorf("secrets: %s:%d: not a NAME=value line", path, line)
		}
		name := strings.TrimSpace(text[:i])
		if !validEnvName(name) {
			return nil, fmt.Errorf("secrets: %s:%d: invalid name %q", path, line, name)
		}
		values[name] = strings.TrimSpace(text[i+1:])
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("secrets: read %q: %w", path, err)
	}
	return &FileResolver{values: values}, nil
}

// NewStateFileResolver resolves <stateDir>/secrets.env, the production
// credential source. stateDir is a plain path: this package does not import
// internal/loop, so the caller passes loop.StateDir() itself. An empty stateDir
// is refused; NewFileResolver's errors (including a missing file, which is not
// an error) are returned unchanged.
func NewStateFileResolver(stateDir string) (*FileResolver, error) {
	if stateDir == "" {
		return nil, ErrCredentialFileEmptyStateDir
	}
	return NewFileResolver(filepath.Join(stateDir, credentialFileName))
}

// Get implements Resolver.
func (r *FileResolver) Get(name string) (string, bool) {
	v, ok := r.values[name]
	return v, ok
}
