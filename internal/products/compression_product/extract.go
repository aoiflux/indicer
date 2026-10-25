package compression_product

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	coreerr "indicer/internal/core/errors"
	"indicer/internal/diskspace"
	"indicer/lib/fsx"
)

// Conflict policies for an entry whose target already exists.
const (
	conflictOverwrite = "overwrite" // replace the file (the default)
	conflictSkip      = "skip"      // keep the existing file
	conflictError     = "error"     // refuse the extraction, before anything is moved

	stagePrefix = ".qwikpak-stage-"
	backupDir   = ".replaced"
	maxReported = 20
)

func resolveConflictPolicy(p string) (string, *coreerr.Error) {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case "", conflictOverwrite:
		return conflictOverwrite, nil
	case conflictSkip:
		return conflictSkip, nil
	case conflictError:
		return conflictError, nil
	}
	return "", coreerr.NewWithDetails(coreerr.CodeInvalidRequest, "on_conflict must be overwrite, skip or error", map[string]string{"on_conflict": p})
}

// entryProblem is one archive entry that cannot be extracted as it is.
type entryProblem struct {
	name   string
	reason string
}

// ArchiveError means the archive cannot be extracted here as it stands: an
// entry's name is unsafe or invalid on this system, two entries share a
// name, or the archive's data is damaged.
type ArchiveError struct {
	Msg      string
	Problems []entryProblem // the first maxReported
	Count    int            // all problems
}

func (e *ArchiveError) Error() string {
	if len(e.Problems) == 0 {
		return e.Msg
	}
	return fmt.Sprintf("%s: %s: %s", e.Msg, e.Problems[0].name, e.Problems[0].reason)
}

// ConflictError means entries' targets exist and the policy is "error", or
// an entry's target is of the other kind (a file where a directory goes).
type ConflictError struct {
	Problems []entryProblem
	Count    int
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("%d entries would replace existing files: %s", e.Count, e.Problems[0].name)
}

// damagedError marks an error reading the archive itself, as opposed to
// writing the output.
type damagedError struct{ err error }

func (e *damagedError) Error() string { return "archive data is damaged: " + e.err.Error() }
func (e *damagedError) Unwrap() error { return e.err }

// damageReader marks every error from r, except io.EOF, as damage.
type damageReader struct{ r io.Reader }

func (d damageReader) Read(p []byte) (int, error) {
	n, err := d.r.Read(p)
	if err != nil && err != io.EOF {
		err = &damagedError{err}
	}
	return n, err
}

func problemDetails(problems []entryProblem, count int) map[string]string {
	d := map[string]string{"count": strconv.Itoa(count)}
	for i, p := range problems {
		d["entry_"+strconv.Itoa(i+1)] = p.name + ": " + p.reason
	}
	return d
}

// extractionError maps an extraction failure to the error a caller gets.
func extractionError(err error, msg string) *coreerr.Error {
	var ae *ArchiveError
	var ce *ConflictError
	var de *damagedError
	switch {
	case errors.As(err, &ae):
		return coreerr.NewWithDetails(coreerr.CodeInvalidArchive, ae.Error(), problemDetails(ae.Problems, ae.Count))
	case errors.As(err, &ce):
		return coreerr.NewWithDetails(coreerr.CodeConflict, ce.Error(), problemDetails(ce.Problems, ce.Count))
	case errors.As(err, &de):
		return coreerr.NewWithDetails(coreerr.CodeInvalidArchive, de.Error(), map[string]string{detailCause: de.err.Error()})
	}
	if le, ok := asLimitError(err); ok {
		return limitExceeded(le)
	}
	return coreerr.NewWithDetails(coreerr.CodeInternal, msg, map[string]string{detailCause: err.Error()})
}

// stagedEntry is one entry written to the staging directory.
type stagedEntry struct {
	rel   string // local path, OS separators
	dir   bool
	mtime time.Time
}

// extraction writes an archive's entries into a staging directory inside
// the output directory, through os.Root so that no entry can reach outside
// it, and then moves them into place. Until commit, the output is untouched;
// a failed commit is rolled back.
type extraction struct {
	out       string
	created   []string // directories made for out itself, outermost first
	outRoot   *os.Root
	stageName string
	stage     *os.Root
	limits    Limits
	policy    string
	guard     *outputGuard

	entries  []stagedEntry
	seen     map[string]bool // folded local path -> is a directory
	problems []entryProblem
	nprob    int
	// conflicts are entries commit would refuse, found as they are added
	// so that a refused extraction writes no more content.
	conflicts []entryProblem
	nconf     int
	skipped   []string
	nskipped  int
	count     int64
	files     int
	dirs      int
}

