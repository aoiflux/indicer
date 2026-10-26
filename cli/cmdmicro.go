package cli

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"indicer/internal/console"
	"indicer/internal/faultinject"
	"indicer/internal/lifecycle"
	"indicer/internal/termsafe"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/dbopen"
	"indicer/lib/enrichment"
	duesgraph "indicer/lib/graph"
	"indicer/lib/logging"
	"indicer/lib/microartefact"
	reppkg "indicer/lib/microartefact/repository"
	graphrepo "indicer/lib/microartefact/repository/graphene"
	"indicer/lib/store"
	"indicer/lib/structs"
	"io/fs"
	"os"
	"path"
	"time"

	"github.com/dgraph-io/badger/v4"
	"go.uber.org/zap"
)

// MicroOptions control micro-artefact extraction.
type MicroOptions struct {
	// Force extracts again from files an earlier run examined, replacing the
	// artefacts it stored.
	Force bool
	// TopK is how many indexed files, by artefact count, the HTML view shows
	// (0: all).
	TopK int
	// Out is where the HTML view is written; "" is graph.html in the working
	// directory. It used to be written into the database directory.
	Out string
	// Overwrite replaces an existing file at Out.
	Overwrite bool
}

// microReport counts what an extraction run did.
type microReport struct {
	examined, skipped, noContent, backfilled int
	byStatus                                 map[string]int
	artefacts                                int
	failures                                 []string
	failed                                   int
}

func (r *microReport) fail(what string, err error) {
	r.failed++
	if len(r.failures) < 10 {
		r.failures = append(r.failures, fmt.Sprintf("%s: %v", what, err))
	}
}

// MicroArtefactCmd extracts micro-artefacts from every indexed file of every
// completed evidence and stores them in the graph under their file:
//
// evidence_file → partition → indexed_file → micro_artefact
//
// A file an earlier run examined is skipped unless opts.Force. A file whose
// content cannot be read or stored is counted, and the command exits 2; it
// used to log the failure and exit 0 (GRA-12).
func MicroArtefactCmd(ctx context.Context, opts DBOptions, mo MicroOptions) (err error) {
	start := time.Now()
	out := mo.Out
	if out == "" {
		out = "graph.html"
	}
	if mo.TopK < 0 {
		return lifecycle.Usagef("--top-k must be 0 or more")
	}
	if !mo.Overwrite {
		if _, serr := os.Stat(out); serr == nil {
			return lifecycle.Usagef("%s exists; pass --overwrite to replace it, or --out to write elsewhere", out)
		} else if !errors.Is(serr, fs.ErrNotExist) {
			return serr
		}
	}
	logging.GetLogger().Info("MicroArtefactCmd START",
		zap.String("db_path", opts.Path),
		zap.Bool("force", mo.Force),
		zap.Int("top_k", mo.TopK),
		zap.String("out", out),
	)

	h, err := Common(ctx, opts, dbopen.ReadWrite, true)
	if err != nil {
		logging.GetLogger().Error("MicroArtefactCmd DB_CONNECT_ERROR", zap.Error(err))
		return err
	}
	defer closeDB(h, &err)
	db := h.DB

	repo, err := microartefact.OpenGrapheneRepository(h.Root)
	if err != nil {
		logging.GetLogger().Error("MicroArtefactCmd OPEN_GRAPH_REPOSITORY_ERROR", zap.Error(err))
		return err
	}
	service := microartefact.NewService(repo)
	defer func() {
		if cerr := service.Close(); err == nil {
			err = cerr
		}
	}()

	rep := &microReport{byStatus: map[string]int{}}
	if err := extractAll(ctx, db, service, repo, mo.Force, rep); err != nil {
		logging.GetLogger().Error("MicroArtefactCmd EXTRACT_ERROR", zap.Error(err))
		return err
	}

	console.Printf("[micro] %d file(s) examined: %d with artefacts (%d artefacts), %d without, %d unsupported, %d parse failure(s), %d too large; %d skipped (examined before; --force extracts again), %d without content.\n",
		rep.examined, rep.byStatus[reppkg.StatusExtracted], rep.artefacts, rep.byStatus[reppkg.StatusNone],
		rep.byStatus[reppkg.StatusUnsupported], rep.byStatus[reppkg.StatusParseFailed], rep.byStatus[reppkg.StatusTooLarge],
		rep.skipped, rep.noContent)
	if rep.backfilled > 0 {
		console.Printf("[micro] %d evidence had no file nodes in the graph; enriched them first.\n", rep.backfilled)
	}

	// The files with the most artefacts, not the whole graph: a store can
	// hold millions of artefacts (GRA-17). `dues micro list` lists them all.
	top, err := repo.TopFiles(ctx, cliTopFiles, cliTopFiles)
	if err != nil {
		logging.GetLogger().Error("MicroArtefactCmd TOP_FILES_ERROR", zap.Error(err))
		return err
	}
	shown, err := repo.ExportHTML(out, mo.TopK, mo.Overwrite)
	switch {
	case errors.Is(err, graphrepo.ErrNothingToExport):
		console.Println("[micro] No indexed file has micro-artefacts; no HTML view written.")
	case errors.Is(err, fs.ErrExist):
		return lifecycle.Usagef("%s exists; pass --overwrite to replace it, or --out to write elsewhere", out)
	case err != nil:
		logging.GetLogger().Error("MicroArtefactCmd EXPORT_HTML_ERROR", zap.Error(err), zap.String("file_path", out))
		return err
	default:
		printTopFiles(top)
		console.Printf("HTML graph visualisation (%d file(s)): %s\n", shown, out)
	}
	logging.GetLogger().Info("MicroArtefactCmd COMPLETE",
		zap.Int64("duration_ms", time.Since(start).Milliseconds()),
		zap.Int("failed", rep.failed),
	)
	if rep.failed > 0 {
		for _, f := range rep.failures {
			console.Errorf("failed: %s\n", termsafe.Line(f))
		}
		return &lifecycle.WarningError{Err: fmt.Errorf("micro-artefacts could not be extracted from %d file(s)", rep.failed)}
	}
	return nil
}

