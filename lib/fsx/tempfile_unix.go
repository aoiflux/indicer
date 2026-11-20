//go:build !windows

package fsx

import (
	"fmt"
	"os"
)

func createDeleteOnClose(dir, pattern string) (*os.File, error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return nil, fmt.Errorf("fsx: create temporary file in %s: %w", dir, err)
	}
	if err := os.Remove(f.Name()); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, fmt.Errorf("fsx: unlink temporary file %s: %w", f.Name(), err)
	}
	return f, nil
}
