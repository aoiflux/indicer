package fsx

import "os"

// CreateTempDeleteOnClose creates a new temporary file in dir (os.TempDir()
// when dir is empty), open for reading and writing, that the file system
// removes once it is closed, including when the process dies. Use it for
// scratch data, such as decompressed evidence, that must not outlive the
// process. pattern works as for os.CreateTemp.
//
// On Unix the file is unlinked as soon as it is created, so it has no name.
// On Windows it keeps its name while open, and is created with
// FILE_FLAG_DELETE_ON_CLOSE.
func CreateTempDeleteOnClose(dir, pattern string) (*os.File, error) {
	if dir == "" {
		dir = os.TempDir()
	}
	return createDeleteOnClose(dir, pattern)
}
