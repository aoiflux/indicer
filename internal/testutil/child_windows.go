//go:build windows

package testutil

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// prepareInterruptible starts the child in its own process group, so a
// CTRL_BREAK_EVENT can target it without also hitting the test process.
func prepareInterruptible(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP
}

func interrupt(cmd *exec.Cmd) error {
	return windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(cmd.Process.Pid))
}
