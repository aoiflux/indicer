package cli

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"indicer/internal/console"
	"indicer/internal/duesarc"
	"indicer/internal/lifecycle"
	"indicer/internal/termsafe"
	"indicer/internal/ui"
	"indicer/internal/workpool"
	"indicer/lib/dbmeta"
	"indicer/lib/dbopen"
	"indicer/lib/dbpackage"
	"indicer/lib/graph/signer"
	"indicer/lib/ingest"
	"indicer/lib/logging"
	"indicer/lib/store"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
	"go.uber.org/zap"
)

// PackageExportOptions are dues package export's flags.
type PackageExportOptions struct {
	Output         string
	SplitSizeMB    int
	Force          bool
	WithoutDerived bool
	HostInfo       bool
	Creator        duesarc.Creator
}

// PackageExport packs the database into a package.
func PackageExport(ctx context.Context, opts DBOptions, po PackageExportOptions) error {
	start := time.Now()
	res, err := dbpackage.Export(ctx, dbpackage.ExportOptions{
		Path:           opts.Path,
		Creds:          opts.Creds,
		TempDir:        opts.TempDir,
		Tuning:         opts.Tuning,
		Output:         po.Output,
		PartSize:       int64(po.SplitSizeMB) << 20,
		Force:          po.Force,
		WithoutDerived: po.WithoutDerived,
		HostInfo:       po.HostInfo,
		Creator:        po.Creator,
		Actor:          opts.Actor,
	})
	for _, n := range res.Notices {
		ui.Notef("%s", n)
	}
	for _, w := range res.Warnings {
		ui.Warnf("%s", w)
	}
	if err != nil {
		logging.GetLogger().Error("PackageExport ERROR", zap.String("root", res.Root), zap.Error(err))
		return err
	}
	took := time.Since(start)
	m := res.Manifest
	fields := []zap.Field{zap.String("root", res.Root), zap.String("output", res.Output), zap.Int("parts", len(res.Files)),
		zap.Int("files", len(m.Files)), zap.Int64("bytes", m.Bytes()), zap.Int64("package_bytes", res.Bytes),
		zap.String("content_sha256", m.ContentSHA256), zap.Int64("duration_ms", took.Milliseconds())}
	if s := m.Signature; s != nil {
		fields = append(fields, zap.String("signed_by", s.Actor), zap.String("key_id", s.KeyID))
	}
	logging.GetLogger().Info("PackageExport COMPLETE", fields...)

	w := console.Out()
	fmt.Fprintf(w, "Exported database %s to %s in %s\n", res.Root, res.Output, ui.Duration(took))
	fmt.Fprintf(w, "  %s files, %s; package %s\n", ui.Count(int64(len(m.Files))), ui.Bytes(m.Bytes()), ui.Bytes(res.Bytes))
	if len(res.Files) > 1 || len(res.Files) == 1 && res.Files[0] != res.Output {
		fmt.Fprintf(w, "  written as %d parts, described by %s\n", len(res.Files), filepath.Base(res.Output+duesarc.SidecarExt))
	}
	fmt.Fprintf(w, "  content sha256 %s\n", m.ContentSHA256)
	if s := m.Signature; s != nil {
		fmt.Fprintf(w, "  signed by actor %q, key %s\n", s.Actor, s.KeyID)
	}
	return nil
}

// LoadTrustedKey reads --trusted-key; an empty path is none.
func LoadTrustedKey(path string) (*signer.Entry, error) {
	if path == "" {
		return nil, nil
	}
	e, err := signer.ReadPublicKeyFile(path)
	if err != nil {
		return nil, lifecycle.Usagef("--trusted-key: %v", err)
	}
	return &e, nil
}

func describeSignature(w io.Writer, info *dbpackage.SignatureInfo, trustedPath string) {
	if info == nil {
		fmt.Fprintln(w, "  unsigned")
		return
	}
	fmt.Fprintf(w, "  signed by actor %q, key %s\n", termsafe.Line(info.Actor), signer.FormatID(info.KeyID))
	if info.Trusted {
		fmt.Fprintf(w, "  the key is the trusted key %s\n", trustedPath)
	} else {
		fmt.Fprintln(w, "  not checked against a trusted key: whoever changed the package could sign it again (--trusted-key checks who signed it)")
	}
	if info.Protection == signer.ProtectNone {
		ui.Warnf("the package is signed with an unprotected key: an unencrypted database keeps its actor's key as it is, so whoever could read its data root could sign as it")
	}
}

