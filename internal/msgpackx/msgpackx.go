// Package msgpackx decodes MessagePack that DUES cannot assume it wrote
// intact: records read back from a database, which damage or a crafted
// archive can change.
//
// github.com/vmihailenco/msgpack/v5 (v5.4.1, the latest) allocates a typed
// slice at the length its header declares. The check meant to cap that at a
// million elements compares a flag mask with 1 (decode_slice.go), so it never
// applies, and five bytes can declare four billion elements. The decoder then
// copies the new slice into place, touching every page, and the process is
// killed for memory, which no recover can stop. Unmarshal walks the value
// first and refuses a container that declares more elements than the bytes
// left can hold (every element takes at least one), so what the decoder
// allocates is bounded by the size of its input.
package msgpackx

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/vmihailenco/msgpack/v5"
)

// MaxDepth is how deeply containers may nest. DUES's records nest a handful
// of levels; the decoder recurses once per level.
const MaxDepth = 64

// ErrInvalid means data is not one well-formed MessagePack value: it is
// truncated, uses the reserved code 0xc1, declares more elements than it
// holds, or nests deeper than MaxDepth.
var ErrInvalid = errors.New("invalid msgpack value")

// Unmarshal decodes the first value in data into v, as msgpack.Unmarshal
// does, once Check accepts it.
func Unmarshal(data []byte, v any) error {
	if err := Check(data); err != nil {
		return err
	}
	return msgpack.Unmarshal(data, v)
}

// Check walks the first value in data without decoding it. Bytes after that
// value are not looked at, as msgpack.Unmarshal ignores them.
func Check(data []byte) error {
	var buf [MaxDepth + 1]uint64
	// stack holds how many values each open container still holds; the
	// bottom entry is the one top-level value.
	stack := append(buf[:0], 1)
	pos := 0
	for len(stack) > 0 {
		top := len(stack) - 1
		if stack[top] == 0 {
			stack = stack[:top]
			continue
		}
		stack[top]--
		if pos >= len(data) {
			return invalidAt(pos, "value is truncated")
		}
		c := data[pos]
		pos++
		var skip, children uint64
		var err error
		switch {
		case c <= 0x7f || c >= 0xe0: // positive and negative fixint
		case c <= 0x8f: // fixmap
			children = 2 * uint64(c&0x0f)
		case c <= 0x9f: // fixarray
			children = uint64(c & 0x0f)
		case c <= 0xbf: // fixstr
			skip = uint64(c & 0x1f)
		default:
			switch c {
			case 0xc0, 0xc2, 0xc3: // nil, false, true
			case 0xc1:
				return invalidAt(pos-1, "reserved code 0xc1")
			case 0xc4, 0xd9: // bin8, str8
				skip, err = length(data, &pos, 1)
			case 0xc5, 0xda: // bin16, str16
				skip, err = length(data, &pos, 2)
			case 0xc6, 0xdb: // bin32, str32
				skip, err = length(data, &pos, 4)
			case 0xc7: // ext8: length, type, data
				skip, err = length(data, &pos, 1)
				skip++
			case 0xc8: // ext16
				skip, err = length(data, &pos, 2)
				skip++
			case 0xc9: // ext32
				skip, err = length(data, &pos, 4)
				skip++
			case 0xcc, 0xd0: // uint8, int8
				skip = 1
			case 0xcd, 0xd1: // uint16, int16
				skip = 2
			case 0xca, 0xce, 0xd2: // float32, uint32, int32
				skip = 4
			case 0xcb, 0xcf, 0xd3: // float64, uint64, int64
				skip = 8
			case 0xd4, 0xd5, 0xd6, 0xd7, 0xd8: // fixext 1, 2, 4, 8, 16: type, data
				skip = 1 + 1<<(c-0xd4)
			case 0xdc: // array16
				children, err = length(data, &pos, 2)
			case 0xdd: // array32
				children, err = length(data, &pos, 4)
			case 0xde: // map16
				children, err = length(data, &pos, 2)
				children *= 2
			case 0xdf: // map32
				children, err = length(data, &pos, 4)
				children *= 2
			}
		}
		if err != nil {
			return err
		}
		left := uint64(len(data) - pos)
		if skip > left {
			return invalidAt(pos, fmt.Sprintf("%d bytes declared, %d left", skip, left))
		}
		pos += int(skip)
		if children == 0 {
			continue
		}
		if children > left {
			return invalidAt(pos, fmt.Sprintf("%d elements declared, %d bytes left", children, left))
		}
		if len(stack) > MaxDepth {
			return invalidAt(pos, fmt.Sprintf("nested deeper than %d", MaxDepth))
		}
		stack = append(stack, children)
	}
	return nil
}

// length reads a big-endian length of size bytes at *pos.
func length(data []byte, pos *int, size int) (uint64, error) {
	if len(data)-*pos < size {
		return 0, invalidAt(*pos, "length is truncated")
	}
	b := data[*pos : *pos+size]
	*pos += size
	switch size {
	case 1:
		return uint64(b[0]), nil
	case 2:
		return uint64(binary.BigEndian.Uint16(b)), nil
	default:
		return uint64(binary.BigEndian.Uint32(b)), nil
	}
}

func invalidAt(pos int, why string) error {
	return fmt.Errorf("%w at byte %d: %s", ErrInvalid, pos, why)
}
