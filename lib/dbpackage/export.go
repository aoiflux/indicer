// Package dbpackage packs a DUES data root into a database package, a
// .duesarc whose manifest is version 3, kind database, and reads one back:
// inspect, verify, and import.
//
// A package is a zip. db/ holds the data root's files as they are on disk,
// stored without compression (they are compressed or encrypted already),
// sorted by path, each dated at the package's creation. manifest.json comes
// last: it lists every file with its size and SHA-256, which are computed
// as the file is copied, so the data is read once. Readers find it through
// the zip's central directory.
package dbpackage

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"indicer/internal/diskspace"
	"indicer/internal/duesarc"
	"indicer/internal/lifecycle"
	"indicer/internal/progress"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/dbmeta"
	"indicer/lib/dbopen"
	"indicer/lib/fsx"
	"indicer/lib/graph"
	"indicer/lib/graph/schema"
	"indicer/lib/graph/signer"
	"indicer/lib/structs"

	"github.com/dgraph-io/badger/v4"
)

// Ext is a package's file extension.
const Ext = ".duesarc"

// ExportOptions say what to export and where.
type ExportOptions struct {
	// Path is the data root, as for dbopen.Request.
	Path string
	// Creds, when set, are checked against an encrypted database. Without
	// them the package is made all the same: it copies the encrypted files.
	Creds dbmeta.Credentials
	// TempDir is --temp-dir, for a read-only open of write-protected media.
	TempDir string
	// Tuning is how the storage engine runs on the database it opens; nil
	// takes cnst.DefaultTuning.
	Tuning *cnst.Tuning
	// Output is the package's path. Empty means DefaultName in the current
	// directory.
	Output string
	// PartSize, when above 0, splits the package into parts of that many
	// bytes, described by a sidecar.
	PartSize int64
	// Force replaces a package already at Output.
	Force bool
	// WithoutDerived leaves out the full-text index and the graph, which
	// reindex, enrich and micro extract rebuild.
	WithoutDerived bool
	// HostInfo records the machine's OS, architecture and name.
	HostInfo bool
	// Creator is the program making the package.
	Creator duesarc.Creator
}

// ExportResult is what an export made.
type ExportResult struct {
	Root     string
	Output   string
	Files    []string // what was written: the package, or its parts
	Manifest duesarc.DatabaseManifest
	// Bytes is the size of what was written, all parts together.
	Bytes int64
	// Notices are one-line messages for the user.
	Notices []string
	// Warnings say what the user may not expect, as the plaintext parts of
	// an encrypted database.
	Warnings []string
}

// componentNames name the parts of a data root that are never encrypted.
var componentNames = map[string]string{cnst.FTSDIR: "full-text index", cnst.GRAPHDIR: "graph"}

// DefaultName is the package name for a data root, made at t:
// dues-<root>-<UTC time>.duesarc.
func DefaultName(root string, t time.Time) string {
	base := strings.TrimLeft(filepath.Base(root), ".")
	if base == "" || base == string(filepath.Separator) {
		base = "db"
	}
	return fmt.Sprintf("dues-%s-%s%s", base, t.UTC().Format("20060102T150405Z"), Ext)
}

// entry is one directory or file a package holds.
type entry struct {
	rel  string // slash-separated, relative to the data root
	abs  string
	dir  bool
	size int64
	mode fs.FileMode
}

