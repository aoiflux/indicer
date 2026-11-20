package dbpackage

import (
	"archive/zip"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"indicer/internal/diskspace"
	"indicer/internal/duesarc"
	"indicer/internal/lifecycle"
	"indicer/internal/progress"
	"indicer/internal/workpool"
	"indicer/lib/blobcodec"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/dbmeta"
	"indicer/lib/dbopen"
	"indicer/lib/engine"
	"indicer/lib/fsx"
	"indicer/lib/graph/signer"
	"indicer/lib/ingest"
	"indicer/lib/store"
	"indicer/lib/structs"

	"github.com/dgraph-io/badger/v4"
)

// The flags of dues package merge that its messages name.
const (
	SourcePasswordFileFlag  = "source-password-file"
	SourcePasswordStdinFlag = "source-password-stdin"
	AllowPlaintextFlag      = "allow-plaintext"
	StageDirFlag            = "stage-dir"
)

// A merge stages a package beside its target as
// .<target>.dues-merge-<first stageHashLen hex digits of its content hash>,
// with a lock and a marker naming the whole hash beside it, so that a rerun
// of the same package finds what the last run copied.
const (
	mergeStageMarker   = ".dues-merge-"
	stageHashLen       = 16
	mergeLockSuffix    = ".lock"
	mergePackageSuffix = ".package"
)

// How a merged evidence ended, beside the outcomes store commits:
// "aliased" and "present" are store.CommitAliased and store.CommitPresent.
const (
	MergeMerged  = "merged"
	MergeSkipped = "skipped"
	MergeFailed  = "failed"
)

// MergeTarget is the database a merge writes into, opened for it.
type MergeTarget struct {
	Handle  *dbopen.Handle
	Session *ingest.Session
	// Close closes the session and the handle; it runs after the merge has
	// decided what to do with its stage.
	Close func() error
}

// MergeOptions say what to merge, and into what.
type MergeOptions struct {
	// Package is the package's path, or a part or the sidecar of a split one.
	Package string
	// Path is the target data root, as for dbopen.Request.
	Path string
	// Open opens the target for writing, with recovery, creating it when it
	// is missing. It is called once the package's listing and signature
	// hold, and before anything is copied.
	Open func(ctx context.Context) (MergeTarget, error)
	// SourceCreds open the package's database when it is encrypted.
	SourceCreds dbmeta.Credentials
	// TargetCreds says a password was given for the target, which a target
	// that does not exist yet is then created with.
	TargetCreds bool
	// StageDir is where the package is copied; "" is beside the target.
	StageDir string
	// AllowPlaintext lets an encrypted package into an unencrypted database.
	AllowPlaintext  bool
	RequireVerified bool
	TrustedKey      *signer.Entry
	// Jobs is how many evidence are merged at once.
	Jobs int
	// Fit lowers jobs, or refuses them, to what fits in memory for these
	// items, of these sizes; nil keeps them.
	Fit func(db *badger.DB, jobs int, items []workpool.Item, sizes []int64) (int, error)
	// TempDir and Tuning are for the read-only open of the package's
	// database, as for dbopen.Request.
	TempDir string
	Tuning  *cnst.Tuning
	// Out is told how each evidence ended, in the package's order.
	Out func(MergeItem)

	// afterStage runs once the stage holds the package (tests).
	afterStage func(stage string) error
}

// MergeItem is how one evidence of the package ended.
type MergeItem struct {
	Name     string
	SourceID []byte
	// TargetID is the evidence holding the content in the target.
	TargetID []byte
	Outcome  string
	// Reason says why it was skipped.
	Reason string
	Size   int64
	Result ingest.Result
	// Bytes is what its chunk pass counted, as a merge of one at a time
	// would count it.
	Bytes ingest.Bytes
	// Err is why it failed or, with an outcome, what needs the examiner.
	Err error
	// Warned: it is in the target, but its digests did not match or its
	// index did not finish.
	Warned bool
}

