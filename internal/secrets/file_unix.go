//go:build unix

package secrets

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// openCredentialFile opens path read-only and enforces the unix hardening
// contract:
//
//   - the path is not a symlink (an Lstat pre-check plus O_NOFOLLOW on the open,
//     so the check cannot be raced by a swap);
//   - the path is a regular file (directory/FIFO/socket/device refused);
//   - the opened file is owned by the current uid;
//   - the opened file is mode 0600 (any group/other bit is refused);
//   - the file is no larger than maxCredentialFileSize.
//
// O_NONBLOCK is set so a FIFO that slips past the Lstat check can never block
// the open. A missing file returns an error wrapping os.ErrNotExist, which the
// caller treats as "no credentials". Every other refusal wraps exactly one
// ErrCredentialFile* sentinel and names the path.
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

	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("secrets: open %q: %w", path, os.ErrNotExist)
		}
		if errors.Is(err, syscall.ELOOP) {
			// The final component became a symlink between Lstat and open.
			return nil, fmt.Errorf("%w: %q", ErrCredentialFileSymlink, path)
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
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		f.Close()
		return nil, fmt.Errorf("secrets: stat %q: missing unix metadata", path)
	}
	if int(st.Uid) != os.Getuid() {
		f.Close()
		return nil, fmt.Errorf("%w: %q (uid %d)", ErrCredentialFileOwner, path, st.Uid)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		f.Close()
		return nil, fmt.Errorf("%w: %q (mode %04o)", ErrCredentialFileMode, path, fi.Mode().Perm())
	}
	return f, nil
}
