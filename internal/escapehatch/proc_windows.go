//go:build windows

package escapehatch

import "os/exec"

// configureSysProcAttr is a no-op on Windows: process groups are not created,
// so a cancel terminates only the direct child (its descendants may survive).
func configureSysProcAttr(cmd *exec.Cmd) {}

// killProcess kills the direct child process. It never panics on a nil
// process.
func killProcess(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