// PackageInspect describes a package from its manifest, reading nothing
// else.
func PackageInspect(path string) error {
	p, err := dbpackage.Open(path)
	if err != nil {
		return err
	}
	defer p.Close()
	describePackage(console.Out(), p)
	return nil
}

func describePackage(w io.Writer, p *dbpackage.Package) {
	m := p.Manifest
	db := m.Database
	l := db.Layout
	safe := termsafe.Line
	row := func(label, format string, a ...any) {
		fmt.Fprintf(w, "  %-10s %s\n", label, fmt.Sprintf(format, a...))
	}
	fmt.Fprintf(w, "DUES database package %s\n", p.Path)
	made := safe(m.CreatedBy.Product + " " + m.CreatedBy.Version)
	if m.CreatedBy.Commit != "" {
		made += " (" + safe(m.CreatedBy.Commit) + ")"
	}
	row("made by", "%s at %s", made, safe(m.CreatedAtUTC))
	format := fmt.Sprintf("%s format %d", safe(db.Format), db.FormatVersion)
	keys := make([]string, 0, len(db.Components))
	for k := range db.Components {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		format += fmt.Sprintf(", %s %d", safe(strings.ReplaceAll(k, "_", " ")), db.Components[k])
	}
	row("database", "%s", format)
	row("layout", "%d KiB chunks, %s, %s storage, %s blobs", l.ChunkKiB, safe(l.ChunkHash), safe(l.Storage), safe(l.BlobEncoding))
	switch {
	case !db.Encrypted:
		row("encrypted", "no")
	case len(db.PlaintextComponents) > 0:
		row("encrypted", "yes (%s); not encrypted: %s", safe(db.Encryption), safe(strings.Join(db.PlaintextComponents, ", ")))
	default:
		row("encrypted", "yes (%s)", safe(db.Encryption))
	}
	if s := m.Stats; s != nil {
		row("evidence", "%s, %s", ui.Count(int64(s.Evidence)), ui.Bytes(s.LogicalBytes))
	}
	row("contents", "%s files, %s", ui.Count(int64(len(m.Files))), ui.Bytes(m.Bytes()))
	row("content", "sha256 %s", safe(m.ContentSHA256))
	if p.Parts > 1 {
		row("parts", "%d, %s together", p.Parts, ui.Bytes(p.Size))
	} else {
		row("size", "%s", ui.Bytes(p.Size))
	}
	if h := m.Host; h != nil {
		row("host", "%s/%s %s", safe(h.OS), safe(h.Arch), safe(h.Hostname))
	}
	if s := m.Signature; s != nil {
		row("signed", "by actor %q, key %s, protected by %s (verify checks the signature)", safe(s.Actor), safe(s.KeyID), safe(s.Protection))
	} else {
		row("signed", "no")
	}
}