// Export packs a data root. The root is held (dbopen.TakeSnapshot) from
// before its files are listed until the package is written, so no DUES
// writer changes it meanwhile; readers may run.
func Export(ctx context.Context, opts ExportOptions) (res ExportResult, err error) {
	snap, err := dbopen.TakeSnapshot(ctx, opts.Path)
	if err != nil {
		return res, err
	}
	defer func() { err = errors.Join(err, snap.Release()) }()
	res.Root = snap.Root
	res.Notices = append(res.Notices, snap.Notices...)

	created := time.Now().UTC().Truncate(time.Second)
	out := opts.Output
	if out == "" {
		out = DefaultName(snap.Root, created)
	}
	if out, err = filepath.Abs(out); err != nil {
		return res, err
	}
	res.Output = out
	if inside(snap.Root, out) {
		return res, lifecycle.Usagef("the package %s would be inside the database it copies, %s", out, snap.Root)
	}
	if !opts.Force {
		for _, p := range []string{out, out + duesarc.SidecarExt, duesarc.PartPath(out, 1)} {
			if _, err := os.Lstat(p); err == nil {
				return res, lifecycle.Usagef("%s already exists; pass --force to replace it", p)
			}
		}
	}

	m := duesarc.DatabaseManifest{
		Version:      duesarc.VersionDatabase,
		Kind:         duesarc.KindDatabase,
		Pipeline:     duesarc.PipelineExport,
		CreatedAtUTC: created.Format(time.RFC3339),
		DbPath:       duesarc.DatabaseDir,
		CreatedBy:    opts.Creator,
		Database:     databaseInfo(snap),
	}
	if opts.HostInfo {
		host, _ := os.Hostname()
		m.Host = &duesarc.Host{OS: runtime.GOOS, Arch: runtime.GOARCH, Hostname: host}
	}

	entries, err := listRoot(ctx, snap.Root, opts.WithoutDerived)
	if err != nil {
		return res, err
	}
	derived := derivedIn(entries)
	if n, ok := graphSchema(snap.Root); ok && derived[cnst.GRAPHDIR] {
		m.Database.Components = map[string]int{"graph_schema": n}
	}
	if m.Database.Encrypted {
		for _, d := range []string{cnst.FTSDIR, cnst.GRAPHDIR} {
			if derived[d] {
				m.Database.PlaintextComponents = append(m.Database.PlaintextComponents, d)
			}
		}
		if n := len(m.Database.PlaintextComponents); n > 0 {
			names := make([]string, n)
			for i, c := range m.Database.PlaintextComponents {
				names[i] = componentNames[c]
			}
			res.Warnings = append(res.Warnings, fmt.Sprintf("the database is encrypted, but its %s %s not: the package carries %s unencrypted (--without-derived leaves %s out)",
				strings.Join(names, " and "), plural(n, "is", "are"), plural(n, "it", "them"), plural(n, "it", "them")))
		}
	}

	stats, notices, err := checkDatabase(ctx, snap, opts)
	if err != nil {
		return res, err
	}
	res.Notices = append(res.Notices, notices...)
	if !m.Database.Encrypted {
		m.Stats = stats
	}

	var total int64
	for _, e := range entries {
		total += e.size
	}
	if err := checkSpace(out, total, len(entries), opts.PartSize); err != nil {
		return res, err
	}

	ph := progress.Start(ctx, "export", progress.Bytes, total)
	files, err := duesarc.WriteOutput(ctx, out, opts.PartSize, func(w io.Writer) error {
		return writePackage(ctx, w, entries, &m, created, ph)
	})
	ph.Done(err)
	if err != nil {
		return res, err
	}
	res.Files, res.Manifest = files, m
	for _, f := range files {
		if info, err := os.Stat(f); err == nil {
			res.Bytes += info.Size()
		}
	}
	return res, nil
}

// databaseInfo copies what the database's manifest says into the package's.
func databaseInfo(s *dbopen.Snapshot) duesarc.DatabaseInfo {
	l := s.Manifest.Layout
	return duesarc.DatabaseInfo{
		Format:        s.Manifest.Format,
		FormatVersion: s.Manifest.Version,
		Layout: duesarc.Layout{
			ChunkKiB: l.ChunkKiB, ChunkHash: l.ChunkHash, Storage: l.Storage,
			BlobEncoding: l.BlobEncoding, ValueEncoding: l.ValueEncoding, BadgerCompression: l.BadgerCompression,
		},
		Encryption: s.Manifest.Encryption.Scheme,
		Encrypted:  s.Manifest.Encrypted(),
	}
}

// checkDatabase opens the database read-only, beside the snapshot, when it
// can be: to check the password of an encrypted one, and to count an
// unencrypted one's evidence. An encrypted database without a password is
// not opened.
func checkDatabase(ctx context.Context, s *dbopen.Snapshot, opts ExportOptions) (*duesarc.Stats, []string, error) {
	if s.Manifest.Encrypted() && !opts.Creds.IsSet() {
		return nil, []string{fmt.Sprintf("database %s is encrypted and no password was given: the package copies its encrypted files, and the password was not checked", s.Root)}, nil
	}
	h, err := dbopen.Open(ctx, dbopen.Request{Path: s.Root, Mode: dbopen.ReadOnly, Creds: opts.Creds, TempDir: opts.TempDir, Tuning: opts.Tuning})
	if err != nil {
		return nil, nil, err
	}
	stats, err := evidenceStats(ctx, h.DB)
	return stats, h.Notices, errors.Join(err, h.Close())
}

// evidenceStats counts the completed evidence and their bytes, as stats
// does, reading only the evidence records.
func evidenceStats(ctx context.Context, db *badger.DB) (*duesarc.Stats, error) {
	s := &duesarc.Stats{}
	prefix := []byte(cnst.EviFileNamespace)
	err := db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = prefix
		it := txn.NewIterator(opts)
		defer it.Close()
		for it.Rewind(); it.Valid(); it.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			data, err := it.Item().ValueCopy(nil)
			if err != nil {
				return err
			}
			var ef structs.EvidenceFile
			if err := dbio.DecodeRecord(data, &ef); err != nil {
				return fmt.Errorf("record %q: %w", it.Item().Key(), err)
			}
			if ef.Completed && ef.IngestState != structs.IngestStateDuplicate {
				s.Evidence++
				s.LogicalBytes += ef.Size
			}
		}
		return nil
	})
	return s, err
}

