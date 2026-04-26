package cli

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/microartefact"
	"indicer/lib/store"
	"indicer/lib/structs"
	"indicer/lib/util"
	"path/filepath"
	"strings"

	"github.com/dgraph-io/badger/v4"
	"github.com/vmihailenco/msgpack/v5"
)

// microScanLimit matches the Service's defaultScanLimit so we never stream
// more bytes than the detectors will actually examine.
const microScanLimit = 4 << 20 // 4 MB

// errScanLimitReached is a sentinel used to stop streaming once we have
// collected enough bytes for the scan window.
var errScanLimitReached = errors.New("scan limit reached")

// MicroArtefactCmd opens the DUES database and extracts micro-artefacts from
// every indexed file that has been stored in the hierarchy:
//
// disk_image → partition → indexed_file → micro_artefact
func MicroArtefactCmd(chonkSize int, dbpath string, key []byte, force bool, topK int) error {
	db, dbpath, err := Common(chonkSize, dbpath, key)
	if err != nil {
		return err
	}
	defer db.Close()

	repo, err := microartefact.OpenGrapheneRepository(dbpath)
	if err != nil {
		return err
	}

	service := microartefact.NewService(repo)
	defer service.Close()

	if err := processAllIndexedFiles(db, service, repo, force); err != nil {
		return err
	}

	tree, err := repo.ReadHierarchy()
	if err != nil {
		return err
	}

	htmlPath := filepath.Join(dbpath, "graph.html")
	if err := repo.ExportInteractiveHTMLTopK(htmlPath, topK); err != nil {
		return err
	}

	printHierarchyTree(tree)
	fmt.Printf("HTML graph visualisation: %s\n", htmlPath)
	return nil
}

