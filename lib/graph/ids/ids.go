// Package ids derives the deterministic string identifiers DUES stores as
// graph node properties and index values.
//
// These identifiers are persisted, so the derivations here are a compatibility
// surface, not an implementation detail: changing one orphans every node in
// every existing store that was keyed by it. ids_test.go pins each against
// values taken from the checked-in legacy fixture for exactly that reason.
package ids

import (
	"crypto/sha256"
	"encoding/hex"
)

// HashText hashes a value and any number of additional components, separating
// them with a NUL byte so that ("ab", "c") and ("a", "bc") differ.
//
// This is the single definition of a derivation that was previously copied
// verbatim into lib/enrichment/types.go, lib/microartefact/model/file_record.go
// and lib/microartefact/repository/graphene/repository.go. The three agreed, so
// consolidating changes no stored value — which the tests assert rather than
// assume.
func HashText(value string, rest ...string) string {
	hasher := sha256.New()
	_, _ = hasher.Write([]byte(value))
	for _, next := range rest {
		_, _ = hasher.Write([]byte{0})
		_, _ = hasher.Write([]byte(next))
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

// ArtefactLookupKey keys artefact nodes by kind and value while relation edges
// are being resolved within one file.
//
// It is deliberately NUL-joined rather than hashed: this key never leaves the
// process, and keeping it readable makes a failed relation lookup legible in a
// debugger.
func ArtefactLookupKey(kind, value string) string {
	return kind + "\x00" + value
}