// MergeResult is what a merge did.
type MergeResult struct {
	Root      string
	Stage     string
	Manifest  duesarc.DatabaseManifest
	Signature *SignatureInfo
	// Created is set when the merge created the target.
	Created bool

	Merged, Aliased, Present, Skipped, Failed int
	// Warned counts the evidence of the others that need the examiner.
	Warned int
	Bytes  ingest.Bytes
	// StageKept is set when the stage was left for a rerun to resume.
	StageKept bool
	Notices   []string
}

// Merge copies the evidence of a package into an existing database, or a
// new one. The package's listing and signature are checked first, then the
// target is opened, locked and recovered, then the package's files are
// copied beside it, each checked against the manifest as it is written. The
// copied database is opened read-only, and each of its evidence is stored
// again in the target's layout by ingest.Session.MergePresent: checked
// against the hash it records, indexed, and committed with where it came
// from; content the target holds becomes an alias. The copy is removed once
// every evidence is resolved, or failed in a way a rerun would repeat; a
// rerun of the same package resumes from it.
func Merge(ctx context.Context, opts MergeOptions) (res MergeResult, err error) {
	root, err := dbopen.ResolveRoot(opts.Path)
	if err != nil {
		return res, err
	}
	res.Root = root
	p, err := Open(opts.Package)
	if err != nil {
		return res, err
	}
	defer p.Close()
	m := p.Manifest
	res.Manifest = m
	if err := checkReadable(m); err != nil {
		return res, err
	}
	if p.zip.In.Verified() {
		if err := p.zip.In.Verify(ctx); err != nil {
			return res, err
		}
	}
	entries, bad := p.entries()
	checkMemtableLogs(m, bad)
	if bad.Count > 0 {
		return res, bad
	}
	if res.Signature, err = p.checkSignature(entries, opts.TrustedKey); err != nil {
		return res, err
	}
	if m.Database.Encrypted && !opts.SourceCreds.IsSet() {
		return res, lifecycle.Usagef("the package's database is encrypted; give its password with --%s or --%s", SourcePasswordFileFlag, SourcePasswordStdinFlag)
	}
	parent := cmp.Or(opts.StageDir, filepath.Dir(root))
	if parent, err = filepath.Abs(parent); err != nil {
		return res, err
	}
	if err := checkStageParent(root, parent); err != nil {
		return res, err
	}
	if err := checkPlaintext(m, root, opts); err != nil {
		return res, err
	}
	extract, derived, dirs := splitDerived(m)
	if err := checkDerived(ctx, entries, derived); err != nil {
		return res, err
	}

	t, err := opts.Open(ctx)
	if err != nil {
		return res, err
	}
	defer func() { err = errors.Join(err, t.Close()) }()
	res.Created = t.Handle.Created
	if m.Database.Encrypted && !t.Handle.Manifest.Encrypted() && !opts.AllowPlaintext {
		return res, plaintextError(root)
	}

	st, err := openStage(parent, root, m.ContentSHA256)
	if err != nil {
		return res, err
	}
	res.Stage = st.dir
	removeStage := false
	defer func() {
		if removeStage {
			err = errors.Join(err, st.remove())
			return
		}
		res.StageKept = true
		res.Notices = append(res.Notices, "the copy of the package is kept at "+st.dir+"; run the same command again to resume")
		err = errors.Join(err, st.release())
	}()
	if err := st.fill(ctx, p, entries, extract, dirs, root); err != nil {
		_, removeStage = errors.AsType[*DamagedError](err)
		return res, err
	}
	if opts.afterStage != nil {
		if err := opts.afterStage(st.dir); err != nil {
			return res, err
		}
	}

	src, err := openSource(ctx, st.dir, opts)
	if err != nil {
		_, removeStage = errors.AsType[*DamagedError](err)
		return res, err
	}
	run := &mergeRun{opts: opts, res: &res, src: src.DB, target: t.Session, origin: originOf(m, res.Signature)}
	err = run.items(ctx)
	err = errors.Join(err, src.Close())
	interrupted := ctx.Err() != nil
	removeStage = err == nil && !interrupted && !run.retryable
	if interrupted {
		return res, errors.Join(err, ctx.Err())
	}
	return res, err
}

