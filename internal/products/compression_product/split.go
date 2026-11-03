package compression_product

import "indicer/internal/duesarc"

// isArchiveName reports whether a name has an archive's extension, so that
// name.001 beside it is a part of a split archive written before sidecars
// existed.
func isArchiveName(p string) bool { return detectArchiveFormatFromPath(p) != "" }

func splitBase(p string) (string, bool) { return duesarc.SplitBase(p, isArchiveName) }

func openArchiveInput(p string) (*duesarc.Input, error) {
	return duesarc.OpenInput(p, isArchiveName)
}

func openZip(p string, verifyParts bool) (*duesarc.Zip, error) {
	return duesarc.OpenZip(p, verifyParts, isArchiveName)
}
