//go:build !windows

package run

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"
)

// prepareCmd puts the child in its own process group, so the whole tree can be
// signalled at once. Without this, killing the node process leaves Playwright's
// browser processes running.
func prepareCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// KillTree signals the process group: SIGTERM first so Playwright can flush its
// reporters and write partial artifacts, then SIGKILL for anything still alive.
//
// The negative pid targets the group — prepareCmd made the child its leader, so
// its group id equals its pid.
func KillTree(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("invalid pid %d", pid)
	}

	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil // already gone
		}
		return fmt.Errorf("SIGTERM group %d: %w", pid, err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(-pid, 0); errors.Is(err, syscall.ESRCH) {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}

	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("SIGKILL group %d: %w", pid, err)
	}
	return nil
}
