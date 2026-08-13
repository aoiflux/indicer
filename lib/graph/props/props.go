// Package props packs and unpacks the msgpack property blobs carried by
// graphene nodes and edges.
//
// It replaces three near-duplicate helper sets that had drifted apart:
// lib/enrichment/graphene_repository.go, lib/microartefact/repository/graphene/
// query.go and .../repository.go. The drift was not cosmetic — the three
// disagreed on real inputs:
//
//   - the enrichment copy's int64 accessor handled only int64 and float64, so a
//     size that msgpack had encoded as uint64, int8 or int16 read back as 0
//     while the query copy read it correctly;
//   - the enrichment copy's bool accessor rejected the string "true";
//   - only the micro-artefact copy coerced float32 and the unsigned widths to int.
//
// The accessors here are the union, which is the correct behaviour: msgpack
// chooses its integer width from the *value*, not the Go type, so a field
// declared int64 comes back as int8 when it happens to be small. Any accessor
// that switches on a subset of the widths is a latent zero.
package props

import (
	"strconv"
	"time"

	"github.com/vmihailenco/msgpack/v5"
)

// Map is a decoded property blob. Accessors are total: a missing key, a nil
// map or a value of an unexpected type yields the zero value rather than an
// error, because a property blob is data DUES wrote itself and a decode
// mismatch is a bug to be surfaced by tests, not a condition every call site
// should branch on.
type Map map[string]any

// Pack encodes a value as msgpack.
func Pack(value any) ([]byte, error) {
	return msgpack.Marshal(value)
}

// MustPack panics on an encoding failure.
//
// Deprecated: prefer Pack. Panicking part-way through an ingest is the wrong
// behaviour for a forensic tool — it abandons the run with a partially written
// store. This exists to bound the diff while the write path moves onto
// transactions; new code should return the error.
func MustPack(value any) []byte {
	packed, err := msgpack.Marshal(value)
	if err != nil {
		panic("graph/props: pack: " + err.Error())
	}
	return packed
}

// Unpack decodes a property blob. It never returns nil, so callers can index
// the result unconditionally, and it never returns an error: an undecodable
// blob yields an empty Map.
func Unpack(packed []byte) Map {
	if len(packed) == 0 {
		return Map{}
	}
	var decoded map[string]any
	if err := msgpack.Unmarshal(packed, &decoded); err != nil {
		return Map{}
	}
	if decoded == nil {
		return Map{}
	}
	return decoded
}

// Has reports whether the key is present, regardless of its value.
func (m Map) Has(key string) bool {
	_, ok := m[key]
	return ok
}

// Str returns a string value. []byte is decoded as UTF-8; every other type
// yields "" rather than a formatted rendering, so a type mismatch stays
// visible instead of turning into plausible-looking output.
func (m Map) Str(key string) string {
	switch v := m[key].(type) {
	case string:
		return v
	case []byte:
		return string(v)
	default:
		return ""
	}
}

// Int64 returns an integer value, coercing across every width msgpack may have
// chosen and parsing decimal strings.
func (m Map) Int64(key string) int64 {
	switch v := m[key].(type) {
	case int:
		return int64(v)
	case int8:
		return int64(v)
	case int16:
		return int64(v)
	case int32:
		return int64(v)
	case int64:
		return v
	case uint:
		return int64(v)
	case uint8:
		return int64(v)
	case uint16:
		return int64(v)
	case uint32:
		return int64(v)
	case uint64:
		return int64(v)
	case float32:
		return int64(v)
	case float64:
		return int64(v)
	case string:
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return 0
		}
		return n
	default:
		return 0
	}
}

// Int is Int64 narrowed to int.
func (m Map) Int(key string) int { return int(m.Int64(key)) }

// Float64 returns a floating-point value, coercing from every integer width.
func (m Map) Float64(key string) float64 {
	switch v := m[key].(type) {
	case float32:
		return float64(v)
	case float64:
		return v
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return float64(m.Int64(key))
	case string:
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return 0
		}
		return f
	default:
		return 0
	}
}

// Float32 is Float64 narrowed to float32. Confidence scores are stored as
// float32, so msgpack round-trips them as float32 rather than float64.
func (m Map) Float32(key string) float32 { return float32(m.Float64(key)) }

// Bool returns a boolean value. Strings are parsed with strconv.ParseBool, and
// numbers are truthy when non-zero — both appear in stored blobs because index
// values were written with strconv.FormatBool.
func (m Map) Bool(key string) bool {
	switch v := m[key].(type) {
	case bool:
		return v
	case string:
		b, err := strconv.ParseBool(v)
		if err != nil {
			return false
		}
		return b
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return m.Int64(key) != 0
	case float32, float64:
		return m.Float64(key) != 0
	default:
		return false
	}
}

// Strings returns a string slice. msgpack decodes arrays as []any, so the
// element-wise path is the one that actually runs; []string is handled for
// values that never left the process. A lone string is promoted to a
// one-element slice.
func (m Map) Strings(key string) []string {
	switch v := m[key].(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			switch s := item.(type) {
			case string:
				out = append(out, s)
			case []byte:
				out = append(out, string(s))
			default:
				out = append(out, "")
			}
		}
		return out
	case string:
		if v == "" {
			return nil
		}
		return []string{v}
	default:
		return nil
	}
}

// Time returns a timestamp. Values may be a time.Time, an RFC 3339 string, or
// an integer count of Unix nanoseconds.
func (m Map) Time(key string) time.Time {
	switch v := m[key].(type) {
	case time.Time:
		return v
	case string:
		parsed, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			return time.Time{}
		}
		return parsed
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return time.Unix(0, m.Int64(key)).UTC()
	default:
		return time.Time{}
	}
}
