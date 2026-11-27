//go:build windows

package secrets

import (
	"errors"
	"fmt"
	"os"
)

// openCredentialFile opens path read-only on Windows. Windows has no O_NOFOLLOW
// and no portable POSIX uid/permission model, so only the portable parts of the
// hardening contract are enforced:
//
//   - the path is not a symlink (an Lstat pre-check);
//   - the path is a regular file (directory/socket/device refused);
//   - the file is no larger than maxCredentialFileSize.
//
// The unix owner (uid) and mode-0600 checks are NOT enforced here: on Windows
// file access is governed by NTFS ACLs, not by an open-time mode/uid test, so
// the ACL is the trust boundary. This is the documented, graceful degradation.
// A missing file returns an error wrapping os.ErrNotExist, which the caller
// treats as "no credentials"; other refusals wrap a ErrCredentialFile* sentinel
// and name the path.
func openCredentialFile(path string) (*os.File, error) {
	li, err := os.Lstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, fmt.Errorf("secrets: stat %q: %w", path, os.ErrNotExist)
	case err != nil:
		return nil, fmt.Errorf("secrets: stat %q: %w", path, err)
	case li.Mode()&os.ModeSymlink != 0:
		return nil, fmt.Errorf("%w: %q", ErrCredentialFileSymlink, path)
	case !li.Mode().IsRegular():
		return nil, fmt.Errorf("%w: %q", ErrCredentialFileNotRegular, path)
	}

	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("secrets: open %q: %w", path, os.ErrNotExist)
		}
		return nil, fmt.Errorf("secrets: open %q: %w", path, err)
	}

	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("secrets: stat %q: %w", path, err)
	}
	if !fi.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("%w: %q", ErrCredentialFileNotRegular, path)
	}
	if fi.Size() > maxCredentialFileSize {
		f.Close()
		return nil, fmt.Errorf("%w: %q (%d bytes)", ErrCredentialFileTooLarge, path, fi.Size())
	}
	return f, nil
}
