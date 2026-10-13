//go:build !windows

package testutil

import (
	"os"
	"os/exec"
)

func prepareInterruptible(*exec.Cmd) {}

func interrupt(cmd *exec.Cmd) error { return cmd.Process.Signal(os.Interrupt) }