// newExtraction prepares an extraction into out, creating it if needed.
// in reports the compressed bytes read so far, for the ratio limit.
func newExtraction(out string, limits Limits, policy string, in func() int64) (*extraction, error) {
	out, err := filepath.Abs(out)
	if err != nil {
		return nil, err
	}
	x := &extraction{out: out, limits: limits, policy: policy, seen: map[string]bool{}}
	x.guard = &outputGuard{limits: limits, in: in}
	if err := x.makeOut(); err != nil {
		return nil, err
	}
	if x.outRoot, err = os.OpenRoot(out); err != nil {
		x.removeCreated()
		return nil, err
	}
	var b [8]byte
	_, _ = rand.Read(b[:])
	x.stageName = stagePrefix + hex.EncodeToString(b[:])
	if err := x.outRoot.Mkdir(x.stageName, 0o700); err != nil {
		x.abort()
		return nil, err
	}
	if x.stage, err = x.outRoot.OpenRoot(x.stageName); err != nil {
		x.abort()
		return nil, err
	}
	return x, nil
}

// makeOut creates out and any missing parents, remembering which.
func (x *extraction) makeOut() error {
	var missing []string
	for dir := x.out; ; dir = filepath.Dir(dir) {
		info, err := os.Stat(dir)
		if err == nil {
			if !info.IsDir() {
				return fmt.Errorf("%s is not a directory", dir)
			}
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		missing = append(missing, dir)
		if filepath.Dir(dir) == dir {
			break
		}
	}
	for i := len(missing) - 1; i >= 0; i-- {
		if err := os.Mkdir(missing[i], defaultDirPerm); err != nil && !errors.Is(err, fs.ErrExist) {
			x.removeCreated()
			return err
		}
		x.created = append(x.created, missing[i])
	}
	return nil
}

func (x *extraction) removeCreated() {
	for i := len(x.created) - 1; i >= 0; i-- {
		_ = os.Remove(x.created[i]) // only when empty
	}
	x.created = nil
}

// abort removes the staging directory, and the output directory if this
// extraction created it and it is empty.
func (x *extraction) abort() {
	if x.stage != nil {
		_ = x.stage.Close()
		x.stage = nil
	}
	if x.outRoot != nil {
		_ = fsx.RetryBusy(func() error { return x.outRoot.RemoveAll(x.stageName) })
		_ = x.outRoot.Close()
		x.outRoot = nil
	}
	x.removeCreated()
}

// reserve checks the archive's declared output against the limits and the
// free space, before anything is written.
func (x *extraction) reserve(declared int64) error {
	if declared > x.limits.MaxTotalBytes {
		return &LimitError{Limit: "max_total_bytes", Value: x.limits.MaxTotalBytes, What: "declared output"}
	}
	if free, err := diskspace.Free(x.out); err == nil && declared > 0 && uint64(declared) > free {
		return &LimitError{Limit: "free_space", Value: int64(min(free, 1<<62)), What: "declared output"}
	}
	return nil
}

func (x *extraction) problem(name, reason string) {
	x.nprob++
	if len(x.problems) < maxReported {
		x.problems = append(x.problems, entryProblem{name: name, reason: reason})
	}
}

// skip records an entry that is not extracted (a link or a device).
func (x *extraction) skip(name, kind string) {
	x.nskipped++
	if len(x.skipped) < maxReported {
		x.skipped = append(x.skipped, name+" ("+kind+")")
	}
}

// skipEntry counts an entry that is not extracted, and records it.
func (x *extraction) skipEntry(name, kind string) error {
	if err := x.countEntry(name); err != nil {
		return err
	}
	x.skip(name, kind)
	return nil
}

// countEntry applies the entry-count and path-length limits.
func (x *extraction) countEntry(name string) error {
	x.count++
	if x.count > x.limits.MaxEntries {
		return &LimitError{Limit: "max_entries", Value: x.limits.MaxEntries, What: "entry count"}
	}
	if int64(len(name)) > x.limits.MaxPathBytes {
		return &LimitError{Limit: "max_path_bytes", Value: x.limits.MaxPathBytes, What: "entry path"}
	}
	return nil
}

// localName turns an archive entry name (slash-separated) into a local
// path, or explains why it cannot be one. "" with no error is the archive
// root itself.
func localName(name string) (string, error) {
	switch {
	case strings.ContainsRune(name, '\\'):
		return "", errors.New("a backslash in a name is ambiguous")
	case strings.ContainsRune(name, 0):
		return "", errors.New("a NUL byte in a name")
	case strings.HasPrefix(name, "/"):
		return "", errors.New("an absolute path")
	}
	var keep []string
	for _, c := range strings.Split(name, "/") {
		switch c {
		case "", ".":
			continue
		case "..":
			return "", errors.New("a reference to a parent directory")
		}
		if err := checkComponent(c); err != nil {
			return "", err
		}
		keep = append(keep, c)
	}
	if len(keep) == 0 {
		return "", nil
	}
	local, err := filepath.Localize(strings.Join(keep, "/"))
	if err != nil {
		return "", errors.New("not a valid name on this system")
	}
	return local, nil
}

// claim registers local as a directory or file entry, with its parents as
// directories. It returns a reason when the name is taken.
func (x *extraction) claim(local string, dir bool) string {
	key := foldName(local)
	if isDir, ok := x.seen[key]; ok {
		if dir && isDir {
			return ""
		}
		return "another entry has the same name"
	}
	for p := filepath.Dir(local); p != "." && p != string(filepath.Separator); p = filepath.Dir(p) {
		pk := foldName(p)
		if isDir, ok := x.seen[pk]; ok {
			if !isDir {
				return "a file entry has the name of its directory"
			}
			break
		}
		x.seen[pk] = true
	}
	x.seen[key] = dir
	return ""
}

// addDir stages a directory entry.
func (x *extraction) addDir(name string, mtime time.Time) error {
	if err := x.countEntry(name); err != nil {
		return err
	}
	local, err := localName(name)
	if err != nil {
		x.problem(name, err.Error())
		return nil
	}
	if local == "" {
		return nil
	}
	if reason := x.claim(local, true); reason != "" {
		x.problem(name, reason)
		return nil
	}
	x.target(local, true) // a conflict found now stops the writing
	if x.nprob > 0 || x.nconf > 0 {
		return nil // the extraction will be refused; write nothing more
	}
	if err := x.stage.MkdirAll(local, defaultDirPerm); err != nil {
		return err
	}
	x.entries = append(x.entries, stagedEntry{rel: local, dir: true, mtime: mtime})
	x.dirs++
	return nil
}

// addFile stages a file entry whose content fill writes. declared is the
// size the archive states, or -1.
func (x *extraction) addFile(name string, declared int64, mtime time.Time, fill func(w io.Writer) error) error {
	if err := x.countEntry(name); err != nil {
		return err
	}
	if declared > x.limits.MaxEntryBytes {
		return &LimitError{Limit: "max_entry_bytes", Value: x.limits.MaxEntryBytes, What: "entry " + name}
	}
	local, err := localName(name)
	if err == nil && local == "" {
		err = errors.New("a file entry with no name")
	}
	if err != nil {
		x.problem(name, err.Error())
		return nil
	}
	if reason := x.claim(local, false); reason != "" {
		x.problem(name, reason)
		return nil
	}
	x.target(local, false)
	if x.nprob > 0 || x.nconf > 0 {
		return nil
	}
	if dir := filepath.Dir(local); dir != "." {
		if err := x.stage.MkdirAll(dir, defaultDirPerm); err != nil {
			return err
		}
	}
	f, err := x.stage.OpenFile(local, os.O_WRONLY|os.O_CREATE|os.O_EXCL, defaultFilePerm)
	if err != nil {
		return err
	}
	ew := &entryWriter{w: guardedWriter{w: f, g: x.guard}, limit: x.limits.MaxEntryBytes, name: name}
	err = fill(ew)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	x.entries = append(x.entries, stagedEntry{rel: local, mtime: mtime})
	x.files++
	return nil
}

// entryWriter enforces the per-entry limit.
type entryWriter struct {
	w     io.Writer
	n     int64
	limit int64
	name  string
}

func (e *entryWriter) Write(p []byte) (int, error) {
	if e.n+int64(len(p)) > e.limit {
		return 0, &LimitError{Limit: "max_entry_bytes", Value: e.limit, What: "entry " + e.name}
	}
	n, err := e.w.Write(p)
	e.n += int64(n)
	return n, err
}

// What commit does with an entry whose target exists.
const (
	targetNew = iota // nothing is there
	targetSkip
	targetReplace
	targetConflict
)

// target checks rel's target in the output under the policy, recording a
// conflict.
func (x *extraction) target(rel string, dir bool) int {
	info, err := x.outRoot.Lstat(rel)
	if errors.Is(err, fs.ErrNotExist) {
		return targetNew
	}
	reason := ""
	switch {
	case err != nil:
		reason = err.Error()
	case dir && info.IsDir():
		return targetNew
	case dir:
		reason = "a file is where the directory goes"
	case info.IsDir():
		reason = "a directory is where the file goes"
	case x.policy == conflictError:
		reason = "exists"
	case x.policy == conflictSkip:
		return targetSkip
	default:
		return targetReplace
	}
	x.nconf++
	if len(x.conflicts) < maxReported {
		x.conflicts = append(x.conflicts, entryProblem{name: filepath.ToSlash(rel), reason: reason})
	}
	return targetConflict
}

// refused returns the error for the problems found, if any.
func (x *extraction) refused() error {
	if x.nprob == 0 {
		return nil
	}
	return &ArchiveError{Msg: "archive cannot be extracted here", Problems: x.problems, Count: x.nprob}
}

// commitResult says what commit did.
type commitResult struct {
	files, dirs    int
	replaced, kept int
	skipped        int
	skippedEntries []string
}

type journalOp struct {
	kind string // "mkdir", "move", "backup"
	rel  string
}

// commit moves the staged entries into the output under the conflict
// policy. Conflicts are checked for every entry before anything moves; if a
// move fails, the moves made so far are undone.
func (x *extraction) commit() (res commitResult, err error) {
	defer x.abort()
	if err := x.refused(); err != nil {
		return res, err
	}

	if x.nconf > 0 {
		return res, &ConflictError{Problems: x.conflicts, Count: x.nconf}
	}

	// Check every target again: the output may have changed since.
	replace := map[string]bool{}
	keep := map[string]bool{}
	for _, e := range x.entries {
		switch x.target(e.rel, e.dir) {
		case targetSkip:
			keep[e.rel] = true
		case targetReplace:
			replace[e.rel] = true
		}
	}
	if x.nconf > 0 {
		return res, &ConflictError{Problems: x.conflicts, Count: x.nconf}
	}

	var journal []journalOp
	defer func() {
		if err != nil {
			x.rollback(journal)
		}
	}()
	backup := filepath.Join(x.stageName, backupDir)
	mkdirs := func(rel string) error {
		var missing []string
		for p := rel; p != "." && p != string(filepath.Separator); p = filepath.Dir(p) {
			if _, err := x.outRoot.Lstat(p); err == nil {
				break
			}
			missing = append(missing, p)
		}
		for i := len(missing) - 1; i >= 0; i-- {
			if err := x.outRoot.Mkdir(missing[i], defaultDirPerm); err != nil {
				return err
			}
			journal = append(journal, journalOp{"mkdir", missing[i]})
		}
		return nil
	}
	for _, e := range x.entries {
		if e.dir {
			if err := mkdirs(e.rel); err != nil {
				return res, err
			}
			res.dirs++
			continue
		}
		if keep[e.rel] {
			res.kept++
			continue
		}
		if err := mkdirs(filepath.Dir(e.rel)); err != nil {
			return res, err
		}
		staged := filepath.Join(x.stageName, e.rel)
		if replace[e.rel] {
			// A replaced file must be complete on disk before the old one
			// goes.
			if err := syncIn(x.stage, e.rel); err != nil {
				return res, err
			}
			if err := x.outRoot.MkdirAll(filepath.Join(backup, filepath.Dir(e.rel)), 0o700); err != nil {
				return res, err
			}
			if err := renameIn(x.outRoot, e.rel, filepath.Join(backup, e.rel)); err != nil {
				return res, err
			}
			journal = append(journal, journalOp{"backup", e.rel})
			res.replaced++
		}
		if err := renameIn(x.outRoot, staged, e.rel); err != nil {
			return res, err
		}
		journal = append(journal, journalOp{"move", e.rel})
		res.files++
	}
	for _, e := range x.entries {
		if !e.mtime.IsZero() && !keep[e.rel] {
			_ = x.outRoot.Chtimes(e.rel, e.mtime, e.mtime)
		}
	}
	_ = fsx.SyncDir(x.out)
	res.skipped = x.nskipped
	res.skippedEntries = x.skipped
	return res, nil
}

// rollback undoes journal in reverse: moved files go back to the staging
// directory, replaced files come back from their backups, and directories
// made are removed.
func (x *extraction) rollback(journal []journalOp) {
	backup := filepath.Join(x.stageName, backupDir)
	for i := len(journal) - 1; i >= 0; i-- {
		op := journal[i]
		switch op.kind {
		case "move":
			_ = renameIn(x.outRoot, op.rel, filepath.Join(x.stageName, op.rel))
		case "backup":
			_ = renameIn(x.outRoot, filepath.Join(backup, op.rel), op.rel)
		case "mkdir":
			_ = x.outRoot.Remove(op.rel)
		}
	}
}

func renameIn(r *os.Root, from, to string) error {
	return fsx.RetryBusy(func() error { return r.Rename(from, to) })
}

func syncIn(r *os.Root, rel string) error {
	f, err := r.OpenFile(rel, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	err = f.Sync()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