// listRoot lists what a package of root holds, sorted by path: every
// directory and regular file except the locks, the temporary files a killed
// writer left, the uploads a server was receiving, and, withoutDerived, the
// full-text index and the graph.
func listRoot(ctx context.Context, root string, withoutDerived bool) ([]entry, error) {
	var entries []entry
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if skip(rel, d.IsDir(), withoutDerived) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		e := entry{rel: rel, abs: p, dir: d.IsDir(), mode: info.Mode()}
		switch {
		case d.IsDir():
		case info.Mode().IsRegular():
			e.size = info.Size()
		default:
			return fmt.Errorf("the data root holds %s, which is not a regular file or a directory; a package copies only those", p)
		}
		entries = append(entries, e)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].rel < entries[j].rel })
	return entries, nil
}

// skip reports whether a path of the data root stays out of a package.
func skip(rel string, dir, withoutDerived bool) bool {
	top, _, _ := strings.Cut(rel, "/")
	base := rel[strings.LastIndex(rel, "/")+1:]
	switch {
	case rel == cnst.UploadsDir && dir:
		return true
	case withoutDerived && (top == cnst.FTSDIR || top == cnst.GRAPHDIR):
		return true
	case dir:
		return false
	case rel == dbopen.LockName, rel == cnst.KVDBDIR+"/LOCK", fsx.IsTempName(base):
		return true
	case top == cnst.GRAPHDIR && (graph.IsLockFile(base) || graph.IsScratchFile(base)):
		return true
	case top == cnst.KEYSDIR && strings.HasSuffix(base, signer.KeyFileExt):
		// An actor's private key never leaves its data root; the public
		// keyring, which checks what it signed, goes with the package.
		return true
	}
	return false
}

// derivedIn reports which of the full-text index and the graph the package
// holds files of.
func derivedIn(entries []entry) map[string]bool {
	in := map[string]bool{}
	for _, e := range entries {
		if top, _, ok := strings.Cut(e.rel, "/"); ok && !e.dir && (top == cnst.FTSDIR || top == cnst.GRAPHDIR) {
			in[top] = true
		}
	}
	return in
}

// graphSchema is the schema version the graph's marker records.
func graphSchema(root string) (int, bool) {
	data, err := os.ReadFile(filepath.Join(root, cnst.GRAPHDIR, schema.MarkerFileName))
	if err != nil {
		return 0, false
	}
	var marker struct {
		SchemaVersion int `json:"schema_version"`
	}
	if json.Unmarshal(data, &marker) != nil || marker.SchemaVersion <= 0 {
		return 0, false
	}
	return marker.SchemaVersion, true
}

// checkSpace refuses an export the output's volume has no room for. A split
// package is written whole first, then cut into parts, so it needs twice
// the room.
func checkSpace(out string, total int64, entries int, partSize int64) error {
	need := uint64(total) + uint64(entries)*256 + 1<<20
	if partSize > 0 {
		need *= 2
	}
	return diskspace.Check(filepath.Dir(out), need)
}

// writePackage writes the zip: the entries in order, then the manifest,
// which gets their sizes and hashes.
func writePackage(ctx context.Context, w io.Writer, entries []entry, m *duesarc.DatabaseManifest, created time.Time, ph *progress.Phase) error {
	zw := zip.NewWriter(w)
	m.Dirs, m.Files = nil, nil
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := duesarc.DatabaseDir + "/" + e.rel
		if e.dir {
			hdr := &zip.FileHeader{Name: name + "/", Method: zip.Store, Modified: created}
			hdr.SetMode(fs.ModeDir | e.mode.Perm())
			if _, err := zw.CreateHeader(hdr); err != nil {
				return err
			}
			m.Dirs = append(m.Dirs, e.rel)
			continue
		}
		sum, err := copyEntry(ctx, zw, name, e, created, ph)
		if err != nil {
			return err
		}
		m.Files = append(m.Files, duesarc.File{Path: e.rel, Size: e.size, SHA256: sum})
	}
	if len(m.Files) == 0 {
		return errors.New("the data root holds no files to package")
	}
	m.ContentSHA256 = duesarc.ContentHash(m.Files)
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	hdr := &zip.FileHeader{Name: duesarc.ManifestName, Method: zip.Deflate, Modified: created}
	hdr.SetMode(cnst.FilePerm)
	mw, err := zw.CreateHeader(hdr)
	if err != nil {
		return err
	}
	if _, err := mw.Write(append(data, '\n')); err != nil {
		return err
	}
	return zw.Close()
}

// copyEntry copies one file into the zip and returns its SHA-256. A file
// whose size changed since it was listed was written to meanwhile, which
// the snapshot should have made impossible: the export fails.
func copyEntry(ctx context.Context, zw *zip.Writer, name string, e entry, created time.Time, ph *progress.Phase) (string, error) {
	f, err := os.Open(e.abs)
	if err != nil {
		return "", err
	}
	defer f.Close()
	hdr := &zip.FileHeader{Name: name, Method: zip.Store, Modified: created}
	hdr.SetMode(e.mode.Perm())
	zf, err := zw.CreateHeader(hdr)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	n, err := duesarc.Copy(ctx, io.MultiWriter(zf, h), f, ph.Add)
	if err != nil {
		return "", err
	}
	if n != e.size {
		return "", fmt.Errorf("%s changed while it was copied (%d bytes, listed with %d)", e.abs, n, e.size)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// inside reports whether p is root or under it.
func inside(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