// PackageVerify checks a package against its manifest and signature. With
// deep, it also copies the database into the temporary directory, opens it
// read-only with opts' credentials and reads every evidence back, as repair
// --verify does.
func PackageVerify(ctx context.Context, opts DBOptions, path string, deep bool, trustedKey string) error {
	start := time.Now()
	trusted, err := LoadTrustedKey(trustedKey)
	if err != nil {
		return err
	}
	p, err := dbpackage.Open(path)
	if err != nil {
		return err
	}
	defer p.Close()
	var res dbpackage.VerifyResult
	if deep {
		res, err = p.VerifyDeep(ctx, dbpackage.DeepOptions{Creds: opts.Creds, TempDir: opts.TempDir, Tuning: opts.Tuning, TrustedKey: trusted})
	} else {
		res, err = p.Verify(ctx, dbpackage.VerifyOptions{TrustedKey: trusted})
	}
	if _, ok := errors.AsType[*dbpackage.SignatureError](err); ok {
		logging.GetLogger().Error("PackageVerify SIGNATURE", zap.String("path", p.Path), zap.Error(err))
		return fmt.Errorf("package %s: %w", p.Path, err)
	}
	for _, n := range res.Notices {
		ui.Notef("%s", n)
	}
	if bad, ok := errors.AsType[*dbpackage.DamagedError](err); ok {
		for _, pr := range bad.Problems {
			ui.Errorf("%s: %s", termsafe.Line(pr.Name), termsafe.Line(pr.Reason))
		}
		if more := bad.Count - len(bad.Problems); more > 0 {
			ui.Errorf("%d more problems", more)
		}
		logging.GetLogger().Error("PackageVerify DAMAGED", zap.String("path", p.Path), zap.Int("problems", bad.Count))
		return fmt.Errorf("package %s does not match its manifest: %s", p.Path, problems(bad.Count))
	}
	if err != nil {
		return err
	}
	if ev := res.Evidence; ev != nil {
		if err := printVerifyReport(*ev); err != nil {
			logging.GetLogger().Error("PackageVerify EVIDENCE_DAMAGED", zap.String("path", p.Path), zap.Int("verified", ev.Verified), zap.Int("damaged", ev.Problems))
			return fmt.Errorf("package %s: its files match the manifest, but %w", p.Path, err)
		}
	}
	took := time.Since(start)
	fields := []zap.Field{zap.String("path", p.Path), zap.Int("files", res.Files), zap.Int64("bytes", res.Bytes),
		zap.Bool("sidecar", res.SidecarChecked), zap.Bool("deep", deep), zap.Int64("duration_ms", took.Milliseconds())}
	if ev := res.Evidence; ev != nil {
		fields = append(fields, zap.Int("evidence_verified", ev.Verified))
	}
	if s := res.Signature; s != nil {
		fields = append(fields, zap.String("signed_by", s.Actor), zap.String("key_id", signer.FormatID(s.KeyID)), zap.Bool("trusted", s.Trusted))
	}
	logging.GetLogger().Info("PackageVerify COMPLETE", fields...)
	w := console.Out()
	fmt.Fprintf(w, "Package %s is intact: %s files, %s checked in %s\n", p.Path, ui.Count(int64(res.Files)), ui.Bytes(res.Bytes), ui.Duration(took))
	if res.SidecarChecked {
		fmt.Fprintf(w, "  its %d parts match their sidecar\n", p.Parts)
	}
	if ev := res.Evidence; ev != nil {
		fmt.Fprintf(w, "  its database opens, and %s evidence read back against their chunk hashes\n", ui.Count(int64(ev.Verified)))
	}
	fmt.Fprintf(w, "  content sha256 %s\n", termsafe.Line(p.Manifest.ContentSHA256))
	describeSignature(w, res.Signature, trustedKey)
	return nil
}

func problems(n int) string {
	if n == 1 {
		return "1 problem"
	}
	return fmt.Sprintf("%d problems", n)
}

// PackageImport makes a new database, at the --dbpath target, from a
// package.
func PackageImport(ctx context.Context, opts DBOptions, path, trustedKey string) error {
	start := time.Now()
	trusted, err := LoadTrustedKey(trustedKey)
	if err != nil {
		return err
	}
	res, err := dbpackage.Import(ctx, dbpackage.ImportOptions{Package: path, Path: opts.Path, Creds: opts.Creds, TempDir: opts.TempDir, Tuning: opts.Tuning, TrustedKey: trusted})
	for _, n := range res.Notices {
		ui.Notef("%s", n)
	}
	if bad, ok := errors.AsType[*dbpackage.DamagedError](err); ok {
		for _, pr := range bad.Problems {
			ui.Errorf("%s: %s", termsafe.Line(pr.Name), termsafe.Line(pr.Reason))
		}
		return fmt.Errorf("package %s does not match its manifest: %s; nothing was imported", path, problems(bad.Count))
	}
	if _, ok := errors.AsType[*dbpackage.SignatureError](err); ok {
		logging.GetLogger().Error("PackageImport SIGNATURE", zap.String("package", path), zap.Error(err))
		return fmt.Errorf("package %s: %w; nothing was imported", path, err)
	}
	if err != nil {
		logging.GetLogger().Error("PackageImport ERROR", zap.String("package", path), zap.String("root", res.Root), zap.Error(err))
		return err
	}
	took := time.Since(start)
	m := res.Manifest
	logging.GetLogger().Info("PackageImport COMPLETE", zap.String("package", path), zap.String("root", res.Root),
		zap.Int("files", len(m.Files)), zap.Int64("bytes", m.Bytes()), zap.Bool("opened", res.Opened),
		zap.String("content_sha256", m.ContentSHA256), zap.Int64("duration_ms", took.Milliseconds()))
	w := console.Out()
	fmt.Fprintf(w, "Imported %s as database %s in %s\n", path, res.Root, ui.Duration(took))
	fmt.Fprintf(w, "  %s files, %s, each matching the package\n", ui.Count(int64(len(m.Files))), ui.Bytes(m.Bytes()))
	fmt.Fprintf(w, "  content sha256 %s\n", termsafe.Line(m.ContentSHA256))
	describeSignature(w, res.Signature, trustedKey)
	return nil
}