// checkMemtableLogs adds a problem for each memtable log the package's
// database lists. Export refuses a database that was not closed cleanly, so
// only a made-up package holds one, and it would turn the read-only open of
// the copy into a writable recovery.
func checkMemtableLogs(m duesarc.DatabaseManifest, bad *DamagedError) {
	for _, f := range m.Files {
		top, name, ok := strings.Cut(f.Path, "/")
		if !ok || !strings.EqualFold(top, cnst.KVDBDIR) || strings.Contains(name, "/") {
			continue
		}
		if _, mem := duesarc.CutSuffixFold(name, memtableExt); mem {
			bad.add(duesarc.DatabaseDir+"/"+f.Path, "a memtable log, which a package of a database closed cleanly never holds")
		}
	}
}

// memtableExt is the extension of badger's memtable logs.
const memtableExt = ".mem"

// checkStageParent makes the folder the stage goes in and refuses it inside
// the target, with links resolved: a stage inside would be part of the
// database it copies into.
func checkStageParent(root, parent string) error {
	if err := os.MkdirAll(parent, cnst.DirPerm); err != nil {
		return err
	}
	info, err := os.Stat(parent)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return lifecycle.Usagef("--%s %s is not a folder", StageDirFlag, parent)
	}
	real, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return err
	}
	if inside(resolveExisting(root), real) {
		return lifecycle.Usagef("the package would be copied to %s, inside the database %s; pass --%s outside it", parent, root, StageDirFlag)
	}
	return nil
}

// resolveExisting is p with the links of its longest existing prefix
// resolved.
func resolveExisting(p string) string {
	rest := ""
	for {
		if real, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(real, rest)
		}
		up := filepath.Dir(p)
		if up == p {
			return filepath.Join(p, rest)
		}
		rest = filepath.Join(filepath.Base(p), rest)
		p = up
	}
}

// checkPlaintext refuses, before the target is opened, an encrypted package
// into a target that is not encrypted, or would be created without a
// password, unless AllowPlaintext lets it. The opened target decides.
func checkPlaintext(m duesarc.DatabaseManifest, root string, opts MergeOptions) error {
	if !m.Database.Encrypted || opts.AllowPlaintext {
		return nil
	}
	encrypted := opts.TargetCreds
	if tm, err := dbmeta.Load(root); err == nil {
		encrypted = tm.Encrypted()
	}
	if !encrypted {
		return plaintextError(root)
	}
	return nil
}

func plaintextError(root string) error {
	return lifecycle.Usagef("the package's database is encrypted and %s is not: its evidence would be stored unencrypted; pass --%s to allow it", root, AllowPlaintextFlag)
}

// splitDerived parts the manifest's files into those the stage holds and the
// full-text index and graph, which the target builds again and which are
// only checked; dirs are the folders the stage holds.
func splitDerived(m duesarc.DatabaseManifest) (extract, derived []duesarc.File, dirs []string) {
	for _, f := range m.Files {
		if isDerived(f.Path) {
			derived = append(derived, f)
		} else {
			extract = append(extract, f)
		}
	}
	for _, d := range m.Dirs {
		if !isDerived(d) {
			dirs = append(dirs, d)
		}
	}
	return extract, derived, dirs
}

// isDerived reports whether a manifest path is under the full-text index or
// the graph, in any case.
func isDerived(path string) bool {
	top, _, _ := strings.Cut(path, "/")
	return strings.EqualFold(top, cnst.FTSDIR) || strings.EqualFold(top, cnst.GRAPHDIR)
}

