package compression_product

import (
	"archive/zip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/dgraph-io/badger/v4"

	"indicer/cli"
	"indicer/internal/duesarc"
	"indicer/lib/blobcodec"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/dbopen"
	"indicer/lib/fsx"
	"indicer/lib/ingest"
	"indicer/lib/store"
	"indicer/lib/structs"
	"indicer/lib/util"
)

const (
	manifestFileName       = "manifest.json"
	manifestVersionV1      = "v1"
	manifestVersionV2      = "v2"
	manifestPipelineStore  = "store"
	manifestDbRelativePath = "db"

	entryTypeFile = "file"
	entryTypeDir  = "dir"

	stagedArchiveDir = "archive"

	// archiveManifestKey holds an encrypted archive's full manifest inside
	// its database, since manifest.json is readable without the password.
	// When present it is the copy extraction follows.
	archiveManifestKey = "M|||:qwikpak-manifest"
)

// archiveManifest is a .duesarc archive's manifest.json.
//
// Version 2 lists every archived path with the evidence holding its content,
// so files with the same content (stored once, D1), empty files and empty
// directories all come back, in their tree. It records no absolute path of
// the machine that made it. Version 1 recorded only the input path, and
// extraction derived each file's place from its evidence.
//
// An encrypted archive keeps its full manifest in its database, and its
// manifest.json lists no entries and no input size.
type archiveManifest struct {
	Version      string `json:"version"`
	CreatedAtUTC string `json:"created_at_utc"`
	Pipeline     string `json:"pipeline"`
	DbPath       string `json:"db_path"`
	ChunkSizeKB  int    `json:"chunk_size_kb"`
	InputIsDir   bool   `json:"input_is_dir"`
	InputBytes   int64  `json:"input_bytes"`
	// Version 1 only.
	InputPath  string   `json:"input_path,omitempty"`
	InputPaths []string `json:"input_paths,omitempty"`
	// Version 2.
	Entries []manifestEntry `json:"entries,omitempty"`
	// Encrypted is set when the database is encrypted; the entries are then
	// only in the database (archiveManifestKey).
	Encrypted bool `json:"encrypted,omitempty"`
}

// manifestEntry is one archived file or directory.
type manifestEntry struct {
	Path     string `json:"path"` // slash-separated, relative to the output
	Type     string `json:"type"` // "file" or "dir"
	Evidence string `json:"evidence,omitempty"`
	Size     int64  `json:"size"`
	ModTime  string `json:"mtime,omitempty"` // RFC 3339, UTC
}

func (e manifestEntry) mtime() time.Time {
	t, _ := time.Parse(time.RFC3339Nano, e.ModTime)
	return t
}

// validate checks a manifest read from an archive.
func (m *archiveManifest) validate() error {
	if m.Version != manifestVersionV1 && m.Version != manifestVersionV2 {
		return &ArchiveError{Msg: fmt.Sprintf("unsupported manifest version %q", m.Version)}
	}
	if m.Pipeline != "" && m.Pipeline != manifestPipelineStore {
		return &ArchiveError{Msg: fmt.Sprintf("unsupported manifest pipeline %q", m.Pipeline)}
	}
	if local, err := duesarc.LocalName(m.DbPath); err != nil || local == "" {
		return &ArchiveError{Msg: errExtractDbPathMissing}
	}
	for _, e := range m.Entries {
		switch {
		case e.Type == entryTypeDir:
		case e.Type != entryTypeFile:
			return &ArchiveError{Msg: fmt.Sprintf("manifest entry %q has unknown type %q", e.Path, e.Type)}
		case e.Size < 0:
			return &ArchiveError{Msg: fmt.Sprintf("manifest entry %q has a negative size", e.Path)}
		case e.Evidence == "":
			return &ArchiveError{Msg: fmt.Sprintf("manifest entry %q names no content", e.Path)}
		}
	}
	return nil
}

