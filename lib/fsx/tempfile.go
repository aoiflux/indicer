package fsx

import (
	"errors"
	"os"
)

// CreateTempDeleteOnClose creates a new temporary file in dir, open for
// reading and writing, that the file system removes once it is closed,
// including when the process dies. Use it for scratch data, such as
// decompressed evidence, that must not outlive the process. pattern works
// as for os.CreateTemp. dir must be named: an empty one is an error, not
// os.TempDir, which reads the environment.
//
// On Unix the file is unlinked as soon as it is created, so it has no name.
// On Windows it keeps its name while open, and is created with
// FILE_FLAG_DELETE_ON_CLOSE.
func CreateTempDeleteOnClose(dir, pattern string) (*os.File, error) {
	if dir == "" {
		return nil, errors.New("create a temporary file: no directory given")
	}
	return createDeleteOnClose(dir, pattern)
}
