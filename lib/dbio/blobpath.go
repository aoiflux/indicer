package dbio

import (
	"path/filepath"
	"strings"

	"indicer/lib/cnst"
	"indicer/lib/util"
)

// blobPathPrefix starts every chunk path recorded from this pass on: C|||
// and block index paths are relative to the data root, with forward slashes,
// so a data root can be moved, copied or opened from another machine
// (STO-10).
const blobPathPrefix = cnst.BLOBSDIR + "/"

// StoredBlobPath returns the form in which C||| records path, a blob or
// container in the blob directory of the database at dbDir: "blob/<name>".
// A path outside that directory is returned unchanged.
func StoredBlobPath(dbDir, path string) string {
	rel, err := filepath.Rel(util.BlobPath(dbDir), path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return path
	}
	return blobPathPrefix + filepath.ToSlash(rel)
}

// ResolveBlobPath returns the file a recorded chunk path names in the
// database at dbDir (its root or its kvdb directory).
//
// A path in the stored form resolves under the data root. Any other path was
// written before this pass, absolute, by whichever machine and data root
// wrote it; it resolves to the file of that name in this database's blob
// directory, where every blob and container lives. It never resolves to the
// recorded location, which may now belong to another case.
func ResolveBlobPath(dbDir, stored string) string {
	root := util.ResolveDataRoot(dbDir)
	if strings.HasPrefix(stored, blobPathPrefix) {
		return filepath.Join(root, filepath.FromSlash(stored))
	}
	base := stored[strings.LastIndexAny(stored, `/\`)+1:]
	return filepath.Join(root, cnst.BLOBSDIR, base)
}