// errDatabasePackage refuses a database package (manifest version 3, kind
// database), which dues package reads: it holds a database, not files.
const errDatabasePackage = "this archive is a DUES database package, not an archive of files; use `dues package inspect`, `dues package verify` or `dues package import`"

func parseManifest(data []byte) (archiveManifest, error) {
	var hdr duesarc.Header
	if err := json.Unmarshal(data, &hdr); err == nil && hdr.IsDatabase() {
		return archiveManifest{}, &ArchiveError{Msg: errDatabasePackage}
	}
	var m archiveManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return archiveManifest{}, &ArchiveError{Msg: errExtractManifestReadFailed + ": " + err.Error()}
	}
	return m, m.validate()
}

func readArchiveManifest(path string) (archiveManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return archiveManifest{}, &ArchiveError{Msg: errManifestMissing}
	}
	return parseManifest(data)
}

// zipManifest reads a .duesarc zip's manifest without extracting anything.
// found is false when the zip has no manifest; a manifest naming a database
// the zip does not hold is an error.
func zipManifest(zr *zip.Reader) (m archiveManifest, found bool, err error) {
	for _, f := range zr.File {
		if f.Name != manifestFileName {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return m, true, &duesarc.DamagedError{Err: err}
		}
		data, err := io.ReadAll(io.LimitReader(duesarc.DamageReader{R: rc}, 64<<20))
		_ = rc.Close()
		if err != nil {
			return m, true, err
		}
		m, err = parseManifest(data)
		if err != nil {
			return m, true, err
		}
		prefix := strings.TrimSuffix(m.DbPath, "/") + "/"
		for _, g := range zr.File {
			if strings.HasPrefix(g.Name, prefix) {
				return m, true, nil
			}
		}
		return m, true, &ArchiveError{Msg: errExtractDbPathMissing}
	}
	return m, false, nil
}

