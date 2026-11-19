package dbpackage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"indicer/internal/duesarc"
	"indicer/internal/testutil"
	"indicer/internal/testutil/testdb"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/dbmeta"
	"indicer/lib/dbopen"
	"indicer/lib/fsx"
	"indicer/lib/graph/signer"
	"indicer/lib/ingest"
	"indicer/lib/store"
	"indicer/lib/structs"
	"indicer/lib/util"
)

// The merge tests run serially: each holds three databases, and badger
// commits a memtable and cache sketches for every one as it opens.

// mergeInto merges pkg into the data root root, opened as dues package merge
// opens it, with creds and, for a new database, layout; edit changes the
// options first.
func mergeInto(t *testing.T, pkg, root string, creds dbmeta.Credentials, layout dbopen.Requested, edit func(*MergeOptions)) (MergeResult, []MergeItem, error) {
	t.Helper()
	var items []MergeItem
	opts := MergeOptions{
		Package:     pkg,
		Path:        root,
		TargetCreds: creds.IsSet(),
		Open: func(ctx context.Context) (MergeTarget, error) {
			h, err := dbopen.Open(ctx, dbopen.Request{Path: root, Mode: dbopen.ReadWrite, Creds: creds, KDF: &fastKDF, Layout: layout, Recover: true, WrittenBy: "test"})
			if err != nil {
				return MergeTarget{}, err
			}
			if err := util.EnsureBlobPath(h.Root); err != nil {
				return MergeTarget{}, errors.Join(err, h.Close())
			}
			sess := ingest.NewSession(h.DB)
			return MergeTarget{Handle: h, Session: sess, Close: func() error { return errors.Join(sess.Close(), h.Close()) }}, nil
		},
		Out: func(it MergeItem) { items = append(items, it) },
	}
	if edit != nil {
		edit(&opts)
	}
	res, err := Merge(t.Context(), opts)
	return res, items, err
}

// contentSums returns the SHA-256 of the content of every evidence of the
// database at root that holds its own, by name.
func contentSums(t *testing.T, root string, creds dbmeta.Credentials) map[string]string {
	t.Helper()
	h, err := dbopen.Open(t.Context(), dbopen.Request{Path: root, Mode: dbopen.ReadOnly, Creds: creds})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	sums := map[string]string{}
	err = store.WalkEvidence(h.DB, func(eid []byte, e structs.EvidenceFile, err error) error {
		if err != nil || e.IngestState == structs.IngestStateDuplicate {
			return err
		}
		ec, err := dbio.OpenEvidenceChunks(h.DB, eid, dbio.ReadOptions{})
		if err != nil {
			return err
		}
		s := sha256.New()
		if _, err := io.Copy(s, io.NewSectionReader(ec, 0, e.Size)); err != nil {
			return err
		}
		sums[store.CleanName(e.Name)] = hex.EncodeToString(s.Sum(nil))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return sums
}

// noMergeStage fails when a merge left its copy, marker or lock beside root.
func noMergeStage(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(root))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), mergeStageMarker) {
			t.Errorf("the merge left %s", e.Name())
		}
	}
}

// stageOf is where a merge of the package res into root copies it.
func stageOf(root string, res ExportResult) string {
	return filepath.Join(filepath.Dir(root), "."+filepath.Base(root)+mergeStageMarker+res.Manifest.ContentSHA256[:stageHashLen])
}

