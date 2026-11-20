// Package partition parses partition tables (MBR/GPT/APM/BSD/Sun) over a
// core.Image using libtable, returning a normalised core.PartitionTable with
// absolute byte offsets.
package partition

import (
	"errors"
	"fmt"

	"github.com/aoiflux/libtable"

	"indicer/internal/safe"
	"indicer/lib/parser/core"
)

// Parse reads the partition table over img. When no supported partition scheme
// is present the whole device is returned as a single Scheme=none partition, so
// the filesystem layer can still attempt a partitionless ("superfloppy") mount —
// mirroring the legacy whole-disk fallback.
func Parse(img core.Image) (core.PartitionTable, error) {
	t, _, err := ParseReport(img)
	return t, err
}

// ParseReport is Parse plus what the table got wrong: libtable's warnings
// (overlaps, entries outside the image, a damaged GPT backup) and every
// partition cut to fit the image (PAR-17). A partition that starts past the
// end of the image is kept with size 0, so it is still reported.
func ParseReport(img core.Image) (core.PartitionTable, []core.Issue, error) {
	size := img.Size()

	tbl, err := safe.Value(safe.Site{Component: "parser/partition", Op: "parse"}, func() (*libtable.Table, error) {
		return libtable.Parse(img, uint64(size), libtable.Options{})
	})
	if err != nil {
		if errors.Is(err, libtable.ErrUnknownTable) {
			return wholeDevice(size), nil, nil
		}
		return core.PartitionTable{}, nil, err
	}
	var issues []core.Issue
	for _, w := range tbl.Warnings {
		code := core.IssueTable
		if w.Code == libtable.WarnOverlap {
			code = core.IssueOverlap
		}
		issues = append(issues, core.Issue{Code: code, Detail: fmt.Sprintf("%s: %s (LBA %d)", w.Code, w.Msg, w.LBA)})
	}
	out, more := convert(tbl, size)
	return out, append(issues, more...), nil
}

func convert(tbl *libtable.Table, imageSize int64) (core.PartitionTable, []core.Issue) {
	out := core.PartitionTable{
		Scheme:    mapScheme(tbl.Type),
		BlockSize: tbl.BlockSize,
	}
	var issues []core.Issue
	for _, p := range tbl.Partitions {
		// ByteRange is ok only for a range below 2^63, so the conversions
		// cannot wrap.
		uoff, un, ok := tbl.ByteRange(p)
		off, n := int64(uoff), int64(un)
		cp := core.Partition{
			Index:      p.Index,
			TypeCode:   p.TypeCode,
			TypeName:   p.TypeName,
			Name:       p.Name,
			GUIDType:   p.GUIDType,
			GUIDUnique: p.GUIDUnique,
			Allocated:  p.Flags&libtable.PartFlagAlloc != 0,
		}
		switch {
		case !ok:
			cp.Offset, cp.Size = imageSize, 0
			issues = append(issues, core.Issue{Code: core.IssueClamped, Detail: fmt.Sprintf("partition %d: LBA %d+%d overflows", p.Index, p.StartLBA, p.LengthLBA)})
		case off >= imageSize:
			cp.Offset, cp.Size = off, 0
			if cp.Allocated {
				issues = append(issues, core.Issue{Code: core.IssueClamped, Detail: fmt.Sprintf("partition %d starts at %d, past the image end %d", p.Index, off, imageSize)})
			}
		case n > imageSize-off:
			cp.Offset, cp.Size = off, imageSize-off
			if cp.Allocated {
				issues = append(issues, core.Issue{Code: core.IssueClamped, Detail: fmt.Sprintf("partition %d [%d, +%d) cut to the image end %d", p.Index, off, n, imageSize)})
			}
		default:
			cp.Offset, cp.Size = off, n
		}
		out.Partitions = append(out.Partitions, cp)
	}
	return out, issues
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