// createDuesarc stores every input file in a new database in a work
// directory, writes the manifest, and packs both into the archive.
func createDuesarc(ctx context.Context, tmp *tempRoot, req createArchiveReq, in *archiveInputs, outputPath string, partSize int64, chunkKiB int, report progressReporter) (files []string, workPath string, err error) {
	var wd *workDir
	if req.WorkDir != "" {
		wd, err = openWorkDir(req.WorkDir)
	} else {
		wd, err = tmp.newWorkDir(tempWorkDirPattern)
	}
	if err != nil {
		return nil, "", err
	}
	defer func() { wd.release(wd.created && !req.KeepWorkDir) }()
	if !wd.created || req.KeepWorkDir {
		workPath = wd.path
	}

	entries := make([]manifestEntry, 0, len(in.items))
	m := archiveManifest{
		Version:     manifestVersionV2,
		Pipeline:    manifestPipelineStore,
		DbPath:      manifestDbRelativePath,
		ChunkSizeKB: chunkKiB,
		InputIsDir:  in.isDir,
		InputBytes:  in.bytes,
	}
	dbPath := filepath.Join(wd.path, manifestDbRelativePath)
	err = withStoreRuntime(ctx, func() (err error) {
		h, err := cli.Common(ctx, archiveDBOptions(dbPath, req.Password, chunkKiB), dbopen.ReadWrite, false)
		if err != nil {
			return err
		}
		defer func() {
			if cerr := h.Close(); err == nil {
				err = cerr
			}
		}()
		if err := util.EnsureBlobPath(h.Root); err != nil {
			return err
		}
		var done int64
		for _, it := range in.items {
			if err := ctx.Err(); err != nil {
				return err
			}
			e := manifestEntry{Path: it.rel, Type: entryTypeDir, ModTime: it.mtime.UTC().Format(time.RFC3339Nano)}
			if !it.dir {
				// Files are archived as their own bytes, not decoded (an E01
				// must come back as the E01), and an archive needs no index.
				// A file whose content is already stored becomes an alias of
				// it (D1); the manifest still lists its path.
				res, err := ingest.Run(ctx, h.DB, ingest.Request{
					Path: it.abs, Name: path.Base(it.rel), SourcePath: it.rel,
					Decode: false, NoIndex: true, Quiet: true,
				})
				if err != nil {
					return fmt.Errorf("%s: %w", it.rel, err)
				}
				e.Type = entryTypeFile
				e.Evidence = base64.StdEncoding.EncodeToString(res.EvidenceID)
				e.Size = res.Size
				done += it.size
				emitProgress(report, "ingesting", progressFromBytes(done, in.bytes, 35, 70), "ingesting input into dedup store")
			}
			entries = append(entries, e)
		}
		emitProgress(report, "writing_manifest", 72, "writing archive manifest")
		m.CreatedAtUTC = time.Now().UTC().Format(time.RFC3339)
		m.Entries = entries
		m.Encrypted = h.Manifest.Encrypted()
		if m.Encrypted {
			data, err := json.Marshal(m)
			if err != nil {
				return err
			}
			if err := h.DB.Update(func(txn *badger.Txn) error { return txn.Set([]byte(archiveManifestKey), data) }); err != nil {
				return err
			}
		}
		_, _ = dbio.CompactValueLog(h.DB)
		return nil
	})
	if err != nil {
		return nil, workPath, err
	}

	public := m
	if m.Encrypted {
		public.Entries, public.InputBytes, public.InputIsDir = nil, 0, false
	}
	data, err := json.MarshalIndent(public, "", "  ")
	if err != nil {
		return nil, workPath, err
	}
	if err := fsx.WriteFileAtomic(filepath.Join(wd.path, manifestFileName), data, defaultFilePerm); err != nil {
		return nil, workPath, err
	}

	emitProgress(report, "packaging", 82, "packaging archive")
	files, err = duesarc.WriteOutput(ctx, outputPath, partSize, func(w io.Writer) error {
		return writeDuesarcZip(ctx, w, wd.path, func(processed, total int64) {
			emitProgress(report, "packaging", progressFromBytes(processed, total, 82, 97), "packaging archive")
		})
	})
	return files, workPath, err
}

// writeDuesarcZip packs a work directory's manifest and database: the
// manifest first, so a reader finds it at once.
func writeDuesarcZip(ctx context.Context, w io.Writer, workPath string, onBytes func(processed, total int64)) error {
	var items []inputItem
	var total int64
	if info, err := os.Stat(filepath.Join(workPath, manifestFileName)); err == nil {
		items = append(items, inputItem{abs: filepath.Join(workPath, manifestFileName), rel: manifestFileName, size: info.Size(), mtime: info.ModTime()})
		total += info.Size()
	}
	dbRoot := filepath.Join(workPath, manifestDbRelativePath)
	err := filepath.WalkDir(dbRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(workPath, p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		it := inputItem{abs: p, rel: filepath.ToSlash(rel), dir: d.IsDir(), mtime: info.ModTime()}
		if !d.IsDir() {
			if !d.Type().IsRegular() {
				return fmt.Errorf("unexpected %s in the archive database: %s", entryKind(d.Type()), rel)
			}
			it.size = info.Size()
			total += it.size
		}
		items = append(items, it)
		return nil
	})
	if err != nil {
		return err
	}
	var written int64
	return writeZipItems(ctx, w, items, func(n int64) {
		written += n
		if onBytes != nil {
			onBytes(written, total)
		}
	})
}

// stagedArchive is a .duesarc archive's manifest and database extracted
// into a locked work directory.
type stagedArchive struct {
	wd       *workDir
	dir      string
	manifest archiveManifest
}

func (s *stagedArchive) release() { s.wd.release(true) }

