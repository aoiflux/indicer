package configfile

import (
	"io/fs"
	"os"
)

func open(path string) (*os.File, error) { return os.Open(path) }

// checkAccess checks nothing: Windows keeps who may change a file in its
// access control list, which the user's profile folder already limits to
// the user and administrators.
func checkAccess(fs.FileInfo) string { return "" }