// checkDerived reads the files a merge does not copy against the manifest,
// so a damaged package is refused whichever of its files is damaged.
func checkDerived(ctx context.Context, entries map[string]*zip.File, files []duesarc.File) error {
	var total int64
	for _, f := range files {
		total += f.Size
	}
	bad := &DamagedError{}
	ph := progress.Start(ctx, "check", progress.Bytes, total)
	for _, f := range files {
		reason, err := checkEntry(ctx, entries[f.Path], f, io.Discard, ph)
		if err != nil {
			ph.Done(err)
			return err
		}
		if reason != "" {
			bad.add(entries[f.Path].Name, reason)
		}
	}
	if bad.Count > 0 {
		ph.Done(bad)
		return bad
	}
	ph.Done(nil)
	return nil
}

// mergeStage is the folder a merge copies a package into, with its lock and
// the marker that says which package it holds.
type mergeStage struct {
	dir, marker, lockPath string
	lock                  *fsx.Lock
}

// openStage locks the stage of the package whose content hash is sha, and
// makes it, or checks that the one there was made by a merge of this
// package: a plain folder of this user's, private on unix, with a marker
// naming the package. Its name follows from the package, which anyone can read, so a
// folder that is not all that is refused, never used.
func openStage(parent, root, sha string) (*mergeStage, error) {
	name := "." + filepath.Base(root) + mergeStageMarker + sha[:stageHashLen]
	st := &mergeStage{
		dir:      filepath.Join(parent, name),
		marker:   filepath.Join(parent, name+mergePackageSuffix),
		lockPath: filepath.Join(parent, name+mergeLockSuffix),
	}
	lock, err := fsx.TryLockStable(st.lockPath, false)
	if errors.Is(err, fsx.ErrLocked) {
		return nil, fmt.Errorf("another merge is using the copy of this package at %s: %w", st.dir, err)
	}
	if err != nil {
		return nil, err
	}
	st.lock = lock
	info, err := os.Lstat(st.dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// The marker comes first: a folder without one is never resumed.
		err = fsx.WriteFileAtomic(st.marker, []byte(sha+"\n"), cnst.BlobPerm)
		if err == nil {
			err = os.Mkdir(st.dir, cnst.DirPerm)
		}
		if err == nil {
			err = os.Chmod(st.dir, cnst.DirPerm)
		}
		if err != nil {
			return nil, errors.Join(err, st.release())
		}
		return st, nil
	case err != nil:
		return nil, errors.Join(err, st.release())
	}
	if info.Mode()&(fs.ModeSymlink|fs.ModeIrregular) != 0 || !fsx.PrivateDir(st.dir, info, cnst.DirPerm) || !st.markerNames(sha) {
		return nil, errors.Join(lifecycle.Usagef("%s exists and was not made by your merge of this package; remove it or pass --%s", st.dir, StageDirFlag), st.release())
	}
	return st, nil
}

// markerNames reports whether the stage's marker names the package sha.
func (st *mergeStage) markerNames(sha string) bool {
	if info, err := os.Lstat(st.marker); err != nil || !info.Mode().IsRegular() {
		return false
	}
	f, err := os.Open(st.marker)
	if err != nil {
		return false
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, int64(2*len(sha))))
	return err == nil && strings.TrimSpace(string(data)) == sha
}

// release unlocks the stage, leaving it for a rerun.
func (st *mergeStage) release() error {
	// Windows removes no file that is open.
	if err := st.lock.Unlock(); err != nil {
		return err
	}
	return fsx.Remove(st.lockPath)
}

// remove removes the stage, its marker and its lock.
func (st *mergeStage) remove() error {
	if err := fsx.RetryBusy(func() error { return os.RemoveAll(st.dir) }); err != nil {
		return errors.Join(err, st.release())
	}
	return errors.Join(fsx.Remove(st.marker), st.release())
}

