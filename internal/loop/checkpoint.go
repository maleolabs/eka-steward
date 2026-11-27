package loop

import (
	"fmt"
	"os"
	"path/filepath"
)

// The checkpoint is the loop's persisted State file. Its location is explicit
// and deterministic so a restart finds it without guessing:
//
//	$EKA_STEWARD_STATE_DIR/checkpoint.json   (env override)
//	$EKA_HOME/steward/checkpoint.json        (when EKA_HOME is set)
//	<user home>/.eka/steward/checkpoint.json (default)
//
// EKA_STEWARD_STATE_DIR is the documented override: it names the directory the
// steward owns for its own state, deliberately separate from the EKA workspace
// store (a checkpoint is steward bookkeeping, not EKA knowledge). See
// docs/checkpoint.md.
const (
	// StateDirEnv is the documented environment override for the checkpoint
	// directory.
	StateDirEnv = "EKA_STEWARD_STATE_DIR"
	// CheckpointFile is the checkpoint file name inside the state directory.
	CheckpointFile = "checkpoint.json"
)

// StateDir returns the directory the loop's checkpoint lives in, resolving the
// documented precedence: EKA_STEWARD_STATE_DIR, then $EKA_HOME/steward, then
// <user home>/.eka/steward. The directory is created on first Save (Save writes
// its temp file beside the target and rename requires the directory to exist;
// the caller may create it, and Save does not create parent directories by
// design so a misconfigured path fails loudly). Use EnsureStateDir to create
// it.
func StateDir() (string, error) {
	if d := os.Getenv(StateDirEnv); d != "" {
		return d, nil
	}
	if eka := os.Getenv("EKA_HOME"); eka != "" {
		return filepath.Join(eka, "steward"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("loop: cannot resolve the state directory (%s unset, no EKA_HOME, no home): %w", StateDirEnv, err)
	}
	return filepath.Join(home, ".eka", "steward"), nil
}

// DefaultStatePath returns the full checkpoint path under StateDir.
func DefaultStatePath() (string, error) {
	dir, err := StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, CheckpointFile), nil
}

// EnsureStateDir resolves the state directory and creates it (and any parents).
func EnsureStateDir() (string, error) {
	dir, err := StateDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("loop: create state directory %s: %w", dir, err)
	}
	return dir, nil
}