// PackageMergeOptions are dues package merge's flags.
type PackageMergeOptions struct {
	TrustedKey      string
	SourceCreds     dbmeta.Credentials
	StageDir        string
	AllowPlaintext  bool
	RequireVerified bool
	Jobs            int
	// AutoJobs says Jobs was chosen for this machine, as for a store.
	AutoJobs bool
}

// PackageMerge copies the evidence of a package into the database at
// --dbpath, creating it when it is missing, and reports each evidence on
// stdout in the package's order.
func PackageMerge(ctx context.Context, opts DBOptions, path string, mo PackageMergeOptions) error {
	start := time.Now()
	trusted, err := LoadTrustedKey(mo.TrustedKey)
	if err != nil {
		return err
	}
	w := console.Out()
	res, err := dbpackage.Merge(ctx, dbpackage.MergeOptions{
		Package:         path,
		Path:            opts.Path,
		Open:            func(ctx context.Context) (dbpackage.MergeTarget, error) { return openMergeTarget(ctx, opts) },
		SourceCreds:     mo.SourceCreds,
		TargetCreds:     opts.Creds.IsSet(),
		StageDir:        mo.StageDir,
		AllowPlaintext:  mo.AllowPlaintext,
		RequireVerified: mo.RequireVerified,
		TrustedKey:      trusted,
		Jobs:            mo.Jobs,
		Fit: func(db *badger.DB, jobs int, items []workpool.Item, sizes []int64) (int, error) {
			return jobsInMemory(db, jobs, mo.AutoJobs, items, sizes, ingest.IndexedPasses)
		},
		TempDir: opts.TempDir,
		Tuning:  opts.Tuning,
		Out:     func(it dbpackage.MergeItem) { printMergeItem(w, it) },
	})
	for _, n := range res.Notices {
		ui.Notef("%s", termsafe.Line(n))
	}
	if bad, ok := errors.AsType[*dbpackage.DamagedError](err); ok {
		for _, pr := range bad.Problems {
			ui.Errorf("%s: %s", termsafe.Line(pr.Name), termsafe.Line(pr.Reason))
		}
		logging.GetLogger().Error("PackageMerge DAMAGED", zap.String("package", path), zap.Int("problems", bad.Count))
		return fmt.Errorf("package %s does not match its manifest: %s; nothing was merged", path, problems(bad.Count))
	}
	if _, ok := errors.AsType[*dbpackage.SignatureError](err); ok {
		logging.GetLogger().Error("PackageMerge SIGNATURE", zap.String("package", path), zap.Error(err))
		return fmt.Errorf("package %s: %w; nothing was merged", path, err)
	}
	took := time.Since(start)
	m := res.Manifest
	fields := []zap.Field{zap.String("package", path), zap.String("root", res.Root), zap.String("content_sha256", m.ContentSHA256),
		zap.Int("merged", res.Merged), zap.Int("aliased", res.Aliased), zap.Int("present", res.Present),
		zap.Int("skipped", res.Skipped), zap.Int("failed", res.Failed), zap.Int("warned", res.Warned),
		zap.Int64("read_bytes", res.Bytes.Read), zap.Int64("new_bytes", res.Bytes.New), zap.Int64("written_bytes", res.Bytes.Written),
		zap.Bool("created", res.Created), zap.Bool("stage_kept", res.StageKept), zap.Int64("duration_ms", took.Milliseconds())}
	if s := res.Signature; s != nil {
		fields = append(fields, zap.String("signed_by", s.Actor), zap.String("key_id", signer.FormatID(s.KeyID)), zap.Bool("trusted", s.Trusted))
	}
	if err != nil {
		logging.GetLogger().Error("PackageMerge ERROR", append(fields, zap.Error(err))...)
		return err
	}
	logging.GetLogger().Info("PackageMerge COMPLETE", fields...)

	fmt.Fprintln(w)
	fmt.Fprintf(w, "Merged package %s into database %s in %s\n", path, res.Root, ui.Duration(took))
	fmt.Fprintf(w, "  %d merged, %d aliased, %d present, %d skipped, %d failed\n", res.Merged, res.Aliased, res.Present, res.Skipped, res.Failed)
	if line := storageLine(res.Bytes); line != "" {
		fmt.Fprintln(w, " ", line)
	}
	fmt.Fprintf(w, "  content sha256 %s\n", termsafe.Line(m.ContentSHA256))
	describeSignature(w, res.Signature, mo.TrustedKey)
	if res.StageKept {
		fmt.Fprintf(w, "  copy of the package kept at %s\n", res.Stage)
	}
	switch {
	case res.Failed > 0:
		return fmt.Errorf("%d evidence of package %s did not merge", res.Failed, path)
	case res.Warned > 0:
		return &lifecycle.WarningError{Err: fmt.Errorf("%d evidence of package %s merged with warnings", res.Warned, path)}
	}
	return nil
}