// fill makes the stage hold exactly the files of extract and the folders of
// dirs, as the package has them: a file there that matches the manifest is
// kept, any other is written again, and everything else is removed. A link
// or special file anywhere in it refuses the stage: os.Root follows links
// inside it.
func (st *mergeStage) fill(ctx context.Context, p *Package, entries map[string]*zip.File, extract []duesarc.File, dirs []string, root string) error {
	want := map[string]duesarc.File{}
	keep := map[string]bool{}
	for _, d := range dirs {
		local, err := duesarc.LocalName(d)
		if err != nil {
			return err
		}
		keep[local] = true
	}
	for _, f := range extract {
		local, err := duesarc.LocalName(f.Path)
		if err != nil {
			return err
		}
		want[local] = f
		for d := filepath.Dir(local); d != "."; d = filepath.Dir(d) {
			keep[d] = true
		}
	}
	var stale []string
	var sized []string
	err := filepath.WalkDir(st.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == st.dir {
			return err
		}
		rel, err := filepath.Rel(st.dir, path)
		if err != nil {
			return err
		}
		t := d.Type()
		if t&(fs.ModeSymlink|fs.ModeIrregular) != 0 || !d.IsDir() && !t.IsRegular() {
			return lifecycle.Usagef("the copy of the package at %s holds %s, which is not a plain file or folder; remove the copy or pass --%s", st.dir, path, StageDirFlag)
		}
		if d.IsDir() {
			if !keep[rel] {
				stale = append(stale, path)
				return fs.SkipDir
			}
			return nil
		}
		f, ok := want[rel]
		if !ok {
			stale = append(stale, path)
			return nil
		}
		if info, err := d.Info(); err == nil && info.Size() == f.Size {
			sized = append(sized, rel)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, path := range stale {
		if err := fsx.RetryBusy(func() error { return os.RemoveAll(path) }); err != nil {
			return err
		}
	}

	r, err := os.OpenRoot(st.dir)
	if err != nil {
		return err
	}
	defer r.Close()
	have, err := st.matching(ctx, r, want, sized)
	if err != nil {
		return err
	}
	var todo []duesarc.File
	var need uint64
	for _, f := range extract {
		local, _ := duesarc.LocalName(f.Path)
		if !have[local] {
			todo = append(todo, f)
			need += uint64(f.Size) + perFileSlack
		}
	}
	if err := checkMergeSpace(st.dir, root, need, extract); err != nil {
		return err
	}
	for _, d := range dirs {
		local, _ := duesarc.LocalName(d)
		if err := r.MkdirAll(local, cnst.DirPerm); err != nil {
			return err
		}
	}
	if err := writeStage(ctx, r, entries, todo); err != nil {
		return err
	}
	return syncDirs(st.dir, dirs)
}

// matching reads the staged files named in sized, which have the size the
// manifest gives, and returns those whose SHA-256 is the manifest's too.
func (st *mergeStage) matching(ctx context.Context, r *os.Root, want map[string]duesarc.File, sized []string) (map[string]bool, error) {
	have := map[string]bool{}
	if len(sized) == 0 {
		return have, nil
	}
	var total int64
	for _, local := range sized {
		total += want[local].Size
	}
	ph := progress.Start(ctx, "check copy", progress.Bytes, total)
	for _, local := range sized {
		f, err := r.Open(local)
		if err != nil {
			ph.Done(err)
			return nil, err
		}
		h := sha256.New()
		_, err = duesarc.Copy(ctx, h, io.LimitReader(f, want[local].Size), ph.Add)
		f.Close()
		if err != nil {
			ph.Done(err)
			return nil, err
		}
		have[local] = hex.EncodeToString(h.Sum(nil)) == want[local].SHA256
	}
	ph.Done(nil)
	return have, nil
}

// checkMergeSpace checks that the stage has room for need bytes more and the
// target for the evidence it will store again: about what the package's
// chunk store and index take. On one volume they share its free space.
func checkMergeSpace(stage, root string, need uint64, extract []duesarc.File) error {
	var target uint64
	for _, f := range extract {
		top, _, _ := strings.Cut(f.Path, "/")
		if top == cnst.KVDBDIR || top == cnst.BLOBSDIR {
			target += uint64(f.Size)
		}
	}
	same, err := diskspace.SameVolume(stage, root)
	if err != nil {
		return err
	}
	if same {
		return diskspace.Check(stage, need+target)
	}
	if err := diskspace.Check(stage, need); err != nil {
		return err
	}
	return diskspace.Check(root, target)
}

// writeStage writes files into the stage, each replacing what is there and
// checked against the manifest as it is written. What is there is removed
// first: writing through a hard link would change the file it links to.
func writeStage(ctx context.Context, r *os.Root, entries map[string]*zip.File, files []duesarc.File) error {
	var total int64
	for _, f := range files {
		total += f.Size
	}
	bad := &DamagedError{}
	ph := progress.Start(ctx, "copy", progress.Bytes, total)
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			ph.Done(err)
			return err
		}
		local, err := duesarc.LocalName(f.Path)
		if err == nil {
			err = fsx.RetryBusy(func() error { return r.Remove(local) })
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			ph.Done(err)
			return err
		}
		reason, err := writeFile(ctx, r, entries[f.Path], f, ph)
		if err != nil {
			ph.Done(err)
			return err
		}
		if reason != "" {
			bad.add(entries[f.Path].Name, reason)
		}
	}
	if bad.Count > 0 {
		ph.Done(bad)
		return bad
	}
	ph.Done(nil)
	return nil
}