// extractAll extracts from every completed evidence in turn. Evidence whose
// index is quarantined is skipped, as list and search skip it.
func extractAll(ctx context.Context, db *badger.DB, service *microartefact.Service, repo *microartefact.GrapheneRepository, force bool, rep *microReport) error {
	var ids [][]byte
	err := db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = false
		prefix := []byte(cnst.EviFileNamespace)
		it := txn.NewIterator(opts)
		defer it.Close()
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			ids = append(ids, dbio.EvidenceRawID(it.Item().KeyCopy(nil)))
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, eid := range ids {
		if err := ctx.Err(); err != nil {
			return context.Cause(ctx)
		}
		e, err := dbio.GetEvidenceFile(eid, db)
		if err != nil {
			return err
		}
		if !e.Completed || (e.Index != nil && e.Index.State == structs.IndexInvalid) {
			continue
		}
		if err := extractEvidence(ctx, db, service, repo, eid, e, force, rep); err != nil {
			return err
		}
	}
	return nil
}

func extractEvidence(ctx context.Context, db *badger.DB, service *microartefact.Service, repo *microartefact.GrapheneRepository, eid []byte, e structs.EvidenceFile, force bool, rep *microReport) error {
	evidenceID := base64.StdEncoding.EncodeToString(eid)
	type member struct{ pid, pname, iid string }
	var members []member
	err := db.View(func(txn *badger.Txn) error {
		for pidB64 := range e.InternalObjects {
			pid, err := base64.StdEncoding.DecodeString(pidB64)
			if err != nil {
				return fmt.Errorf("partition %s: %w", pidB64, err)
			}
			var pdata structs.PartitionFile
			if err := dbio.GetRecordTxn(txn, dbio.CanonicalPartitionKey(pid), &pdata); err != nil {
				return fmt.Errorf("partition %s: %w", pidB64, err)
			}
			err = dbio.ForEachPartitionMemberTxn(txn, pid, pdata, func(iid string, _ structs.InternalOffset) error {
				members = append(members, member{pid: pidB64, pname: pdata.Name, iid: iid})
				return nil
			})
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	backfilled := false
	for _, m := range members {
		if err := ctx.Err(); err != nil {
			return context.Cause(ctx)
		}
		// The graph holds the evidence's files under their record IDs, as
		// enrichment writes them (GRA-1). An evidence never enriched is
		// enriched once, before its first file.
		has, err := repo.HasIndexedFile(m.iid)
		if err != nil {
			return err
		}
		if !has && !backfilled {
			backfilled = true
			if err := backfillEvidence(ctx, db, eid); err != nil {
				return err
			}
			rep.backfilled++
			if has, err = repo.HasIndexedFile(m.iid); err != nil {
				return err
			}
		}
		if !has {
			rep.fail(m.iid, errors.New("no indexed-file node in the graph after enrichment"))
			continue
		}
		if err := extractFile(ctx, db, service, repo, evidenceID, e.Name, m.pid, m.pname, m.iid, force, rep); err != nil {
			return err
		}
	}
	return nil
}

// backfillEvidence writes an evidence's hierarchy to the graph. Entropy it
// cannot compute does not stop extraction.
func backfillEvidence(ctx context.Context, db *badger.DB, eid []byte) (err error) {
	enrichRepo, err := enrichment.OpenGrapheneRepository(dbio.KVDir(db))
	if err != nil {
		return err
	}
	svc := enrichment.NewService(db, enrichRepo)
	defer func() {
		if cerr := svc.Close(); err == nil {
			err = cerr
		}
	}()
	var incomplete *enrichment.IncompleteError
	if _, err := svc.EnrichEvidenceIndex(ctx, eid); err != nil && !errors.As(err, &incomplete) {
		return err
	}
	return nil
}

// extractFile extracts from one indexed file. Only a database or graph error
// stops the run; a file that cannot be read or stored is counted.
func extractFile(ctx context.Context, db *badger.DB, service *microartefact.Service, repo *microartefact.GrapheneRepository, evidenceID, evidenceName, pid, pname, iidB64 string, force bool, rep *microReport) error {
	if err := faultinject.Hit(faultinject.MicroMid); err != nil {
		return err
	}
	raw, err := base64.StdEncoding.DecodeString(iidB64)
	if err != nil {
		rep.fail(iidB64, err)
		return nil
	}
	key := dbio.CanonicalIndexedKey(raw)
	ifile, err := dbio.GetIndexedFile(key, db)
	if err != nil {
		rep.fail(iidB64, err)
		return nil
	}
	filePath := dbio.RecordPath(ifile)
	if !force {
		status, err := repo.ExtractionStatus(iidB64)
		if err != nil {
			return err
		}
		if status != "" {
			rep.skipped++
			return nil
		}
	}

	content, size, err := readIndexedFileContent(ctx, db, key, microartefact.RawLimit)
	switch {
	case errors.Is(err, dbio.ErrContentUnavailable):
		rep.noContent++
		return nil
	case err != nil && ctx.Err() != nil:
		return context.Cause(ctx)
	case err != nil:
		rep.fail(filePath, err)
		return nil
	}

	record := microartefact.FileRecord{
		Hash:             iidB64,
		Name:             path.Base(filePath),
		Path:             filePath,
		Size:             size,
		IsDeleted:        ifile.IsDeleted,
		IsFragmented:     ifile.IsFragmented,
		FileType:         ifile.IndexedType,
		EvidenceFileID:   evidenceID,
		EvidenceFileName: evidenceName,
		PartitionID:      pid,
		PartitionName:    pname,
		IndexedFileID:    iidB64,
	}
	outcome, err := service.ProcessFile(record, content, size, force)
	if err != nil {
		rep.fail(filePath, err)
		return nil
	}
	rep.examined++
	rep.byStatus[outcome.Status]++
	rep.artefacts += outcome.Artefacts
	if outcome.Artefacts > 0 {
		console.Printf("Micro-artefacts extracted: %s (%d)\n", termsafe.Line(filePath), outcome.Artefacts)
	}
	return nil
}

// readIndexedFileContent reads up to limit bytes of an indexed file through
// the reader, which verifies each chunk and serves every content kind. It
// returns the bytes and the file's size.
func readIndexedFileContent(ctx context.Context, db *badger.DB, key []byte, limit int64) ([]byte, int64, error) {
	lg, err := dbio.OpenLogical(db, key, dbio.ReadOptions{})
	if err != nil {
		return nil, 0, err
	}
	n := min(lg.Size(), limit)
	buf := make([]byte, 0, n)
	err = lg.Stream(ctx, func(p []byte) error {
		if room := n - int64(len(buf)); int64(len(p)) > room {
			p = p[:room]
		}
		buf = append(buf, p...)
		if int64(len(buf)) >= n {
			return errMicroLimit
		}
		return nil
	})
	if errors.Is(err, errMicroLimit) {
		err = nil
	}
	return buf, lg.Size(), err
}

// errMicroLimit stops a read once the bytes extraction needs are read.
var errMicroLimit = errors.New("read limit reached")

// ─── CLI visualisation tree ───────────────────────────────────────────────────

// cliTopFiles is how many files, and artefacts of each, micro extract shows.
const cliTopFiles = 20

// fileArtefacts is how many artefacts a file holds, shown or not.
func fileArtefacts(f *microartefact.IndexedFileArtefactView) int {
	return max(f.ArtefactCount, len(f.Artefacts))
}

// printTopFiles renders, after extraction, the files with the most
// micro-artefacts as an ASCII tree, and says how many more there are.
func printTopFiles(top *graphrepo.TopFiles) {
	if top == nil || top.Tree == nil || len(top.Tree.EvidenceFiles) == 0 {
		console.Println("\n  (no micro-artefacts found)")
		return
	}
	tree := top.Tree
	console.Printf("\n🔬 Micro-artefacts: %d file(s) hold %d artefact(s); the %d with the most:\n\n", top.Files, top.Artefacts, top.Shown)
	defer func() {
		if more := top.Files - top.Shown; more > 0 {
			console.Printf("\n… and %d more file(s) with artefacts; `dues micro list` lists them all.\n", more)
		}
	}()

	for di, disk := range tree.EvidenceFiles {
		isLastDisk := di == len(tree.EvidenceFiles)-1
		totalForDisk := 0
		for _, p := range disk.Partitions {
			for _, f := range p.Files {
				totalForDisk += fileArtefacts(f)
			}
		}

		diskName := disk.Name
		if diskName == "" {
			diskName = disk.ID
		}
		console.Printf("🖥  %s  [%d artefact(s)]\n", termsafe.Line(diskName), totalForDisk)

		for pi, part := range disk.Partitions {
			isLastPart := pi == len(disk.Partitions)-1
			partPfx := cliLevelPfx(isLastDisk)
			console.Printf("%s%s🗂  %s\n", partPfx, cliBranch(isLastPart), termsafe.Line(partDisplayNameCLI(part.Name, part.ID)))

			for fi, file := range part.Files {
				isLastFile := fi == len(part.Files)-1
				filePfx := partPfx + cliLevelPfx(isLastPart)
				console.Printf("%s%s📄 %s  (%s · %d artefact(s))\n",
					filePfx, cliBranch(isLastFile), termsafe.Line(file.Name),
					humanReadableSize(file.Size), fileArtefacts(file))

				grouped := cliGroupByKind(file.Artefacts)
				kinds := cliSortedKinds(grouped)
				for ki, kind := range kinds {
					isLastKind := ki == len(kinds)-1
					kindPfx := filePfx + cliLevelPfx(isLastFile)
					arts := grouped[kind]
					console.Printf("%s%s%s %s  ×%d\n", kindPfx, cliBranch(isLastKind), cliKindIcon(kind), kind, len(arts))

					limit := len(arts)
					truncated := 0
					if limit > 3 {
						truncated = limit - 3
						limit = 3
					}
					valPfx := kindPfx + cliLevelPfx(isLastKind)
					for vi, a := range arts[:limit] {
						isLastVal := vi == limit-1 && truncated == 0
						val := a.Value
						if len(val) > 100 {
							val = val[:99] + "…"
						}
						// Artefact values are strings found in evidence content.
						console.Printf("%s%s%s\n", valPfx, cliBranch(isLastVal), termsafe.Line(val))
					}
					if truncated > 0 {
						console.Printf("%s└─ … %d more\n", valPfx, truncated)
					}
				}
			}
		}

		if !isLastDisk {
			console.Println()
		}
	}
	console.Println()
}

func cliLevelPfx(isLast bool) string {
	if isLast {
		return "    "
	}
	return "│   "
}

func cliBranch(isLast bool) string {
	if isLast {
		return "└─ "
	}
	return "├─ "
}

func partDisplayNameCLI(name, id string) string {
	if name != "" && name != id {
		return name
	}
	if id != "" {
		return id
	}
	return "(unknown)"
}

func cliKindIcon(kind string) string {
	switch kind {
	case "url":
		return "🌐"
	case "key_value":
		return "🔑"
	case "ioc":
		return "⚠️ "
	case "hash", "file_hash":
		return "🔢"
	case "pe_metadata":
		return "⚙️ "
	case "registry_hive":
		return "🗃️ "
	case "evtx":
		return "📋"
	case "scheduled_task":
		return "⏰"
	case "service":
		return "🔧"
	case "browser":
		return "🌍"
	case "log_line":
		return "📜"
	case "windows_event":
		return "🪟"
	default:
		return "◈ "
	}
}

func cliGroupByKind(arts []*microartefact.MicroArtefactNode) map[string][]*microartefact.MicroArtefactNode {
	m := make(map[string][]*microartefact.MicroArtefactNode)
	for _, a := range arts {
		m[a.Kind] = append(m[a.Kind], a)
	}
	return m
}

var cliKindPriority = map[string]int{
	"ioc": 0, "url": 1, "key_value": 2, "hash": 3, "file_hash": 4,
	"pe_metadata": 5, "evtx": 6, "windows_event": 7, "registry_hive": 8,
	"scheduled_task": 9, "service": 10, "browser": 11,
	"log_line": 12,
}

func cliSortedKinds(m map[string][]*microartefact.MicroArtefactNode) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0; j-- {
			pi, oki := cliKindPriority[keys[j]]
			pj, okj := cliKindPriority[keys[j-1]]
			swap := false
			if oki && okj {
				swap = pi < pj
			} else if oki {
				swap = true
			} else if !okj {
				swap = keys[j] < keys[j-1]
			}
			if !swap {
				break
			}
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

func humanReadableSize(n int64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
	)
	switch {
	case n >= GB:
		return fmt.Sprintf("%.1f GB", float64(n)/GB)
	case n >= MB:
		return fmt.Sprintf("%.1f MB", float64(n)/MB)
	case n >= KB:
		return fmt.Sprintf("%.1f kB", float64(n)/KB)
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// ListMicroArtefactsCmd lists all micro-artefacts in JSON format
func ListMicroArtefactsCmd(ctx context.Context, opts DBOptions) (err error) {
	h, err := Common(ctx, opts, dbopen.ReadOnly, false)
	if err != nil {
		return err
	}
	defer closeDB(h, &err)

	repo, err := microartefact.OpenGrapheneRepositoryReadOnly(h.Root)
	if errors.Is(err, duesgraph.ErrNoGraph) {
		console.Errorln("no micro-artefacts: this database has no graph store; run 'dues micro extract' first")
		return store.PrintMicroArtefacts(&microartefact.EvidenceFileHierarchy{})
	}
	if err != nil {
		return err
	}
	defer func() {
		if cerr := repo.Close(); err == nil {
			err = cerr
		}
	}()

	return store.ListMicroArtefacts(repo)
}
