// Package tskcompat renders the pure-Go scan output into the exact JSON shape
// produced by libtusk_analyze. It is a debug renderer: ingest indexes through
// lib/index since W6, and this output lets the scan be compared with libtusk.
//
// The JSON shape mirrors clib/LIBTUSK_ABI.md:
//
//	{ "image", "partitions": [ { "name","start_offset","end_offset",
//	    "filesystem": {"type","block_size","offset"},
//	    "files": [ {"filename","type","is_fragmented","is_deleted","size",
//	                "fragments": [{"start_offset","end_offset"}] } ] } ] }
//
// Files additionally carry optional "mtime"/"atime"/"ctime"/"btime" (Unix
// nanoseconds). libtusk could not express timestamps, so these are an additive
// extension rather than part of the shape being mirrored — omitted entirely
// when the source filesystem does not record them.
package tskcompat

import (
	"encoding/json"
	"time"

	"indicer/lib/parser/core"
	"indicer/lib/parser/image"
	"indicer/lib/parser/scan"
)

type filesystemJSON struct {
	Type      string `json:"type"`
	BlockSize int    `json:"block_size"`
	Offset    int64  `json:"offset"`
}

type fragmentJSON struct {
	StartOffset int64 `json:"start_offset"`
	EndOffset   int64 `json:"end_offset"`
}

type fileJSON struct {
	Filename     string         `json:"filename"`
	Type         string         `json:"type"`
	IsFragmented bool           `json:"is_fragmented"`
	IsDeleted    bool           `json:"is_deleted"`
	Size         int64          `json:"size"`
	Fragments    []fragmentJSON `json:"fragments"`

	// MACB timestamps as Unix nanoseconds, omitted when the source filesystem
	// does not record them. These are an extension: libtusk's JSON could not
	// carry timestamps, which is why they were being parsed and discarded. The
	// fields are additive, so the shape a legacy consumer expects is unchanged.
	Modified *int64 `json:"mtime,omitempty"`
	Accessed *int64 `json:"atime,omitempty"`
	Changed  *int64 `json:"ctime,omitempty"`
	Born     *int64 `json:"btime,omitempty"`
}

type partitionJSON struct {
	Name        string         `json:"name"`
	StartOffset int64          `json:"start_offset"`
	EndOffset   int64          `json:"end_offset"`
	Filesystem  filesystemJSON `json:"filesystem"`
	Files       []fileJSON     `json:"files"`
}

type resultJSON struct {
	Image      string          `json:"image"`
	Partitions []partitionJSON `json:"partitions"`
}

// Analyze opens the image at path and renders libtusk-compatible JSON: the same
// shape the CGo libtusk path used to emit.
func Analyze(path string) (string, error) {
	img, err := image.Open(path)
	if err != nil {
		return "", err
	}
	defer img.Close()
	return Render(img, path)
}

// Render walks img and renders libtusk-compatible JSON. imagePath is echoed into
// the "image" field.
func Render(img core.Image, imagePath string) (string, error) {
	res := resultJSON{Image: imagePath}
	byIndex := map[int]*partitionJSON{}
	var order []int

	err := scan.WalkImage(img, func(p core.Partition, fsType core.FSType, rec core.FileRecord) error {
		pj, ok := byIndex[p.Index]
		if !ok {
			pj = &partitionJSON{
				Name:        p.Name,
				StartOffset: p.Offset,
				EndOffset:   p.Offset + p.Size - 1,
				Filesystem:  filesystemJSON{Type: string(fsType), Offset: p.Offset},
			}
			byIndex[p.Index] = pj
			order = append(order, p.Index)
		}
		pj.Files = append(pj.Files, toFile(rec))
		return nil
	})
	if err != nil {
		return "", err
	}

	for _, idx := range order {
		res.Partitions = append(res.Partitions, *byIndex[idx])
	}

	b, err := json.Marshal(res)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func toFile(rec core.FileRecord) fileJSON {
	f := fileJSON{
		Filename:     rec.Path,
		Type:         nodeTypeString(rec.Kind),
		IsFragmented: rec.IsFragmented,
		IsDeleted:    rec.IsDeleted,
		Size:         rec.Size,
		Modified:     unixNanos(rec.MACB.Modified),
		Accessed:     unixNanos(rec.MACB.Accessed),
		Changed:      unixNanos(rec.MACB.Changed),
		Born:         unixNanos(rec.MACB.Born),
	}
	for _, r := range rec.Fragments {
		if r.Sparse {
			// libtusk fragments are concrete image byte ranges; a sparse hole has
			// no location, so it is omitted (the file is flagged fragmented).
			continue
		}
		f.Fragments = append(f.Fragments, fragmentJSON{StartOffset: r.Start, EndOffset: r.End})
	}
	return f
}

// unixNanos renders a timestamp for the wire, preserving "not carried" as nil.
// A zero time is treated as absent: fsutil.TimePtr already uses nil for that,
// and a filesystem reporting the epoch is reporting no information.
func unixNanos(t *time.Time) *int64 {
	if t == nil || t.IsZero() {
		return nil
	}
	n := t.UnixNano()
	return &n
}

func nodeTypeString(k core.NodeKind) string {
	switch k {
	case core.KindDir:
		return "directory"
	case core.KindFile:
		return "file"
	default:
		return "other"
	}
}