// openSource opens the staged database read-only. A database the open
// would have to recover is refused as damaged: the recovery would write to
// the copy, and export never packs such a database.
func openSource(ctx context.Context, dir string, opts MergeOptions) (*dbopen.Handle, error) {
	h, err := dbopen.Open(ctx, dbopen.Request{Path: dir, Mode: dbopen.ReadOnly, Creds: opts.SourceCreds, TempDir: opts.TempDir, Tuning: opts.Tuning})
	switch {
	case errors.Is(err, dbmeta.ErrWrongPassword) && opts.SourceCreds.IsSet():
		return nil, lifecycle.Usagef("wrong password for the package's database (--%s or --%s)", SourcePasswordFileFlag, SourcePasswordStdinFlag)
	case errors.Is(err, dbmeta.ErrPasswordRequired), errors.Is(err, dbmeta.ErrWrongPassword):
		return nil, lifecycle.Usagef("the package's database is encrypted; give its password with --%s or --%s", SourcePasswordFileFlag, SourcePasswordStdinFlag)
	case errors.Is(err, dbopen.ErrNeedsRecovery):
		return nil, &DamagedError{Problems: []Problem{{Name: duesarc.DatabaseDir, Reason: "its database was not closed cleanly"}}, Count: 1}
	case err != nil:
		return nil, fmt.Errorf("the package's database does not open: %w", err)
	}
	if !h.ReadOnly {
		return nil, errors.Join(&DamagedError{Problems: []Problem{{Name: duesarc.DatabaseDir, Reason: "its database was not closed cleanly"}}, Count: 1}, h.Close())
	}
	return h, nil
}

// originOf is where the package's evidence come from, as their merges
// record it.
func originOf(m duesarc.DatabaseManifest, sig *SignatureInfo) structs.MergeInfo {
	o := structs.MergeInfo{Package: m.ContentSHA256, At: time.Now().UnixNano()}
	if sig != nil {
		o.SourceActor = sig.Actor
		o.SourceActorID = signer.FormatID(sig.ActorID)
		o.SourceKeyID = signer.FormatID(sig.KeyID)
		o.Trusted = sig.Trusted
	}
	return o
}

// sourceItem is one evidence record of the package's database.
type sourceItem struct {
	id  []byte
	rec structs.EvidenceFile
	// err is why the record does not decode.
	err error
}

// mergeRun merges the evidence of one package's database.
type mergeRun struct {
	opts   MergeOptions
	res    *MergeResult
	src    *badger.DB
	target *ingest.Session
	origin structs.MergeInfo
	// retryable is set when an evidence failed in a way a rerun may not
	// repeat, so the stage is kept for it.
	retryable bool
}