// TestMergeRoundTrip: a package of a plain database with 256 KiB chunks
// merges into a new encrypted one with 64 KiB chunks; the content reads back
// the same, the merge records where it came from, leaves no copy behind, and
// a rerun finds every evidence present and writes nothing.
func TestMergeRoundTrip(t *testing.T) {
	src := newRoot(t, dbmeta.NoPassword())
	exp := export(t, ExportOptions{Path: src})
	target := filepath.Join(t.TempDir(), "target")
	creds := dbmeta.Password("target")
	res, items, err := mergeInto(t, exp.Output, target, creds, dbopen.Requested{ChunkKiB: 64}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created || res.Merged != 1 || res.Failed != 0 || len(items) != 1 || items[0].Outcome != MergeMerged {
		t.Fatalf("merge = %+v, items %+v", res, items)
	}
	if res.Signature == nil || res.Signature.Trusted {
		t.Errorf("signature %+v: want signed, not checked", res.Signature)
	}
	noMergeStage(t, target)
	want := contentSums(t, src, dbmeta.NoPassword())
	if got := contentSums(t, target, creds); len(got) != 1 || got[items[0].Name] != want[items[0].Name] {
		t.Errorf("content %v; want %v", got, want)
	}

	h, err := dbopen.Open(t.Context(), dbopen.Request{Path: target, Mode: dbopen.ReadOnly, Creds: creds})
	if err != nil {
		t.Fatal(err)
	}
	e, err := dbio.GetEvidenceFile(items[0].TargetID, h.DB)
	chunk := dbmeta.ChunkSize(h.DB)
	if cerr := h.Close(); err != nil || cerr != nil {
		t.Fatal(err, cerr)
	}
	if chunk != 64<<10 || e.Merged == nil || e.Merged.Package != exp.Manifest.ContentSHA256 || e.Merged.SourceActor != signerDatabase {
		t.Errorf("chunk size %d, merged %+v", chunk, e.Merged)
	}

	res, items, err = mergeInto(t, exp.Output, target, creds, dbopen.Requested{}, nil)
	if err != nil || res.Present != 1 || res.Merged != 0 || items[0].Outcome != string(store.CommitPresent) {
		t.Fatalf("rerun = %+v, %+v, %v", res, items, err)
	}
	noMergeStage(t, target)
}

// signerDatabase is the actor a database signs its packages as.
const signerDatabase = "database"

// TestMergeResumesItsCopy: a merge stopped once the package is copied
// keeps the copy; the rerun writes again a copied file that was changed,
// without writing through the hard link it was changed to, removes one that
// was added, and finishes.
func TestMergeResumesItsCopy(t *testing.T) {
	src := newRoot(t, dbmeta.NoPassword())
	exp := export(t, ExportOptions{Path: src})
	target := filepath.Join(t.TempDir(), "target")
	stop := errors.New("stopped after the copy")
	res, _, err := mergeInto(t, exp.Output, target, dbmeta.NoPassword(), dbopen.Requested{}, func(o *MergeOptions) {
		o.afterStage = func(string) error { return stop }
	})
	if !errors.Is(err, stop) || !res.StageKept {
		t.Fatalf("stopped merge = %+v, %v", res, err)
	}
	stage := stageOf(target, exp)
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(stage, dbmeta.ManifestName)
	if err := os.Remove(manifest); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, manifest); err != nil {
		t.Fatal(err)
	}
	extra := filepath.Join(stage, "extra.txt")
	if err := os.WriteFile(extra, []byte("not in the package"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, _, err = mergeInto(t, exp.Output, target, dbmeta.NoPassword(), dbopen.Requested{}, func(o *MergeOptions) {
		o.afterStage = func(stage string) error {
			if _, err := os.Stat(extra); err == nil {
				t.Error("the added file is still in the copy")
			}
			for _, dir := range []string{cnst.FTSDIR, cnst.GRAPHDIR} {
				if _, err := os.Stat(filepath.Join(stage, dir)); err == nil {
					t.Errorf("the copy holds %s", dir)
				}
			}
			return nil
		}
	})
	if err != nil || res.Merged != 1 || res.StageKept {
		t.Fatalf("resumed merge = %+v, %v", res, err)
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "changed" {
		t.Errorf("the file the copy linked to holds %q, %v", data, err)
	}
	noMergeStage(t, target)
}

// TestMergeStageRefusals: a copy another merge holds, one made for another
// package, or a folder at its name that no merge made is never used, and is
// left as it is; a copy inside the target is refused.
func TestMergeStageRefusals(t *testing.T) {
	src := newRoot(t, dbmeta.NoPassword())
	exp := export(t, ExportOptions{Path: src})
	target := filepath.Join(t.TempDir(), "target")
	stage := stageOf(target, exp)

	lock, err := fsx.TryLock(stage+mergeLockSuffix, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := mergeInto(t, exp.Output, target, dbmeta.NoPassword(), dbopen.Requested{}, nil); !errors.Is(err, fsx.ErrLocked) {
		t.Errorf("a held copy: %v", err)
	}
	if err := lock.Unlock(); err != nil {
		t.Fatal(err)
	}

	if err := os.Mkdir(stage, cnst.DirPerm); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(stage, "keep.txt")
	if err := os.WriteFile(sentinel, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, marker := range map[string]string{"no marker": "", "another package's": strings.Repeat("0", 64)} {
		if marker != "" {
			if err := os.WriteFile(stage+mergePackageSuffix, []byte(marker), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if _, _, err := mergeInto(t, exp.Output, target, dbmeta.NoPassword(), dbopen.Requested{}, nil); !isUsage(err) || !strings.Contains(err.Error(), "not made by your merge of this package") {
			t.Errorf("%s: %v", name, err)
		}
		if _, err := os.Stat(sentinel); err != nil {
			t.Errorf("%s: the folder was changed: %v", name, err)
		}
	}

	inside := filepath.Join(t.TempDir(), "inside")
	if _, _, err := mergeInto(t, exp.Output, inside, dbmeta.NoPassword(), dbopen.Requested{}, func(o *MergeOptions) {
		o.StageDir = filepath.Join(inside, "stage")
	}); !isUsage(err) || !strings.Contains(err.Error(), "inside the database") {
		t.Errorf("a copy inside the target: %v", err)
	}
}

// TestMergeRefusesDamage: a byte changed in a file the merge copies, or in
// the full-text index it only checks, refuses the package before anything
// is stored, and leaves no copy; damage in the index refuses it before the
// target is made.
func TestMergeRefusesDamage(t *testing.T) {
	src := newRoot(t, dbmeta.NoPassword())
	exp := export(t, ExportOptions{Path: src})
	fts := duesarc.DatabaseDir + "/fts/store/root.bolt"
	for _, entry := range []string{fts, duesarc.DatabaseDir + "/" + dbmeta.ManifestName} {
		damaged := rewrite(t, exp.Output, func(name string, data []byte) ([]byte, bool) {
			if name == entry {
				data = append([]byte(nil), data...)
				data[0] ^= 1
			}
			return data, true
		}, nil)
		target := filepath.Join(t.TempDir(), "target")
		res, _, err := mergeInto(t, damaged, target, dbmeta.NoPassword(), dbopen.Requested{}, nil)
		if !isDamaged(err, entry) || res.Merged != 0 {
			t.Errorf("%s changed: %+v, %v", entry, res, err)
		}
		if _, err := os.Stat(target); entry == fts && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s changed: the target was made: %v", entry, err)
		}
		noMergeStage(t, target)
	}
}

// TestMergeReportCountsOnce: an evidence stored or found present whose merge
// then failed counts as failed only, and is kept for a rerun only when a
// rerun may not fail it the same way; one that ended without a result says
// so.
func TestMergeReportCountsOnce(t *testing.T) {
	cases := []struct {
		name      string
		res       ingest.Result
		err       error
		retryable bool
	}{
		{"stored, aliases not carried", ingest.Result{Outcome: store.CommitStored}, errors.New("carry aliases"), true},
		{"present, content not the package's", ingest.Result{Outcome: store.CommitPresent}, &ingest.ContentMismatchError{}, false},
		{"no result", ingest.Result{}, nil, true},
	}
	for _, c := range cases {
		r := &mergeRun{res: &MergeResult{}}
		it := r.report(sourceItem{}, c.res, c.err)
		total := r.res.Merged + r.res.Aliased + r.res.Present + r.res.Skipped + r.res.Failed
		if it.Outcome != MergeFailed || r.res.Failed != 1 || total != 1 || it.Err == nil || r.retryable != c.retryable {
			t.Errorf("%s: item %+v, result %+v, retryable %v", c.name, it, r.res, r.retryable)
		}
	}
}

// TestOnlyWarnings: an error is a warning only when every error it wraps,
// joined or wrapped with context at any depth, is one.
func TestOnlyWarnings(t *testing.T) {
	incomplete := &ingest.IndexIncompleteError{}
	stored := &ingest.DigestMismatchError{Stored: true}
	hard := errors.New("disk failed")
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{incomplete, true},
		{stored, true},
		{&ingest.DigestMismatchError{}, false},
		{fmt.Errorf("evidence: %w", incomplete), true},
		{errors.Join(incomplete, stored), true},
		{fmt.Errorf("evidence: %w", errors.Join(incomplete, hard)), false},
		{errors.Join(fmt.Errorf("index: %w", incomplete), fmt.Errorf("aliases: %w", hard)), false},
		{hard, false},
	}
	for _, c := range cases {
		if got := onlyWarnings(c.err); got != c.want {
			t.Errorf("onlyWarnings(%v) = %v; want %v", c.err, got, c.want)
		}
	}
}

// TestMergeEncryption: an encrypted package needs its password, and goes
// into an unencrypted database only when allowed.
func TestMergeEncryption(t *testing.T) {
	src := newRoot(t, dbmeta.Password("source"))
	exp := export(t, ExportOptions{Path: src, Creds: dbmeta.Password("source")})
	target := filepath.Join(t.TempDir(), "target")
	plain := dbmeta.NoPassword()
	if _, _, err := mergeInto(t, exp.Output, target, plain, dbopen.Requested{}, nil); !isUsage(err) || !strings.Contains(err.Error(), SourcePasswordFileFlag) {
		t.Errorf("no source password: %v", err)
	}
	withSource := func(pw string, allow bool) func(*MergeOptions) {
		return func(o *MergeOptions) { o.SourceCreds, o.AllowPlaintext = dbmeta.Password(pw), allow }
	}
	if _, _, err := mergeInto(t, exp.Output, target, plain, dbopen.Requested{}, withSource("source", false)); !isUsage(err) || !strings.Contains(err.Error(), AllowPlaintextFlag) {
		t.Errorf("into a new unencrypted database: %v", err)
	}
	if _, err := os.Stat(target); err == nil {
		t.Error("the refused merge made the target")
	}
	if _, _, err := mergeInto(t, exp.Output, target, plain, dbopen.Requested{}, withSource("wrong", true)); !isUsage(err) || !strings.Contains(err.Error(), "wrong password") {
		t.Errorf("a wrong source password: %v", err)
	}
	res, _, err := mergeInto(t, exp.Output, target, plain, dbopen.Requested{}, withSource("source", true))
	if err != nil || res.Merged != 1 {
		t.Fatalf("allowed: %+v, %v", res, err)
	}
	noMergeStage(t, target)
	if got, want := contentSums(t, target, plain), contentSums(t, src, dbmeta.Password("source")); len(got) != 1 || len(want) != 1 || got[items0(got)] != want[items0(want)] {
		t.Errorf("content %v; want %v", got, want)
	}

	encrypted := filepath.Join(t.TempDir(), "encrypted")
	res, _, err = mergeInto(t, exp.Output, encrypted, dbmeta.Password("target"), dbopen.Requested{}, withSource("source", false))
	if err != nil || res.Merged != 1 {
		t.Errorf("into an encrypted database: %+v, %v", res, err)
	}
}

// items0 is the one key of a map of one.
func items0(m map[string]string) string {
	for k := range m {
		return k
	}
	return ""
}

// TestMergeTrustedKey: under --trusted-key, a package that key did not
// sign, or that is unsigned, is refused before the target is opened.
func TestMergeTrustedKey(t *testing.T) {
	src := newRoot(t, dbmeta.NoPassword())
	exp := export(t, ExportOptions{Path: src})
	trusted := currentDatabaseKey(t, src)
	other, ring := otherDatabaseActor(t)
	stripped := rewrite(t, exp.Output, func(name string, data []byte) ([]byte, bool) {
		if name == duesarc.ManifestName {
			data = unsigned(t, data)
		}
		return data, true
	}, nil)
	for name, pkg := range map[string]string{"signed by another key": resign(t, exp.Output, other, ring), "unsigned": stripped} {
		target := filepath.Join(t.TempDir(), "target")
		opened := false
		_, _, err := mergeInto(t, pkg, target, dbmeta.NoPassword(), dbopen.Requested{}, func(o *MergeOptions) {
			o.TrustedKey = &trusted
			open := o.Open
			o.Open = func(ctx context.Context) (MergeTarget, error) { opened = true; return open(ctx) }
		})
		if _, ok := errors.AsType[*SignatureError](err); !ok || opened {
			t.Errorf("%s: %v (target opened: %v)", name, err, opened)
		}
	}
	target := filepath.Join(t.TempDir(), "target")
	res, _, err := mergeInto(t, exp.Output, target, dbmeta.NoPassword(), dbopen.Requested{}, func(o *MergeOptions) { o.TrustedKey = &trusted })
	if err != nil || res.Signature == nil || !res.Signature.Trusted {
		t.Errorf("signed by the trusted key: %+v, %v", res.Signature, err)
	}
	h, err := dbopen.Open(t.Context(), dbopen.Request{Path: target, Mode: dbopen.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	var trustedMerge bool
	_ = store.WalkEvidence(h.DB, func(_ []byte, e structs.EvidenceFile, _ error) error {
		trustedMerge = e.Merged != nil && e.Merged.Trusted
		return nil
	})
	if !trustedMerge {
		t.Error("the merged evidence does not record the trusted signature")
	}
}

// TestMergeJobsAndAliases: a package of several evidence, one a copy of
// another, merges to the same target one at a time and four at a time; the
// copy stays an alias, carried with where it came from.
func TestMergeJobsAndAliases(t *testing.T) {
	root := testdb.NewDataRoot(t)
	h, err := dbopen.Open(t.Context(), dbopen.Request{Path: root, Mode: dbopen.ReadWrite, WrittenBy: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := util.EnsureBlobPath(h.Root); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	var paths []string
	for i, size := range []int{200 << 10, 70 << 10, 300 << 10, 1 << 10} {
		paths = append(paths, testutil.WriteRandomFile(t, dir, "e"+string(rune('a'+i))+".bin", uint64(i+1), size))
	}
	copyPath := filepath.Join(dir, "copy.bin")
	data, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(copyPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range append(paths, copyPath) {
		if _, err := ingest.Run(t.Context(), h.DB, ingest.Request{Path: p, Decode: true, Quiet: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	exp := export(t, ExportOptions{Path: root})

	type tally struct{ merged, aliased, present, skipped, failed, warned int }
	var counts []tally
	for _, jobs := range []int{1, 4} {
		target := filepath.Join(t.TempDir(), "target")
		res, items, err := mergeInto(t, exp.Output, target, dbmeta.NoPassword(), dbopen.Requested{ChunkKiB: 64}, func(o *MergeOptions) { o.Jobs = jobs })
		if err != nil {
			t.Fatalf("jobs %d: %v", jobs, err)
		}
		if len(items) != len(paths) {
			t.Errorf("jobs %d: %d items reported", jobs, len(items))
		}
		counts = append(counts, tally{res.Merged, res.Aliased, res.Present, res.Skipped, res.Failed, res.Warned})

		th, err := dbopen.Open(t.Context(), dbopen.Request{Path: target, Mode: dbopen.ReadOnly})
		if err != nil {
			t.Fatal(err)
		}
		var carried int
		for _, it := range items {
			aliases, err := dbio.Aliases(th.DB, it.TargetID)
			if err != nil {
				t.Fatal(err)
			}
			for _, a := range aliases {
				if a.Merged != nil && a.Merged.Package == exp.Manifest.ContentSHA256 && a.AttemptID == nil {
					carried++
				}
			}
		}
		if cerr := th.Close(); cerr != nil {
			t.Fatal(cerr)
		}
		if carried == 0 {
			t.Errorf("jobs %d: no merged alias was carried", jobs)
		}
		if got, want := contentSums(t, target, dbmeta.NoPassword()), contentSums(t, root, dbmeta.NoPassword()); len(got) != len(want) {
			t.Errorf("jobs %d: %d evidence hold content; want %d", jobs, len(got), len(want))
		} else {
			for name, sum := range want {
				if got[name] != sum {
					t.Errorf("jobs %d: %s differs", jobs, name)
				}
			}
		}
	}
	if counts[0] != counts[1] {
		t.Errorf("jobs 1 %+v, jobs 4 %+v", counts[0], counts[1])
	}
}

// TestPackageListingFoldsCase: a private key is refused in any case, and
// two names one file on this filesystem are refused where it folds case.
func TestPackageListingFoldsCase(t *testing.T) {
	t.Parallel()
	for _, rel := range []string{cnst.KEYSDIR + "/a" + signer.KeyFileExt, strings.ToUpper(cnst.KEYSDIR + "/a" + signer.KeyFileExt)} {
		if !isPrivateKey(rel) {
			t.Errorf("%s is not taken for a private key", rel)
		}
	}
	if isPrivateKey(cnst.KEYSDIR + "/keyring.json") {
		t.Error("the keyring is taken for a private key")
	}
	m := duesarc.DatabaseManifest{Dirs: []string{"kvdb"}, Files: []duesarc.File{{Path: "KVDB/a"}, {Path: "kvdb/A"}}}
	bad := &DamagedError{}
	checkFoldedNames(m, bad)
	folds := duesarc.FoldName("A") == duesarc.FoldName("a")
	if folds && bad.Count != 1 || !folds && bad.Count != 0 {
		t.Errorf("folding %v: %+v", folds, bad)
	}
	mem := &DamagedError{}
	checkMemtableLogs(duesarc.DatabaseManifest{Files: []duesarc.File{{Path: "kvdb/000001.MEM"}, {Path: "kvdb/000001.sst"}}}, mem)
	if mem.Count != 1 {
		t.Errorf("memtable logs: %+v", mem)
	}
}