// stageDuesarc extracts a .duesarc archive's manifest and database, with the
// same checks and limits as any extraction, after checking a split set's
// parts against their sidecar.
func stageDuesarc(ctx context.Context, tmp *tempRoot, archivePath string, limits Limits, onBytes func(processed, total int64)) (*stagedArchive, error) {
	limits, lerr := limits.resolve()
	if lerr != nil {
		return nil, lerr
	}
	za, err := openZip(archivePath, true)
	if err != nil {
		return nil, err
	}
	defer za.Close()
	if _, found, err := zipManifest(za.Reader); err != nil {
		return nil, err
	} else if !found {
		return nil, &ArchiveError{Msg: errManifestMissing}
	}
	wd, err := tmp.newWorkDir(tempExtractPattern)
	if err != nil {
		return nil, err
	}
	s := &stagedArchive{wd: wd, dir: filepath.Join(wd.path, stagedArchiveDir)}
	x, err := newExtraction(s.dir, limits, conflictError, nil)
	if err != nil {
		s.release()
		return nil, err
	}
	if err := extractZip(ctx, za.Reader, za.In.Size(), x, onBytes); err != nil {
		x.abort()
		s.release()
		return nil, err
	}
	if _, err := x.commit(); err != nil {
		s.release()
		return nil, err
	}
	if s.manifest, err = readArchiveManifest(filepath.Join(s.dir, manifestFileName)); err != nil {
		s.release()
		return nil, err
	}
	return s, nil
}

// openDB opens the staged database read-only. The caller holds the engine.
func (s *stagedArchive) openDB(ctx context.Context, password string) (*dbopen.Handle, error) {
	local, _ := duesarc.LocalName(s.manifest.DbPath)
	dbPath := filepath.Join(s.dir, local)
	if info, err := os.Stat(dbPath); err != nil || !info.IsDir() {
		return nil, &ArchiveError{Msg: errExtractDbPathMissing}
	}
	chunk := s.manifest.ChunkSizeKB
	if chunk <= 0 {
		chunk = defaultChunkSizeKB
	}
	h, err := cli.Common(ctx, archiveDBOptions(dbPath, password, chunk), dbopen.ReadOnly, false)
	if err != nil {
		return nil, err
	}
	m, err := databaseManifest(h.DB, s.manifest)
	if err != nil {
		_ = h.Close()
		return nil, err
	}
	s.manifest = m
	return h, nil
}

// databaseManifest returns the manifest an archive's database holds, which
// wins over manifest.json, or manifest.json itself for an archive whose
// database holds none (unencrypted, or version 1). An archive manifest.json
// calls encrypted must have the database copy.
func databaseManifest(db *badger.DB, plain archiveManifest) (archiveManifest, error) {
	var data []byte
	err := db.View(func(txn *badger.Txn) error {
		item, err := txn.Get([]byte(archiveManifestKey))
		if err != nil {
			return err
		}
		data, err = item.ValueCopy(nil)
		return err
	})
	switch {
	case errors.Is(err, badger.ErrKeyNotFound):
		if plain.Encrypted {
			return archiveManifest{}, &ArchiveError{Msg: "the archive's database holds no manifest"}
		}
		return plain, nil
	case err != nil:
		return archiveManifest{}, err
	}
	m, err := parseManifest(data)
	if err != nil {
		return archiveManifest{}, err
	}
	if m.Version != manifestVersionV2 || m.DbPath != plain.DbPath {
		return archiveManifest{}, &ArchiveError{Msg: "the archive's manifest does not match its database"}
	}
	return m, nil
}

// planItem is one path an extraction writes.
type planItem struct {
	path     string // slash-separated
	dir      bool
	evidence string // base64 raw evidence ID
	size     int64
	mtime    time.Time
}

