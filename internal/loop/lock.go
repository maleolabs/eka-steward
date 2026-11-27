package loop

import "errors"

// ErrLocked is returned by Acquire when another steward already holds the
// single-instance lock on the same path. Test with errors.Is.
var ErrLocked = errors.New("loop: another steward instance holds the lock")

// The single-instance lock is split by build tag so each OS uses the
// strongest crash-safe primitive it has:
//
//   - lock_unix.go  (//go:build unix): flock(2) with LOCK_EX|LOCK_NB. The
//     kernel drops the flock when the owning process dies, so a killed
//     steward never wedges a restart.
//   - lock_windows.go (//go:build windows): exclusive file creation
//     (O_CREATE|O_EXCL), removed on Release. A crash leaves the file behind
//     (a documented limitation: a restart after a crash refuses until the
//     file is removed manually).
//
// Acquire returns ErrLocked when the lock is already held; Release releases
// it. See docs/control-loop.md for the full semantics.
