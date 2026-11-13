package configfile

import (
	"io/fs"
	"os"
)

func open(path string) (*os.File, error) { return os.Open(path) }

// checkAccess checks nothing: Windows keeps who may change a file in its
// access control list, which the file inherits from its folder. A file
// outside the user's profile, such as a token file, is only as safe as its
// folder's list.
func checkAccess(fs.FileInfo) string { return "" }
