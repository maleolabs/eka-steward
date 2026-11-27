//go:build !windows

package escapehatch

import (
	"os/exec"
	"syscall"
)

// configureSysProcAttr starts the child in its own process group so a cancel
// can terminate the whole tree the agent spawns, not just the direct child.
func configureSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcess kills the child's process group (falling back to the single
// process if the group is already gone). It never panics on a nil process.
func killProcess(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}