// items merges every evidence of the source, jobs at a time, and reports
// each in order.
func (r *mergeRun) items(ctx context.Context) (err error) {
	var items []sourceItem
	err = store.WalkEvidence(r.src, func(eid []byte, e structs.EvidenceFile, err error) error {
		items = append(items, sourceItem{id: eid, rec: e, err: err})
		return nil
	})
	if err != nil {
		return fmt.Errorf("the package's evidence: %w", err)
	}
	db := r.target.DB()
	eng := engine.For(db)
	pool := make([]workpool.Item, len(items))
	sizes := make([]int64, len(items))
	var weight int64
	for i, it := range items {
		sizes[i] = it.rec.Size
		weight += itemWeight(it.rec.Size)
		if it.err == nil {
			pool[i] = workpool.Item{Key: r.contentKey(it.rec), Heavy: it.rec.Size >= ingest.HeavySize}
		}
	}
	jobs := max(r.opts.Jobs, 1)
	if jobs > 1 && r.opts.Fit != nil {
		if jobs, err = r.opts.Fit(db, jobs, pool, sizes); err != nil {
			return err
		}
	}

	all := progress.Start(ctx, "evidence", progress.Evidence, int64(len(items)))
	all.Weigh(weight)
	failed := all.Meter("failed", progress.Evidence)
	credits := store.NewChunkCredits()
	results := make([]ingest.Result, len(items))
	do := func(ctx context.Context, i int) error {
		it := items[i]
		if it.err != nil {
			return it.err
		}
		one := progress.Start(progress.Within(ctx, all), store.CleanName(it.rec.Name), progress.None, 0)
		one.Weigh(itemWeight(it.rec.Size))
		req := ingest.MergeRequest{
			Source: r.src, SourceID: it.id, Origin: r.origin, RequireVerified: r.opts.RequireVerified,
			Credit: store.Credit{Ledger: credits, Item: i}, Cache: dbio.NewChunkCache(dbio.DefaultChunkCacheBytes),
		}
		if jobs > 1 && !pool[i].Heavy {
			req.MaxWorkers = ingest.InnerWorkers(eng, jobs)
		}
		res, err := r.target.MergePresent(progress.Within(ctx, one), req)
		one.Done(err)
		all.Add(1)
		results[i] = res
		return err
	}
	done := func(i int, err error) {
		defer credits.Settle(i)
		if ctx.Err() != nil && results[i].Outcome == "" {
			return
		}
		it := r.report(items[i], results[i], err)
		it.Bytes = results[i].Bytes
		n, written := credits.Adjust(i)
		it.Bytes.New += n
		it.Bytes.Written += written
		r.res.Bytes.Add(it.Bytes)
		if it.Outcome == MergeFailed {
			failed.Add(1)
		}
		if r.opts.Out != nil {
			r.opts.Out(it)
		}
	}
	defer func() { all.Done(err) }()
	if jobs == 1 {
		for i := range items {
			if err = ctx.Err(); err != nil {
				return err
			}
			done(i, do(ctx, i))
		}
		return nil
	}
	if _, err = workpool.RunHeld(ctx, jobs, pool, credits.Full, do, done); err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		return &lifecycle.InternalError{Err: err}
	}
	return nil
}

// contentKey chains the evidence that may hold one content, so they merge
// in turn and the first in the package's order holds it.
func (r *mergeRun) contentKey(e structs.EvidenceFile) string {
	algo := cmp.Or(e.FileHashAlgo, dbmeta.HashAlgo(r.src))
	return fmt.Sprintf("%d:%s:%s", e.Size, algo, cmp.Or(e.PlainHash, e.FileHash))
}

// itemWeight is how much an evidence of size bytes weighs among others on
// the overall bar: its bytes plus what opening and committing one costs.
func itemWeight(size int64) int64 { return max(size, 0) + itemCost }

// itemCost is what one evidence costs beyond its bytes, as bytes of work.
const itemCost = 4 << 20