// openMergeTarget opens the database a merge writes into, as a store does,
// and closes it with what a store does after it.
func openMergeTarget(ctx context.Context, opts DBOptions) (dbpackage.MergeTarget, error) {
	h, err := Common(ctx, opts, dbopen.ReadWrite, true)
	if err != nil {
		return dbpackage.MergeTarget{}, err
	}
	if err := util.EnsureBlobPath(h.Root); err != nil {
		return dbpackage.MergeTarget{}, errors.Join(err, h.Close())
	}
	if h.Created && h.Manifest.Encrypted() {
		// Common has printed the notice of one made without encryption.
		ui.Notef("created database %s", h.Root)
	}
	sess := ingest.NewSession(h.DB)
	return dbpackage.MergeTarget{Handle: h, Session: sess, Close: func() (err error) {
		compactValueLog(ctx, h, "PackageMerge")
		closeSession(sess, &err)
		closeDB(h, &err)
		return err
	}}, nil
}

// printMergeItem says how one evidence of a package ended, on one line;
// names and reasons come from the package, so they are made safe to print.
func printMergeItem(w io.Writer, it dbpackage.MergeItem) {
	id := base64.StdEncoding.EncodeToString
	name := termsafe.Line(it.Name)
	switch it.Outcome {
	case dbpackage.MergeMerged:
		fmt.Fprintf(w, "%-8s %s  %s  %s -> %s\n", it.Outcome, name, ui.Bytes(it.Size), id(it.SourceID), id(it.TargetID))
	case string(store.CommitAliased):
		fmt.Fprintf(w, "%-8s %s  %s  of %s\n", it.Outcome, name, ui.Bytes(it.Size), id(it.TargetID))
	case string(store.CommitPresent):
		fmt.Fprintf(w, "%-8s %s  %s  %s\n", it.Outcome, name, ui.Bytes(it.Size), id(it.TargetID))
	case dbpackage.MergeSkipped:
		fmt.Fprintf(w, "%-8s %s  %s\n", it.Outcome, name, termsafe.Line(it.Reason))
	default:
		fmt.Fprintf(w, "%-8s %s  %s\n", it.Outcome, name, termsafe.Line(fmt.Sprint(it.Err)))
		return
	}
	if it.Warned {
		fmt.Fprintf(w, "  warning: %s\n", termsafe.Line(it.Err.Error()))
	}
}
