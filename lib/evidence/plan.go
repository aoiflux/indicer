package evidence

import (
	"context"
	"fmt"
	"indicer/internal/progress"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aoiflux/libewf"
)

// PlanItem is one evidence a folder ingest stores: the file to open and the
// other files of its set (EWF or split raw segments), which are not stored on
// their own.
type PlanItem struct {
	Path    string
	Members []string
	// Size is the bytes of the file and of its set's other segments, as the
	// walk found them: what the overall bar weighs the item by.
	Size int64
}

// PlanSkip is a folder entry a folder ingest does not store.
type PlanSkip struct {
	Path   string
	Reason string
}

// PlanError is a folder entry that could not be read.
type PlanError struct {
	Path string
	Err  error
}

// FolderPlan is what a folder ingest will do.
type FolderPlan struct {
	Items   []PlanItem
	Skipped []PlanSkip
	Errors  []PlanError
}

// Plan walks root and groups what it finds into evidence. Only regular files
// are stored; a symlink, FIFO or device is skipped, since opening a FIFO
// blocks and a link may lead outside the folder. The members of an EWF set
// (E01, E02, ...) or a split raw set (001, 002, ...) are claimed by the set's
// first segment, so each set is one evidence. An unreadable directory is
// recorded and the walk goes on (ING-11).
func Plan(ctx context.Context, root string, decode bool) (FolderPlan, error) {
	var plan FolderPlan
	var files []string
	sizes := map[string]int64{}
	seen := progress.CurrentIn(ctx, progress.Files)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if err != nil {
			plan.Errors = append(plan.Errors, PlanError{Path: path, Err: err})
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		switch {
		case d.IsDir():
		case d.Type().IsRegular():
			files = append(files, path)
			sizes[filepath.Clean(path)] = entrySize(d)
			seen.Add(1)
		default:
			plan.Skipped = append(plan.Skipped, PlanSkip{Path: path, Reason: "not a regular file (" + d.Type().String() + ")"})
		}
		return nil
	})
	if err != nil {
		return plan, err
	}
	sort.Strings(files)
	if !decode {
		for _, f := range files {
			plan.Items = append(plan.Items, PlanItem{Path: f, Size: sizes[filepath.Clean(f)]})
		}
		return plan, nil
	}

	claimed := map[string]bool{}
	var heads []PlanItem
	for _, f := range files {
		segs := setMembers(f)
		if len(segs) < 2 {
			continue
		}
		item := PlanItem{Path: f, Size: sizes[filepath.Clean(f)]}
		for _, s := range segs[1:] {
			claimed[filepath.Clean(s)] = true
			item.Members = append(item.Members, s)
			item.Size += sizes[filepath.Clean(s)]
		}
		heads = append(heads, item)
	}
	headOf := map[string]PlanItem{}
	for _, h := range heads {
		headOf[h.Path] = h
	}
	for _, f := range files {
		if claimed[filepath.Clean(f)] {
			continue
		}
		if h, ok := headOf[f]; ok {
			plan.Items = append(plan.Items, h)
			continue
		}
		plan.Items = append(plan.Items, PlanItem{Path: f, Size: sizes[filepath.Clean(f)]})
	}
	return plan, nil
}

// entrySize is the size of the file d names, or 0 when it can no longer be
// read: the size only weighs the item for progress, and storing the item
// reports why it cannot be read.
func entrySize(d fs.DirEntry) int64 {
	fi, err := d.Info()
	if err != nil {
		return 0
	}
	return fi.Size()
}

// setMembers returns every segment of the set whose first segment is path,
// path included, or nil when path does not start a set.
func setMembers(path string) []string {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".e01", ".ex01", ".s01":
		segs, err := libewf.SegmentPaths(path)
		if err != nil {
			return nil
		}
		return segs
	case ".001":
		stem := strings.TrimSuffix(path, filepath.Ext(path))
		var segs []string
		for i := 1; i <= 999; i++ {
			p := fmt.Sprintf("%s.%03d", stem, i)
			if fi, err := os.Stat(p); err != nil || !fi.Mode().IsRegular() {
				break
			}
			segs = append(segs, p)
		}
		return segs
	}
	return nil
}
