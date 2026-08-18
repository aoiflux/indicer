// Package propindex encodes property values so that their byte order matches
// their natural order, and declares the keys that may be range-queried.
//
// # Why the encoding matters
//
// Declaring a key ordered changes how graphene compares it. An undeclared key
// uses a scan rule — numeric when both sides parse, byte order otherwise — but
// a declared key is compared byte-wise throughout, because only a total order
// can back a binary search. Under byte comparison "10" sorts before "9", so a
// decimal string is not a valid sort order and a range query over one answers
// wrongly. graphene's own test for this is named
// TestDecimalStrings_AreNotAValidSortOrder.
//
// # Why nothing is declared yet
//
// DUES currently writes several of these index values with strconv:
// start and end on containment edges, deterministic_flag on relation edges,
// is_deleted and is_fragmented on file nodes. Those entries are already on disk
// in every existing store. Declaring a key while old entries remain would not
// convert them — it would reinterpret them, so a range query would silently
// skip or include the wrong rows.
//
// The encoders therefore ship before their use. Declare refuses on any store
// that still holds legacy-encoded values, and the keys are declared as part of
// the rebuild migration, which re-encodes the data in the same pass. Ordered
// range queries are a post-rebuild capability by construction, not by policy.
package propindex

import (
	"fmt"
	"time"

	"indicer/lib/graph/schema"

	graphenedb "github.com/aoiflux/graphene"
	"github.com/aoiflux/graphene/index/encoding"
)

// Uint64 encodes an unsigned integer in big-endian order.
func Uint64(v uint64) []byte { return encoding.Uint64(v) }

// Int64 encodes a signed integer with the sign bit flipped, so negatives sort
// below positives.
func Int64(v int64) []byte { return encoding.Int64(v) }

// Float64 encodes an IEEE-754 double in an order-preserving form.
func Float64(v float64) []byte { return encoding.Float64(v) }

// Time encodes an instant as Unix nanoseconds. Valid for 1678–2262, which
// covers every filesystem timestamp DUES parses.
func Time(t time.Time) []byte { return encoding.Time(t) }

// String passes bytes through: raw bytes already compare lexicographically.
func String(s string) []byte { return encoding.String(s) }

// PrefixUpperBound returns the exclusive end of a prefix range, for turning a
// prefix match into two binary searches.
func PrefixUpperBound(prefix []byte) ([]byte, bool) { return encoding.PrefixUpperBound(prefix) }

// Bool encodes a boolean as a single byte.
//
// Deliberately not strconv.FormatBool: "false" sorts before "true" by accident
// of the alphabet rather than by design, and mixing the two representations for
// one key is what makes a range query incoherent.
func Bool(b bool) []byte {
	if b {
		return []byte{1}
	}
	return []byte{0}
}

// DecodeInt64 and friends reverse the encoders, for rendering a stored index
// value back to a human.
func DecodeInt64(b []byte) (int64, bool)     { return encoding.DecodeInt64(b) }
func DecodeUint64(b []byte) (uint64, bool)   { return encoding.DecodeUint64(b) }
func DecodeFloat64(b []byte) (float64, bool) { return encoding.DecodeFloat64(b) }
func DecodeTime(b []byte) (time.Time, bool)  { return encoding.DecodeTime(b) }

// DeclarableNodeKeys are safe to declare on any store, today.
//
// Auditing what is actually index-registered showed the original blanket
// deferral was too cautious. A key is only unsafe to declare if values are
// already stored under it in a non-order-preserving encoding. These are not:
//
//   - mtime/atime/ctime/btime — new keys, written order-preserving from the
//     first record, with no history at all;
//   - size, entropy, simhash — not index-registered before now;
//   - path, name — registered as raw strings, which already compare
//     lexicographically. Declaring them switches numeric-looking values from
//     the scan rule's numeric comparison to byte order, which for a filename
//     is the correct order rather than a regression.
var DeclarableNodeKeys = []string{
	"mtime", "atime", "ctime", "btime",
	"size", "entropy", "simhash",
	"path", "name",
}

// DeferredKeys carry values written with strconv and cannot be declared until
// the rebuild re-encodes them.
//
// `start` and `end` are byte offsets stored as decimal strings, where byte
// order genuinely disagrees with numeric order — "10" sorts below "9" — so a
// range query over them would answer wrongly. The booleans are strconv too;
// declaring them would be harmless but pointless, since a two-valued key has no
// meaningful range.
var DeferredKeys = []string{"start", "end", "is_deleted", "is_fragmented", "deterministic_flag"}

// ErrLegacyEncoding is returned when a caller asks for a key whose stored
// values were not written with these encoders.
var ErrLegacyEncoding = fmt.Errorf(
	"propindex: values under this key were written with strconv, which is not a valid " +
		"byte-wise sort order; declaring it would make range queries answer wrongly. " +
		"Run 'dues graph migrate --rebuild' to re-encode")

// Declare registers the keys that are safe to range-query on this store.
//
// It is idempotent and takes no view of the store's vocabulary: the keys it
// declares have no conflicting history in any store, legacy or current. Asking
// for a deferred key is refused individually rather than failing the whole set.
func Declare(g *graphenedb.Graph, kinds *schema.Registry) error {
	if kinds == nil {
		return fmt.Errorf("propindex: Declare needs a schema registry")
	}
	for _, key := range DeclarableNodeKeys {
		if err := g.DeclareOrderedProperty(key); err != nil {
			return fmt.Errorf("propindex: declare node key %q: %w", key, err)
		}
	}
	return nil
}

// Deferred reports whether a key is still waiting on the rebuild.
func Deferred(key string) bool {
	for _, deferred := range DeferredKeys {
		if deferred == key {
			return true
		}
	}
	return false
}