// report counts how one evidence ended and describes it.
func (r *mergeRun) report(it sourceItem, res ingest.Result, err error) MergeItem {
	out := MergeItem{Name: store.CleanName(it.rec.Name), SourceID: it.id, TargetID: res.EvidenceID, Size: it.rec.Size, Result: res, Err: err}
	switch {
	case errors.Is(err, ingest.ErrNotMergeable):
		out.Outcome, out.Reason, out.Err = MergeSkipped, notMergeableReason(err), nil
		r.res.Skipped++
		return out
	case res.Outcome == "" || err != nil && !onlyWarnings(err):
		// An evidence counts once: one stored or present whose merge then
		// failed counts as failed only.
		out.Outcome = MergeFailed
		out.Err = cmp.Or(err, errNoOutcome)
		r.res.Failed++
		if it.err == nil && !permanent(err) {
			r.retryable = true
		}
		return out
	case res.Outcome == store.CommitStored:
		out.Outcome = MergeMerged
		r.res.Merged++
	case res.Outcome == store.CommitAliased:
		out.Outcome = string(store.CommitAliased)
		r.res.Aliased++
	default:
		out.Outcome = string(store.CommitPresent)
		r.res.Present++
	}
	switch {
	case err != nil:
		out.Warned = true
	case res.Outcome == store.CommitPresent:
		out.Err = r.presentWarning(res.EvidenceID)
		out.Warned = out.Err != nil
	}
	if out.Warned {
		r.res.Warned++
	}
	return out
}

// notMergeableReason is why an evidence cannot be merged, without the
// sentinel's own words.
func notMergeableReason(err error) string {
	msg := err.Error()
	if _, after, ok := strings.Cut(msg, ingest.ErrNotMergeable.Error()+": "); ok {
		return after
	}
	return msg
}

// permanent reports whether a rerun of the merge would fail an evidence the
// same way: its bytes are not what its record names, or are damaged.
func permanent(err error) bool {
	_, mismatch := errors.AsType[*ingest.ContentMismatchError](err)
	digest, isDigest := errors.AsType[*ingest.DigestMismatchError](err)
	return mismatch || isDigest && !digest.Stored ||
		errors.Is(err, blobcodec.ErrCorrupt) || errors.Is(err, dbio.ErrChunkRange)
}

// errNoOutcome is why an evidence failed whose merge said neither how it
// ended nor what went wrong.
var errNoOutcome = errors.New("the merge of this evidence ended without a result")

// onlyWarnings reports whether err says only that a committed evidence
// needs the examiner: its index did not finish, or its digests did not
// match. Every error it wraps, however joined, must say only that.
func onlyWarnings(err error) bool {
	switch e := err.(type) {
	case *ingest.IndexIncompleteError:
		return true
	case *ingest.DigestMismatchError:
		return e.Stored
	case interface{ Unwrap() []error }:
		errs := e.Unwrap()
		for _, e := range errs {
			if !onlyWarnings(e) {
				return false
			}
		}
		return len(errs) > 0
	case interface{ Unwrap() error }:
		return onlyWarnings(e.Unwrap())
	}
	return false
}

// presentWarning says what of an evidence the target held before needs the
// examiner, as merging it would have: a stored digest that did not match,
// or an index that did not finish. Skipped and invalid indexes are not
// warned of: reindex would not change them.
func (r *mergeRun) presentWarning(id []byte) error {
	e, err := dbio.GetEvidenceFile(id, r.target.DB())
	if err != nil {
		return nil
	}
	if s := e.Source; s != nil && s.EWF != nil && s.EWF.Result == structs.EWFMismatch {
		return &ingest.DigestMismatchError{EvidenceID: id, Stored: true, Check: *s.EWF}
	}
	if info := e.Index; info != nil {
		switch info.State {
		case structs.IndexRunning, structs.IndexInterrupted, structs.IndexFailed, structs.IndexPartial:
			return &ingest.IndexIncompleteError{EvidenceID: id, State: info.State, Reason: info.Reason}
		}
	}
	return nil
}
