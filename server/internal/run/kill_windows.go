//go:build windows

package run

import (
	"fmt"
	"os/exec"
	"strconv"
	"syscall"
)

// prepareCmd is a no-op on Windows: there is no process group to set up before
// starting. The tree is torn down by PID after the fact, via taskkill /T.
func prepareCmd(cmd *exec.Cmd) {}

// KillTree kills the process and every descendant.
//
// Killing only the node process is not enough: Playwright's browser processes
// are children, and they survive — which on a shared server means Chromium
// accumulating until the box runs out of memory. taskkill /T walks the tree,
// which is exactly what the previous Python runner had to do too.
func KillTree(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("invalid pid %d", pid)
	}
	cmd := exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(pid))
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Exit code 128 means "no such process" — already gone, which is the
		// outcome we wanted anyway.
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 128 {
			return nil
		}
		return fmt.Errorf("taskkill pid %d: %w (%s)", pid, err, out)
	}
	return nil
}