// processAllIndexedFiles iterates all completed evidence files in the store and
// processes each indexed file within them for micro-artefact extraction.
func processAllIndexedFiles(db *badger.DB, service *microartefact.Service, repo *microartefact.GrapheneRepository, force bool) error {
	return db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchSize = 100
		it := txn.NewIterator(opts)
		defer it.Close()

		eviPrefix := []byte(cnst.EviFileNamespace)
		for it.Seek(eviPrefix); it.ValidForPrefix(eviPrefix); it.Next() {
			item := it.Item()
			k := item.KeyCopy(nil)
			v, err := item.ValueCopy(nil)
			if err != nil {
				return err
			}

			decoded, err := cnst.DECODER.DecodeAll(v, nil)
			if err == nil {
				v = decoded
			}

			var evidata structs.EvidenceFile
			if err := msgpack.Unmarshal(v, &evidata); err != nil {
				return err
			}
			if !evidata.Completed {
				continue
			}

			eviHash := bytes.TrimPrefix(k, eviPrefix)
			eviHashB64 := base64.StdEncoding.EncodeToString(eviHash)
			eviName := arbitrarySetKey(evidata.Names)

			for phashB64 := range evidata.InternalObjects {
				if err := processPartition(txn, db, service, repo, force, eviHashB64, eviName, phashB64); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// processPartition retrieves a partition file from the store and processes each
// indexed file it contains.
func processPartition(txn *badger.Txn, db *badger.DB, service *microartefact.Service, repo *microartefact.GrapheneRepository, force bool, eviHashB64, eviName, phashB64 string) error {
	rawPhash, err := base64.StdEncoding.DecodeString(phashB64)
	if err != nil {
		return fmt.Errorf("partition hash decode: %w", err)
	}
	pid := util.AppendToBytesSlice(cnst.PartiFileNamespace, rawPhash)

	item, err := txn.Get(pid)
	if err != nil {
		return fmt.Errorf("get partition %s: %w", phashB64, err)
	}
	v, err := item.ValueCopy(nil)
	if err != nil {
		return err
	}
	decoded, err := cnst.DECODER.DecodeAll(v, nil)
	if err == nil {
		v = decoded
	}

	var pdata structs.PartitionFile
	if err := msgpack.Unmarshal(v, &pdata); err != nil {
		return err
	}
	partiName := arbitrarySetKey(pdata.Names)

	for ihashB64 := range pdata.InternalObjects {
		if err := processIndexedFile(db, service, repo, force, eviHashB64, eviName, phashB64, partiName, ihashB64); err != nil {
			// Non-fatal: log and continue so one bad file does not abort the run.
			fmt.Printf("warning: skipping indexed file %s: %v\n", ihashB64, err)
		}
	}
	return nil
}

// processIndexedFile reads one indexed file from the store and runs micro-artefact
// detection on its content, using the real hierarchy IDs for the graph record.
func processIndexedFile(db *badger.DB, service *microartefact.Service, repo *microartefact.GrapheneRepository, force bool, eviHashB64, eviName, partiHashB64, partiName, ihashB64 string) error {
	if !force {
		exists, err := repo.HasIndexedFile(ihashB64)
		if err != nil {
			return fmt.Errorf("check indexed file extraction status: %w", err)
		}
		if exists {
			fmt.Printf("Skipping already-extracted indexed file: %s\n", ihashB64)
			return nil
		}
	}

	rawIhash, err := base64.StdEncoding.DecodeString(ihashB64)
	if err != nil {
		return fmt.Errorf("indexed file hash decode: %w", err)
	}
	iid := util.AppendToBytesSlice(cnst.IdxFileNamespace, rawIhash)

	ifile, err := dbio.GetIndexedFile(iid, db)
	if err != nil {
		return fmt.Errorf("get indexed file: %w", err)
	}

	name, path := resolveIndexedFileName(ifile.Names)

	record := microartefact.FileRecord{
		Hash:          ihashB64,
		Name:          name,
		Path:          path,
		Size:          ifile.Size,
		FileType:      ifile.IndexedType,
		DiskImageID:   eviHashB64,
		DiskImageName: eviName,
		PartitionID:   partiHashB64,
		PartitionName: partiName,
		IndexedFileID: ihashB64,
	}

	content, err := readIndexedFileContent(iid, db)
	if err != nil {
		return fmt.Errorf("read indexed file content: %w", err)
	}

	if err := service.Process(record, content); err != nil {
		return fmt.Errorf("micro-artefact process: %w", err)
	}

	fmt.Printf("Micro-artefacts extracted: %s\n", name)
	return nil
}

// readIndexedFileContent streams up to microScanLimit bytes of the indexed file
// from the store without writing the full file to disk.
func readIndexedFileContent(iid []byte, db *badger.DB) ([]byte, error) {
	buf := make([]byte, 0, microScanLimit)
	err := dbio.StreamLogicalFileBytes(iid, db, func(chunk []byte) error {
		remaining := microScanLimit - len(buf)
		if remaining <= 0 {
			return errScanLimitReached
		}
		if len(chunk) > remaining {
			chunk = chunk[:remaining]
		}
		buf = append(buf, chunk...)
		return nil
	})
	if errors.Is(err, errScanLimitReached) {
		return buf, nil
	}
	return buf, err
}

// resolveIndexedFileName extracts a human-readable name and path from the
// IndexedFile.Names set. Names that follow the "hash|||evi_path|||file_name"
// convention are split accordingly; otherwise the raw entry is used for both.
func resolveIndexedFileName(names map[string]struct{}) (name, path string) {
	for n := range names {
		if strings.Contains(n, cnst.DataSeperator) {
			parts := strings.SplitN(n, cnst.DataSeperator, 3)
			if len(parts) == 3 {
				return parts[2], n
			}
		}
		name = n
		path = n
	}
	return name, path
}

// arbitrarySetKey returns any key from the map (used for display names).
func arbitrarySetKey(m map[string]struct{}) string {
	for k := range m {
		return k
	}
	return ""
}

// ─── CLI visualisation tree ───────────────────────────────────────────────────

// printHierarchyTree renders the micro-artefact hierarchy to stdout as an
// ASCII tree after extraction completes.
func printHierarchyTree(tree *microartefact.HierarchyTree) {
	if tree == nil || len(tree.DiskImages) == 0 {
		fmt.Println("\n  (no micro-artefacts found — run 'dues microartefacts' on a populated database)")
		return
	}

	fmt.Printf("\n🔬 Micro-Artefact Graph  (%d disk image(s) · %d artefact(s) · %d relation(s): %d deterministic, %d probabilistic)\n\n",
		len(tree.DiskImages), tree.TotalArtefacts, tree.TotalRelations, tree.DeterministicRelations, tree.ProbabilisticRelations)

	for di, disk := range tree.DiskImages {
		isLastDisk := di == len(tree.DiskImages)-1
		totalForDisk := 0
		for _, p := range disk.Partitions {
			for _, f := range p.Files {
				totalForDisk += len(f.Artefacts)
			}
		}

		diskName := disk.Name
		if diskName == "" {
			diskName = disk.ID
		}
		fmt.Printf("🖥  %s  [%d artefact(s)]\n", diskName, totalForDisk)

		for pi, part := range disk.Partitions {
			isLastPart := pi == len(disk.Partitions)-1
			partPfx := cliLevelPfx(isLastDisk)
			fmt.Printf("%s%s🗂  %s\n", partPfx, cliBranch(isLastPart), partDisplayNameCLI(part.Name, part.ID))

			for fi, file := range part.Files {
				isLastFile := fi == len(part.Files)-1
				filePfx := partPfx + cliLevelPfx(isLastPart)
				fmt.Printf("%s%s📄 %s  (%s · %d artefact(s))\n",
					filePfx, cliBranch(isLastFile), file.Name,
					humanReadableSize(file.Size), len(file.Artefacts))

				grouped := cliGroupByKind(file.Artefacts)
				kinds := cliSortedKinds(grouped)
				for ki, kind := range kinds {
					isLastKind := ki == len(kinds)-1
					kindPfx := filePfx + cliLevelPfx(isLastFile)
					arts := grouped[kind]
					fmt.Printf("%s%s%s %s  ×%d\n", kindPfx, cliBranch(isLastKind), cliKindIcon(kind), kind, len(arts))

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
						fmt.Printf("%s%s%s\n", valPfx, cliBranch(isLastVal), val)
					}
					if truncated > 0 {
						fmt.Printf("%s└─ … %d more\n", valPfx, truncated)
					}
				}
			}
		}

		if !isLastDisk {
			fmt.Println()
		}
	}
	fmt.Println()
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

func cliGroupByKind(arts []*microartefact.ArtefactNode) map[string][]*microartefact.ArtefactNode {
	m := make(map[string][]*microartefact.ArtefactNode)
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

func cliSortedKinds(m map[string][]*microartefact.ArtefactNode) []string {
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
func ListMicroArtefactsCmd(chonkSize int, dbpath string, key []byte) error {
	_, dbpath, err := Common(chonkSize, dbpath, key)
	if err != nil {
		return err
	}

	repo, err := microartefact.OpenGrapheneRepository(dbpath)
	if err != nil {
		return err
	}
	defer repo.Close()

	return store.ListMicroArtefacts(repo)
}