// planV2 lists a version 2 archive's entries, checking that the database
// holds every one's content at its size.
func planV2(db *badger.DB, m archiveManifest) ([]planItem, error) {
	checked := map[string]structs.EvidenceFile{}
	plan := make([]planItem, 0, len(m.Entries))
	for _, e := range m.Entries {
		it := planItem{path: e.Path, dir: e.Type == entryTypeDir, evidence: e.Evidence, size: e.Size, mtime: e.mtime()}
		if !it.dir {
			ev, ok := checked[e.Evidence]
			if !ok {
				var err error
				if ev, err = storedEvidence(db, e.Evidence); err != nil {
					return nil, &ArchiveError{Msg: "archive content is missing", Problems: []entryProblem{{name: e.Path, reason: err.Error()}}, Count: 1}
				}
				checked[e.Evidence] = ev
			}
			if ev.Size != e.Size {
				return nil, &ArchiveError{Msg: "archive content does not match its manifest", Problems: []entryProblem{{name: e.Path, reason: fmt.Sprintf("the manifest says %d bytes, the database holds %d", e.Size, ev.Size)}}, Count: 1}
			}
		}
		plan = append(plan, it)
	}
	return plan, nil
}

// storedEvidence returns the completed evidence id (base64) names.
func storedEvidence(db *badger.DB, id string) (structs.EvidenceFile, error) {
	raw, err := base64.StdEncoding.DecodeString(id)
	if err != nil {
		return structs.EvidenceFile{}, errors.New("its content ID is not valid")
	}
	ev, err := dbio.GetEvidenceFile(raw, db)
	if errors.Is(err, badger.ErrKeyNotFound) {
		return ev, errors.New("its content is not in the archive's database")
	}
	if err != nil {
		return ev, err
	}
	if !ev.Completed || ev.Failed {
		return ev, errors.New("its content was not completely stored")
	}
	return ev, nil
}

// planV1 lists what a version 1 archive restores: every completed evidence
// and, for archives made since duplicates became aliases, every alias. Each
// is placed by the source path it was stored from, relative to the input
// the manifest names, else by its name. Two that would share a path get
// distinct names, built from the evidence ID.
func planV1(ctx context.Context, db *badger.DB, m archiveManifest) ([]planItem, error) {
	keys, err := listEvidenceKeys(ctx, db)
	if err != nil {
		return nil, err
	}
	used := map[string]bool{}
	var plan []planItem
	for _, key := range keys {
		ev, err := dbio.GetEvidenceFile(key, db)
		if err != nil {
			return nil, err
		}
		if !ev.Completed || ev.Failed {
			continue
		}
		raw := dbio.EvidenceRawID(key)
		id := base64.StdEncoding.EncodeToString(raw)
		sources := []string{recordedSource(ev.Source, ev.Name)}
		aliases, err := dbio.Aliases(db, raw)
		if err != nil {
			return nil, err
		}
		for _, a := range aliases {
			sources = append(sources, recordedSource(&a.Source, a.Path))
		}
		for _, src := range sources {
			rel := uniquePath(v1RelativePath(m, src, safeFallbackName(id)), id, used)
			plan = append(plan, planItem{path: rel, evidence: id, size: ev.Size})
		}
	}
	return plan, nil
}

// recordedSource is the path an evidence or alias was stored from.
func recordedSource(src *structs.EvidenceSource, fallback string) string {
	if src != nil && len(src.Members) > 0 && src.Members[0].Path != "" {
		return src.Members[0].Path
	}
	return fallback
}

