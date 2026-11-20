//go:build !windows

package testutil

import "syscall"

// ErrNoSpace returns the error a write gets when the disk is full.
func ErrNoSpace() error { return syscall.ENOSPC }
