package store

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"indicer/lib/cnst"
	"indicer/lib/fio"
	"indicer/lib/structs"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
	"github.com/dustin/go-humanize"
	"github.com/fatih/color"
	"github.com/vmihailenco/msgpack/v5"
)

type DBStats struct {
	TotalFiles             int64
	CompletedFiles         int64
	TotalLogicalSize       int64
	TotalPartitions        int64
	TotalIndexedFiles      int64
	TotalIndexedNames      int64
	DeletedIndexedFiles    int64
	FragmentedIndexedFiles int64
	DeletedIndexedNames    int64
	FragmentedIndexedNames int64
	UniqueChunks           int64
	TotalChunkRefs         int64
	SharedChunks           int64 // chunk positions referenced by more than 1 file
	OnDiskBytes            int64
}

func Stats(db *badger.DB) error {
	s, err := gatherStats(db)
	if err != nil {
		return err
	}
	printStats(s)
	return nil
}

func GatherStats(db *badger.DB) (*DBStats, error) {
	return gatherStats(db)
}

func gatherStats(db *badger.DB) (*DBStats, error) {
	s := &DBStats{}

	err := db.View(func(txn *badger.Txn) error {
		valOpts := badger.DefaultIteratorOptions
		valOpts.PrefetchSize = 1000

		keyOpts := badger.DefaultIteratorOptions
		keyOpts.PrefetchValues = false
		keyOpts.PrefetchSize = 1000

		// --- Evidence files (need values to get size + completed flag) ---
		eviPrefix := []byte(cnst.EviFileNamespace)
		it := txn.NewIterator(valOpts)
		for it.Seek(eviPrefix); it.ValidForPrefix(eviPrefix); it.Next() {
			v, err := it.Item().ValueCopy(nil)
			if err != nil {
				it.Close()
				return err
			}
			if dec, err := cnst.DECODER.DecodeAll(v, nil); err == nil {
				v = dec
			}
			var ef structs.EvidenceFile
			if err := msgpack.Unmarshal(v, &ef); err != nil {
				it.Close()
				return err
			}
			s.TotalFiles++
			if ef.Completed {
				s.CompletedFiles++
				s.TotalLogicalSize += ef.Size
			}
		}
		it.Close()

		// --- Partitions (keys only) ---
		partiPrefix := []byte(cnst.PartiFileNamespace)
		it = txn.NewIterator(keyOpts)
		for it.Seek(partiPrefix); it.ValidForPrefix(partiPrefix); it.Next() {
			s.TotalPartitions++
		}
		it.Close()

		// --- Indexed files (values for metadata-aware stats) ---
		idxPrefix := []byte(cnst.IdxFileNamespace)
		it = txn.NewIterator(valOpts)
		for it.Seek(idxPrefix); it.ValidForPrefix(idxPrefix); it.Next() {
			v, err := it.Item().ValueCopy(nil)
			if err != nil {
				it.Close()
				return err
			}
			if dec, err := cnst.DECODER.DecodeAll(v, nil); err == nil {
				v = dec
			}

			var ifile structs.IndexedFile
			if err := msgpack.Unmarshal(v, &ifile); err != nil {
				it.Close()
				return err
			}

			s.TotalIndexedFiles++
			if ifile.Name != "" {
				s.TotalIndexedNames++
			}

			objDeleted := ifile.IsDeleted
			var objFragmented bool
			if ifile.IsDeleted {
				s.DeletedIndexedNames++
				objDeleted = true
			}
			if ifile.IsFragmented {
				s.FragmentedIndexedNames++
				objFragmented = true
			}

			if objDeleted {
				s.DeletedIndexedFiles++
			}
			if objFragmented {
				s.FragmentedIndexedFiles++
			}
		}
		it.Close()

		// --- Unique chunks (keys only) ---
		chonkPrefix := []byte(cnst.ChonkNamespace)
		it = txn.NewIterator(keyOpts)
		for it.Seek(chonkPrefix); it.ValidForPrefix(chonkPrefix); it.Next() {
			s.UniqueChunks++
		}
		it.Close()

		// --- Total chunk references / file→chunk mappings (keys only) ---
		relPrefix := []byte(cnst.RelationNamespace)
		it = txn.NewIterator(keyOpts)
		for it.Seek(relPrefix); it.ValidForPrefix(relPrefix); it.Next() {
			s.TotalChunkRefs++
		}
		it.Close()

		// --- Shared chunks: logical rev-rel entries referenced by more than 1 file ---
		sharedByRelation := make(map[string]map[string]struct{})

		appendPrefix := []byte(cnst.ReverseRelationAppendNamespace)
		it = txn.NewIterator(valOpts)
		for it.Seek(appendPrefix); it.ValidForPrefix(appendPrefix); it.Next() {
			memberKey := it.Item().KeyCopy(nil)
			split := bytes.Split(memberKey, []byte(cnst.DataSeperator))
			if len(split) < 4 {
				it.Close()
				return fmt.Errorf("invalid reverse-relation append key: %q", string(memberKey))
			}
			chash := bytes.TrimPrefix(split[1], []byte(":"))
			logicalKey := string(util.AppendToBytesSlice(cnst.ReverseRelationNamespace, chash, cnst.DataSeperator, split[2]))

			v, err := it.Item().ValueCopy(nil)
			if err != nil {
				it.Close()
				return err
			}
			if dec, err := cnst.DECODER.DecodeAll(v, nil); err == nil {
				v = dec
			}

			memberSet := sharedByRelation[logicalKey]
			if memberSet == nil {
				memberSet = make(map[string]struct{}, 1)
				sharedByRelation[logicalKey] = memberSet
			}
			memberSet[string(v)] = struct{}{}
		}
		it.Close()

		for _, memberSet := range sharedByRelation {
			if len(memberSet) > 1 {
				s.SharedChunks++
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	s.OnDiskBytes = calcBlobDirBytes(db.Opts().Dir)
	return s, nil
}

func calcBlobDirBytes(dbDir string) int64 {
	var total int64
	blobsDir := util.BlobPath(dbDir)
	_ = filepath.Walk(blobsDir, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}

func printStats(s *DBStats) {
	sep := strings.Repeat("─", 46)
	dim := color.New(color.FgHiBlack)
	sectionHdr := color.New(color.FgMagenta, color.Bold)
	lbl := color.New(color.FgWhite)
	val := color.New(color.FgYellow, color.Bold)
	good := color.New(color.FgGreen, color.Bold)
	warn := color.New(color.FgRed)

	statRow := func(label string, formatted string, c *color.Color) {
		lbl.Printf("    %-30s", label+":")
		c.Println(formatted)
	}

	color.New(color.FgCyan, color.Bold).Println()
	color.New(color.FgCyan, color.Bold).Println("  ╔══════════════════════════════════════════════╗")
	color.New(color.FgCyan, color.Bold).Println("  ║       DUES  Database  Statistics             ║")
	color.New(color.FgCyan, color.Bold).Println("  ╚══════════════════════════════════════════════╝")

	// ── FILES ──────────────────────────────────────────────────────────────
	fmt.Println()
	sectionHdr.Println("  FILES")
	fmt.Println("  ", dim.Sprint(sep))

	statRow("Total files", humanize.Comma(s.TotalFiles), val)
	statRow("Completed", humanize.Comma(s.CompletedFiles), good)
	if inc := s.TotalFiles - s.CompletedFiles; inc > 0 {
		statRow("Incomplete", humanize.Comma(inc), warn)
	}
	statRow("Total logical size", humanize.Bytes(uint64(s.TotalLogicalSize)), val)
	if s.CompletedFiles > 0 {
		avg := uint64(s.TotalLogicalSize) / uint64(s.CompletedFiles)
		statRow("Avg file size", humanize.Bytes(avg), val)
	}

	// ── CHUNKS ─────────────────────────────────────────────────────────────
	fmt.Println()
	sectionHdr.Println("  CHUNKS")
	fmt.Println("  ", dim.Sprint(sep))

	statRow("Unique chunks stored", humanize.Comma(s.UniqueChunks), val)
	statRow("Total chunk references", humanize.Comma(s.TotalChunkRefs), val)
	if s.CompletedFiles > 0 {
		avgChunks := float64(s.TotalChunkRefs) / float64(s.CompletedFiles)
		statRow("Avg chunks per file", fmt.Sprintf("%.1f", avgChunks), val)
	}
	if s.TotalChunkRefs > 0 && s.TotalLogicalSize > 0 {
		avgSize := uint64(s.TotalLogicalSize) / uint64(s.TotalChunkRefs)
		statRow("Avg chunk size", humanize.Bytes(avgSize), val)
	}
	statRow("Configured chunk size", humanize.Bytes(uint64(cnst.ChonkSize)), dim)

	if s.SharedChunks > 0 {
		statRow("Chunks shared across files", humanize.Comma(s.SharedChunks), good)
	} else {
		statRow("Chunks shared across files", "0 (no cross-file duplicates)", dim)
	}

	deduped := s.TotalChunkRefs - s.UniqueChunks
	if deduped > 0 {
		statRow("Duplicate refs avoided", humanize.Comma(deduped), good)
		if s.TotalChunkRefs > 0 {
			dedupPct := float64(deduped) / float64(s.TotalChunkRefs) * 100
			statRow("Dedup hit rate", fmt.Sprintf("%.1f%%", dedupPct), good)
		}
	} else {
		statRow("Duplicate refs avoided", "0 (no shared chunks yet)", dim)
	}

	// ── STRUCTURE ──────────────────────────────────────────────────────────
	fmt.Println()
	sectionHdr.Println("  STRUCTURE")
	fmt.Println("  ", dim.Sprint(sep))

	statRow("Partition files", humanize.Comma(s.TotalPartitions), val)
	statRow("Indexed FS objects", humanize.Comma(s.TotalIndexedFiles), val)
	statRow("Indexed names", humanize.Comma(s.TotalIndexedNames), val)

	if s.TotalIndexedFiles > 0 {
		deletedPct := float64(s.DeletedIndexedFiles) / float64(s.TotalIndexedFiles) * 100
		fragmentedPct := float64(s.FragmentedIndexedFiles) / float64(s.TotalIndexedFiles) * 100
		statRow("Deleted indexed objects", fmt.Sprintf("%s (%.1f%%)", humanize.Comma(s.DeletedIndexedFiles), deletedPct), val)
		if s.FragmentedIndexedFiles > 0 {
			statRow("Fragmented indexed objects", fmt.Sprintf("%s (%.1f%%)", humanize.Comma(s.FragmentedIndexedFiles), fragmentedPct), warn)
		} else {
			statRow("Fragmented indexed objects", "0 (currently skipped by parser)", dim)
		}
	}

	if s.TotalIndexedNames > 0 {
		deletedNamePct := float64(s.DeletedIndexedNames) / float64(s.TotalIndexedNames) * 100
		fragmentedNamePct := float64(s.FragmentedIndexedNames) / float64(s.TotalIndexedNames) * 100
		statRow("Deleted indexed names", fmt.Sprintf("%s (%.1f%%)", humanize.Comma(s.DeletedIndexedNames), deletedNamePct), val)
		if s.FragmentedIndexedNames > 0 {
			statRow("Fragmented indexed names", fmt.Sprintf("%s (%.1f%%)", humanize.Comma(s.FragmentedIndexedNames), fragmentedNamePct), warn)
		} else {
			statRow("Fragmented indexed names", "0 (currently skipped by parser)", dim)
		}
	}

	// ── STORAGE ────────────────────────────────────────────────────────────
	fmt.Println()
	sectionHdr.Println("  STORAGE")
	fmt.Println("  ", dim.Sprint(sep))

	statRow("Logical size", humanize.Bytes(uint64(s.TotalLogicalSize)), val)
	statRow("On-disk size (blobs)", humanize.Bytes(uint64(s.OnDiskBytes)), val)

	if s.TotalLogicalSize > 0 && s.OnDiskBytes > 0 {
		if s.OnDiskBytes < s.TotalLogicalSize {
			saved := s.TotalLogicalSize - s.OnDiskBytes
			pct := float64(saved) / float64(s.TotalLogicalSize) * 100
			statRow("Space saved", fmt.Sprintf("%s (%.1f%%)", humanize.Bytes(uint64(saved)), pct), good)
		} else {
			ratio := float64(s.OnDiskBytes) / float64(s.TotalLogicalSize)
			statRow("Storage overhead", fmt.Sprintf("%.2fx (encrypted+compressed)", ratio), dim)
		}
	}

	// ── WRITE ENGINE ───────────────────────────────────────────────────────
	fioStats := fio.GetWriteBackendStats()

	fmt.Println()
	sectionHdr.Println("  WRITE ENGINE")
	fmt.Println("  ", dim.Sprint(sep))

	statRow("Requested engine", fioStats.RequestedEngine, val)
	statRow("Selected engine", fioStats.SelectedEngine, val)
	statRow("Fallback count", humanize.Comma(int64(fioStats.FallbackCount)), val)
	if fioStats.RequestedEngine == cnst.StoreIOEngineUring || fioStats.SelectedEngine == cnst.StoreIOEngineUring {
		statRow("io_uring queue depth", humanize.Comma(int64(fioStats.IOUringQueueDepth)), val)
		statRow("io_uring submits", humanize.Comma(int64(fioStats.IOUringSubmitCount)), val)
		statRow("io_uring waits", humanize.Comma(int64(fioStats.IOUringSubmitWaits)), val)
		statRow("io_uring completions", humanize.Comma(int64(fioStats.IOUringCompletions)), val)
		if fioStats.IOUringQueueFull > 0 {
			statRow("io_uring queue-full", humanize.Comma(int64(fioStats.IOUringQueueFull)), warn)
		} else {
			statRow("io_uring queue-full", "0", dim)
		}
		if fioStats.IOUringSubmitErrors > 0 {
			statRow("io_uring submit errors", humanize.Comma(int64(fioStats.IOUringSubmitErrors)), warn)
		} else {
			statRow("io_uring submit errors", "0", dim)
		}
	}

	fmt.Println()
}
