//go:build windows

package testutil

import "golang.org/x/sys/windows"

// ErrNoSpace returns the error a write gets when the disk is full.
func ErrNoSpace() error { return windows.ERROR_DISK_FULL }
