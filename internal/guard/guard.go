// Package guard is the steward's containment layer. It keeps a worker
// inside a known environment, a known working tree and a known tool set,
// and it keeps the steward's ACP permission channel deny-by-default.
//
// Four independent layers, each default-on and each with an honest
// ceiling (see docs/guardrails.md):
//
//  1. Environment (env.go) — a worker runs with the allow-listed worker
//     environment only: no HOME, no EKA_HOME, no PATH, no credential
//     variable, so it cannot reach the default EKA store or a host
//     credential dotfile through the ambient environment.
//  2. Worktree (worktree.go) — each worker gets its own git worktree.
//     This is VCS isolation, NOT a security boundary: a process with
//     filesystem access can still read outside it.
//  3. Permission policy (permission.go) — an ACP permission request is
//     denied by default; only a read-class request inside the workspace
//     root may be allowed; every wait is bounded so a worker never hangs.
//  4. Driver pinning (pinning.go) — every driver carries an exact version
//     and a sha256 checksum; an unpinned or mismatched driver is refused.
//
// Containment is ENABLED BY DEFAULT and there is no opt-out in v1: the
// zero value of Config is contained.
package guard

// Config is the steward's containment configuration. Its ZERO VALUE IS
// CONTAINED: containment is on by default and v1 has no field to turn it
// off.
//
// There is deliberately NO Disable / Bypass / OptOut switch — adding one
// would make "contained" a configuration choice instead of a property, so
// it is omitted on purpose. Config.Contained always reports true; the
// method exists so the invariant is asserted by a test and a future
// opt-out could not be added silently.
type Config struct {
	// Permission decides ACP permission requests. The zero Policy is
	// default-deny.
	Permission Policy
	// WorktreeBase is the parent directory for per-worker worktrees. The
	// zero value imposes no directory; a caller that uses worktrees
	// supplies a base.
	WorktreeBase string
}

// Contained reports whether containment is in force. It is always true:
// the zero Config is contained and v1 has no opt-out.
func (c Config) Contained() bool { return true }
