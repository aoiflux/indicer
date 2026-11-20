package compression_product

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	coreerr "indicer/internal/core/errors"
	"indicer/internal/duesarc"
)

// inputItem is one file or directory to archive.
type inputItem struct {
	abs   string
	rel   string // slash-separated path inside the archive
	dir   bool
	size  int64
	mtime time.Time
}

// archiveInputs is what an archive is made of.
type archiveInputs struct {
	items    []inputItem
	skipped  []string // the first maxReported, as "path (kind)"
	nskipped int
	isDir    bool
	display  string   // the input_path the result reports
	paths    []string // every input given, absolute
	bytes    int64
	files    int
	dirs     int
}

func (in *archiveInputs) add(it inputItem) {
	in.items = append(in.items, it)
	if it.dir {
		in.dirs++
		return
	}
	in.files++
	in.bytes += it.size
}

func (in *archiveInputs) skip(rel, kind string) {
	in.nskipped++
	if len(in.skipped) < maxReported {
		in.skipped = append(in.skipped, rel+" ("+kind+")")
	}
}

// collectInputs lists what req archives. A folder is walked without
// following links: its regular files and directories are archived, and its
// links and special files are skipped and reported. Files given one by one
// keep their paths relative to the folder they share.
func collectInputs(ctx context.Context, req createArchiveReq) (*archiveInputs, *coreerr.Error) {
	if len(req.InputPaths) > 0 {
		return collectFiles(ctx, req.InputPaths)
	}
	abs, err := filepath.Abs(req.InputPath)
	if err != nil {
		return nil, coreerr.NewWithDetails(coreerr.CodeInvalidRequest, errInvalidInputPath, map[string]string{detailCause: err.Error()})
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, coreerr.NewWithDetails(coreerr.CodeInvalidRequest, errInputPathStatFailed, map[string]string{detailCause: err.Error(), detailInputPath: abs})
	}
	in := &archiveInputs{display: abs, paths: []string{abs}}
	if !info.IsDir() {
		if !info.Mode().IsRegular() {
			return nil, coreerr.NewWithDetails(coreerr.CodeInvalidRequest, "input_path must be a regular file or a folder", map[string]string{detailInputPath: abs})
		}
		in.add(inputItem{abs: abs, rel: filepath.Base(abs), size: info.Size(), mtime: info.ModTime()})
		return in, nil
	}
	in.isDir = true
	err = filepath.WalkDir(abs, func(p string, d fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if p == abs {
			return nil
		}
		rel, err := filepath.Rel(abs, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		switch t := d.Type(); {
		case t.IsDir():
			info, err := d.Info()
			if err != nil {
				return err
			}
			in.add(inputItem{abs: p, rel: rel, dir: true, mtime: info.ModTime()})
		case t.IsRegular():
			info, err := d.Info()
			if err != nil {
				return err
			}
			in.add(inputItem{abs: p, rel: rel, size: info.Size(), mtime: info.ModTime()})
		default:
			in.skip(rel, entryKind(t))
		}
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, canceledError(ctx)
		}
		return nil, coreerr.NewWithDetails(coreerr.CodeInvalidRequest, "input folder cannot be read", map[string]string{detailCause: err.Error(), detailInputPath: abs})
	}
	return in, nil
}

// collectFiles lists files given one by one. Each is archived under its path
// relative to the deepest folder they share; files on different volumes go
// under input_NNN/.
func collectFiles(ctx context.Context, paths []string) (*archiveInputs, *coreerr.Error) {
	in := &archiveInputs{}
	seen := map[string]bool{}
	var infos []os.FileInfo
	for _, p := range paths {
		if ctx.Err() != nil {
			return nil, canceledError(ctx)
		}
		if strings.TrimSpace(p) == "" {
			continue
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, coreerr.NewWithDetails(coreerr.CodeInvalidRequest, errInvalidInputPath, map[string]string{detailCause: err.Error()})
		}
		if seen[abs] {
			continue
		}
		info, err := os.Stat(abs)
		if err != nil {
			return nil, coreerr.NewWithDetails(coreerr.CodeInvalidRequest, errInputPathStatFailed, map[string]string{detailCause: err.Error(), detailInputPath: abs})
		}
		if info.IsDir() {
			return nil, coreerr.NewWithDetails(coreerr.CodeInvalidRequest, errInputPathsFilesOnly, map[string]string{detailInputPath: abs})
		}
		if !info.Mode().IsRegular() {
			return nil, coreerr.NewWithDetails(coreerr.CodeInvalidRequest, "input_paths only accepts regular files", map[string]string{detailInputPath: abs})
		}
		seen[abs] = true
		in.paths = append(in.paths, abs)
		infos = append(infos, info)
	}
	if len(in.paths) == 0 {
		return nil, coreerr.New(coreerr.CodeInvalidRequest, errInputPathRequired)
	}
	common := commonDirPath(in.paths)
	in.display = common
	used := map[string]bool{}
	for i, abs := range in.paths {
		rel, err := filepath.Rel(common, abs)
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
			rel = filepath.Join(fmt.Sprintf("input_%03d", i+1), filepath.Base(abs))
		}
		rel = filepath.ToSlash(rel)
		if used[duesarc.FoldName(rel)] {
			rel = fmt.Sprintf("input_%03d/%s", i+1, filepath.Base(abs))
		}
		used[duesarc.FoldName(rel)] = true
		in.add(inputItem{abs: abs, rel: rel, size: infos[i].Size(), mtime: infos[i].ModTime()})
	}
	return in, nil
}

// commonDirPath is the deepest directory holding every path, or "" when
// they share none (different volumes).
func commonDirPath(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	root := filepath.Dir(paths[0])
	for _, p := range paths[1:] {
		next := filepath.Dir(p)
		for {
			rel, err := filepath.Rel(root, next)
			if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
				break
			}
			parent := filepath.Dir(root)
			if parent == root {
				return ""
			}
			root = parent
		}
	}
	return root
}
