package guard

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/maleolabs/eka-steward/v2/internal/driver"
)

// ErrUnpinned is returned (wrapped) when a driver is not pinned or its
// binary does not match its pin. Test with errors.Is.
var ErrUnpinned = errors.New("guard: driver not pinned")

// RequirePinned refuses every driver without an exact Version and a sha256
// Checksum, then verifies each pinned binary's checksum. The error is
// deterministic and names the tool and what is missing or wrong.
//
// Resolution uses os + the PATH only (no os/exec): a cmd containing a path
// separator is used as-is; a bare name is looked up on PATH. A binary that
// cannot be resolved, or whose checksum does not match (case-insensitive
// hex), is refused. An empty registry is vacuously pinned.
func RequirePinned(reg *driver.Registry) error {
	for _, tool := range reg.Names() {
		spec, err := reg.Resolve(tool)
		if err != nil {
			return err // Names comes from reg, so this cannot happen
		}
		if spec.Version == "" {
			return fmt.Errorf("%w: tool %q has no version", ErrUnpinned, tool)
		}
		if spec.Checksum == "" {
			return fmt.Errorf("%w: tool %q has no checksum", ErrUnpinned, tool)
		}
		path, ok := resolveBinary(spec.Cmd)
		if !ok {
			return fmt.Errorf("%w: tool %q binary %q not found", ErrUnpinned, tool, spec.Cmd)
		}
		got, err := fileSHA256(path)
		if err != nil {
			return fmt.Errorf("%w: tool %q: %v", ErrUnpinned, tool, err)
		}
		if !strings.EqualFold(got, spec.Checksum) {
			return fmt.Errorf("%w: tool %q checksum mismatch: got %s want %s", ErrUnpinned, tool, got, spec.Checksum)
		}
	}
	return nil
}

// resolveBinary resolves cmd to a regular file: a path (containing a path
// separator) is taken as-is; a bare name is searched on PATH. It never
// spawns a process.
func resolveBinary(cmd string) (string, bool) {
	if cmd == "" {
		return "", false
	}
	if strings.ContainsRune(cmd, os.PathSeparator) || strings.ContainsRune(cmd, '/') {
		return cmd, isRegularFile(cmd)
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			dir = "."
		}
		if p := filepath.Join(dir, cmd); isRegularFile(p) {
			return p, true
		}
	}
	return "", false
}

// isRegularFile reports whether path exists and is not a directory.
func isRegularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// fileSHA256 returns the lowercase hex sha256 of the file at path.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