// v1RelativePath is where a version 1 archive's file goes, as a
// slash-separated path under the output.
func v1RelativePath(m archiveManifest, original, fallback string) string {
	if fallback == "" {
		fallback = "restored"
	}
	clean := filepath.Clean(original)
	if clean == "." || clean == "" {
		clean = fallback
	}
	var rel string
	if m.InputIsDir && m.InputPath != "" {
		r, err := filepath.Rel(filepath.Clean(m.InputPath), clean)
		if err == nil && r != "." && !filepath.IsAbs(r) && !strings.HasPrefix(r, "..") {
			rel = r
		} else {
			rel = baseName(clean)
		}
	} else {
		rel = baseName(m.InputPath)
		if rel == "" || rel == "." || rel == "/" || rel == `\` {
			rel = baseName(clean)
		}
	}
	if vol := filepath.VolumeName(rel); vol != "" {
		rel = strings.TrimPrefix(rel, vol)
	}
	rel = strings.TrimLeft(filepath.ToSlash(filepath.Clean(rel)), "/")
	if rel == "" || rel == "." {
		rel = fallback
	}
	return rel
}

// baseName is the last element of a path written on either Windows or Unix.
func baseName(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

// uniquePath returns rel, or, when another file already has that path,
// "stem~ID.ext" built from the evidence ID.
func uniquePath(rel, id string, used map[string]bool) string {
	if !used[duesarc.FoldName(rel)] {
		used[duesarc.FoldName(rel)] = true
		return rel
	}
	ext := path.Ext(rel)
	stem := strings.TrimSuffix(rel, ext)
	tag := safeFallbackName(id)
	if len(tag) > 8 {
		tag = tag[:8]
	}
	cand := stem + "~" + tag + ext
	for n := 2; used[duesarc.FoldName(cand)]; n++ {
		cand = fmt.Sprintf("%s~%s-%d%s", stem, tag, n, ext)
	}
	used[duesarc.FoldName(cand)] = true
	return cand
}

func safeFallbackName(hashB64 string) string {
	fallback := strings.NewReplacer("/", "_", "\\", "_", "+", "-", "=", "").Replace(hashB64)
	if fallback == "" {
		return "restored"
	}
	return fallback
}

// restorePlan stages every planned path: a directory is created, the first
// path of each content is restored from the database with full
// verification, and later paths of the same content are copied from it.
func restorePlan(ctx context.Context, db *badger.DB, plan []planItem, x *extraction, onProgress func(done, total int)) error {
	var declared int64
	for _, it := range plan {
		if it.size > x.limits.MaxEntryBytes {
			return &LimitError{Limit: "max_entry_bytes", Value: x.limits.MaxEntryBytes, What: "entry " + it.path}
		}
		declared += it.size
		if declared > x.limits.MaxTotalBytes {
			break
		}
	}
	// The sum counts every path, so content stored once but listed many
	// times (a deduplication bomb) is refused here (CMP-1).
	if err := x.reserve(declared); err != nil {
		return err
	}
	first := map[string]string{} // evidence -> its staged local path
	for i, it := range plan {
		if err := ctx.Err(); err != nil {
			return err
		}
		var err error
		switch src, ok := first[it.evidence]; {
		case it.dir:
			err = x.addDir(it.path, it.mtime)
		case ok:
			err = x.addFile(it.path, it.size, it.mtime, func(w io.Writer) error {
				f, err := x.stage.Open(src)
				if err != nil {
					return err
				}
				defer f.Close()
				_, err = duesarc.Copy(ctx, w, f, nil)
				return err
			})
		default:
			wrote := x.nprob == 0
			err = x.addFile(it.path, it.size, it.mtime, func(w io.Writer) error {
				rep, err := store.Restore(ctx, db, it.evidence, w, store.RestoreOptions{Verify: store.VerifyFull})
				if err != nil {
					return restoreDamage(err)
				}
				if rep.Size != it.size {
					return &duesarc.DamagedError{Err: fmt.Errorf("%s restored %d bytes, the archive lists %d", it.path, rep.Size, it.size)}
				}
				return nil
			})
			if err == nil && wrote && x.nprob == 0 {
				first[it.evidence], _ = duesarc.LocalName(it.path)
			}
		}
		if err != nil {
			return err
		}
		if onProgress != nil {
			onProgress(i+1, len(plan))
		}
	}
	return nil
}

// restoreDamage marks a restore failure caused by the archive's stored data
// (a chunk missing, damaged, or not matching its hash) as archive damage.
func restoreDamage(err error) error {
	if err == nil {
		return nil
	}
	for _, sentinel := range []error{blobcodec.ErrCorrupt, store.ErrDigestMismatch, dbio.ErrRelationMissing, dbio.ErrChunkRange, dbio.ErrChunkNotFound, badger.ErrKeyNotFound} {
		if errors.Is(err, sentinel) {
			return &duesarc.DamagedError{Err: err}
		}
	}
	return err
}

// extractDuesarc restores a .duesarc archive into out.
func extractDuesarc(ctx context.Context, tmp *tempRoot, inputPath, out, password, policy string, limits Limits, report progressReporter) (commitResult, error) {
	emitProgress(report, "extracting", 20, "extracting archive entries")
	staged, err := stageDuesarc(ctx, tmp, inputPath, limits, func(processed, total int64) {
		emitProgress(report, "extracting", progressFromBytes(processed, total, 20, 60), "extracting archive entries")
	})
	if err != nil {
		return commitResult{}, err
	}
	defer staged.release()

	var res commitResult
	err = withStoreRuntime(ctx, func() (err error) {
		h, err := staged.openDB(ctx, password)
		if err != nil {
			return err
		}
		defer func() {
			if cerr := h.Close(); err == nil {
				err = cerr
			}
		}()
		var plan []planItem
		if staged.manifest.Version == manifestVersionV2 {
			plan, err = planV2(h.DB, staged.manifest)
		} else {
			plan, err = planV1(ctx, h.DB, staged.manifest)
		}
		if err != nil {
			return err
		}
		x, err := newExtraction(out, limits, policy, nil)
		if err != nil {
			return err
		}
		emitProgress(report, "restoring", 65, "restoring files")
		if err := restorePlan(ctx, h.DB, plan, x, func(done, total int) {
			emitProgress(report, "restoring", progressFromBytes(int64(done), int64(total), 65, 97), fmt.Sprintf("restoring files (%d/%d)", done, total))
		}); err != nil {
			x.abort()
			return err
		}
		res, err = x.commit()
		return err
	})
	return res, err
}

// listDuesarc lists a .duesarc archive's logical files.
func listDuesarc(ctx context.Context, tmp *tempRoot, inputPath, password string, report progressReporter) (archiveManifest, []archiveLogicalFile, error) {
	staged, err := stageDuesarc(ctx, tmp, inputPath, Limits{}, func(processed, total int64) {
		emitProgress(report, "extracting", progressFromBytes(processed, total, 55, 85), "extracting manifest and metadata")
	})
	if err != nil {
		return archiveManifest{}, nil, err
	}
	defer staged.release()
	var files []archiveLogicalFile
	err = withStoreRuntime(ctx, func() (err error) {
		h, err := staged.openDB(ctx, password)
		if err != nil {
			return err
		}
		defer func() {
			if cerr := h.Close(); err == nil {
				err = cerr
			}
		}()
		files, err = logicalFiles(ctx, h.DB, staged.manifest)
		return err
	})
	return staged.manifest, files, err
}

// logicalFiles describes each archived file: its path, size, content ID and
// the bytes its content takes in the archive.
func logicalFiles(ctx context.Context, db *badger.DB, m archiveManifest) ([]archiveLogicalFile, error) {
	var plan []planItem
	var err error
	if m.Version == manifestVersionV2 {
		plan, err = planV2(db, m)
	} else {
		plan, err = planV1(ctx, db, m)
	}
	if err != nil {
		return nil, err
	}
	compressed := map[string]int64{}
	out := make([]archiveLogicalFile, 0, len(plan))
	for _, it := range plan {
		if it.dir {
			continue
		}
		raw, _ := base64.StdEncoding.DecodeString(it.evidence)
		ev, err := dbio.GetEvidenceFile(raw, db)
		if err != nil {
			return nil, err
		}
		size, ok := compressed[it.evidence]
		if !ok {
			if size, err = storedSize(ctx, db, raw, ev); err != nil {
				return nil, err
			}
			compressed[it.evidence] = size
		}
		status := "pending"
		switch {
		case ev.Failed:
			status = "failed"
		case ev.Completed:
			status = "completed"
		}
		hash := ev.FileHash
		if hash == "" {
			hash = it.evidence
		}
		out = append(out, archiveLogicalFile{
			ID: it.evidence, Hash: hash, Name: path.Base(it.path), Path: it.path,
			Size: it.size, TotalSize: it.size, CompressedSize: size,
			Completed: ev.Completed, Failed: ev.Failed, Status: status,
		})
	}
	return out, nil
}

// storedSize is the bytes an evidence's chunks take in the archive.
func storedSize(ctx context.Context, db *badger.DB, raw []byte, ev structs.EvidenceFile) (int64, error) {
	if ev.Size <= 0 {
		return 0, nil
	}
	chunks, err := dbio.OpenEvidenceChunks(db, raw, dbio.ReadOptions{})
	if err != nil {
		return 0, err
	}
	var total int64
	err = chunks.Spans(ctx, ev.Start, ev.Start+ev.Size, dbio.SpanBatch(chunks.ChunkSize()), func(spans []dbio.ChunkSpan) error {
		for _, s := range spans {
			total += s.Loc.StoredSize
		}
		return nil
	})
	return total, err
}

// verifyDuesarcContent reads back every chunk of every evidence the archive
// restores and checks it against its hash. It returns how many were checked.
func verifyDuesarcContent(ctx context.Context, tmp *tempRoot, inputPath, password string, report progressReporter) (archiveManifest, int, error) {
	staged, err := stageDuesarc(ctx, tmp, inputPath, Limits{}, nil)
	if err != nil {
		return archiveManifest{}, 0, err
	}
	defer staged.release()
	verified := 0
	err = withStoreRuntime(ctx, func() (err error) {
		h, err := staged.openDB(ctx, password)
		if err != nil {
			return err
		}
		defer func() {
			if cerr := h.Close(); err == nil {
				err = cerr
			}
		}()
		var plan []planItem
		if staged.manifest.Version == manifestVersionV2 {
			plan, err = planV2(h.DB, staged.manifest)
		} else {
			plan, err = planV1(ctx, h.DB, staged.manifest)
		}
		if err != nil {
			return err
		}
		pathOf := map[string]string{}
		var order []string
		for _, it := range plan {
			if !it.dir && pathOf[it.evidence] == "" {
				pathOf[it.evidence] = it.path
				order = append(order, it.evidence)
			}
		}
		var problems []entryProblem
		nprob := 0
		for i, id := range order {
			raw, _ := base64.StdEncoding.DecodeString(id)
			ev, err := dbio.GetEvidenceFile(raw, h.DB)
			if err != nil {
				return err
			}
			err = store.ValidateEvidence(ctx, h.DB, raw, ev, store.ValidateContent)
			switch {
			case err == nil:
				verified++
			case errors.Is(err, store.ErrMaterialMissing), errors.Is(err, store.ErrMaterialCorrupt):
				nprob++
				if len(problems) < maxReported {
					problems = append(problems, entryProblem{name: pathOf[id], reason: err.Error()})
				}
			default:
				return err
			}
			emitProgress(report, "verifying_content", progressFromBytes(int64(i+1), int64(len(order)), 60, 97), "verifying stored content")
		}
		if nprob > 0 {
			return &ArchiveError{Msg: "archive content is damaged", Problems: problems, Count: nprob}
		}
		return nil
	})
	return staged.manifest, verified, err
}

func listEvidenceKeys(ctx context.Context, db *badger.DB) ([][]byte, error) {
	var keys [][]byte
	prefix := []byte(cnst.EviFileNamespace)
	err := db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = false
		opts.Prefix = prefix
		it := txn.NewIterator(opts)
		defer it.Close()
		for it.Rewind(); it.Valid(); it.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			keys = append(keys, it.Item().KeyCopy(nil))
		}
		return nil
	})
	return keys, err
}
