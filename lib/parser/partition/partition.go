// Package partition parses partition tables (MBR/GPT/APM/BSD/Sun) over a
// core.Image using libtable, returning a normalised core.PartitionTable with
// absolute byte offsets.
package partition

import (
	"errors"

	"github.com/aoiflux/libtable"

	"indicer/lib/parser/core"
)

// Parse reads the partition table over img. When no supported partition scheme
// is present the whole device is returned as a single Scheme=none partition, so
// the filesystem layer can still attempt a partitionless ("superfloppy") mount —
// mirroring the legacy whole-disk fallback.
func Parse(img core.Image) (core.PartitionTable, error) {
	size := img.Size()

	tbl, err := libtable.Parse(img, uint64(size), libtable.Options{})
	if err != nil {
		if errors.Is(err, libtable.ErrUnknownTable) {
			return wholeDevice(size), nil
		}
		return core.PartitionTable{}, err
	}
	return convert(tbl), nil
}

func convert(tbl *libtable.Table) core.PartitionTable {
	out := core.PartitionTable{
		Scheme:    mapScheme(tbl.Type),
		BlockSize: tbl.BlockSize,
	}
	for _, p := range tbl.Partitions {
		out.Partitions = append(out.Partitions, core.Partition{
			Index:      p.Index,
			Offset:     int64(tbl.ByteOffset(p)),
			Size:       int64(tbl.ByteSize(p)),
			TypeCode:   p.TypeCode,
			TypeName:   p.TypeName,
			Name:       p.Name,
			GUIDType:   p.GUIDType,
			GUIDUnique: p.GUIDUnique,
			Allocated:  p.Flags&libtable.PartFlagAlloc != 0,
		})
	}
	return out
}

func wholeDevice(size int64) core.PartitionTable {
	return core.PartitionTable{
		Scheme: core.SchemeNone,
		Partitions: []core.Partition{{
			Index:     0,
			Offset:    0,
			Size:      size,
			TypeName:  "whole-device",
			Allocated: true,
		}},
	}
}

func mapScheme(t libtable.TableType) core.PartitionScheme {
	switch t {
	case libtable.TypeMBR:
		return core.SchemeMBR
	case libtable.TypeGPT:
		return core.SchemeGPT
	case libtable.TypeMac:
		return core.SchemeAPM
	case libtable.TypeBSD:
		return core.SchemeBSD
	case libtable.TypeSun:
		return core.SchemeSun
	default:
		return core.SchemeNone
	}
}
