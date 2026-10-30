package store

import (
	"context"
	"fmt"
	"indicer/internal/console"
	"os"
	"path/filepath"
	"strings"

	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/dbmeta"
	"indicer/lib/fio"
	"indicer/lib/structs"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
	"github.com/dustin/go-humanize"
	"github.com/fatih/color"
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
	// SharedChunks counts the chunks whose reverse relations name more than
	// one object. Without reverse relations (stored without --revrel) it is
	// 0.
	SharedChunks int64
	OnDiskBytes  int64
	// ChunkSize is the database's chunk size, in bytes.
	ChunkSize int64
}

// Stats prints the database's statistics.
func Stats(ctx context.Context, db *badger.DB) error {
	s, err := GatherStats(ctx, db)
	if err != nil {
		return err
	}
	printStats(s)
	return nil
}

// GatherStats reads the database's statistics in one read transaction,
// stopping when ctx ends. Its memory does not grow with the database.
func GatherStats(ctx context.Context, db *badger.DB) (*DBStats, error) {
	s := &DBStats{ChunkSize: dbmeta.ChunkSize(db)}

	err := db.View(func(txn *badger.Txn) error {
		// count calls fn for each key under prefix, checking ctx as it goes.
		count := func(prefix string, values bool, fn func(*badger.Item) error) error {
			opts := badger.DefaultIteratorOptions
			opts.PrefetchValues = values
			opts.PrefetchSize = 1000
			it := txn.NewIterator(opts)
			defer it.Close()
			n := 0
			for it.Seek([]byte(prefix)); it.ValidForPrefix([]byte(prefix)); it.Next() {
				if n%4096 == 0 {
					if err := ctx.Err(); err != nil {
						return err
					}
				}
				n++
				if err := fn(it.Item()); err != nil {
					return err
				}
			}
			return nil
		}
		record := func(item *badger.Item, v any) error {
			data, err := item.ValueCopy(nil)
			if err != nil {
				return err
			}
			if err := dbio.DecodeRecord(data, v); err != nil {
				return fmt.Errorf("record %q: %w", item.Key(), err)
			}
			return nil
		}

		err := count(cnst.EviFileNamespace, true, func(item *badger.Item) error {
			var ef structs.EvidenceFile
			if err := record(item, &ef); err != nil {
				return err
			}
			s.TotalFiles++
			if ef.Completed && ef.IngestState != structs.IngestStateDuplicate {
				s.CompletedFiles++
				s.TotalLogicalSize += ef.Size
			}
			return nil
		})
		if err != nil {
			return err
		}
		if err := count(cnst.PartiFileNamespace, false, func(*badger.Item) error { s.TotalPartitions++; return nil }); err != nil {
			return err
		}
		err = count(cnst.IdxFileNamespace, true, func(item *badger.Item) error {
			var ifile structs.IndexedFile
			if err := record(item, &ifile); err != nil {
				return err
			}
			s.TotalIndexedFiles++
			if ifile.Name != "" {
				s.TotalIndexedNames++
			}
			if ifile.IsDeleted {
				s.DeletedIndexedFiles++
				s.DeletedIndexedNames++
			}
			if ifile.IsFragmented {
				s.FragmentedIndexedFiles++
				s.FragmentedIndexedNames++
			}
			return nil
		})
		if err != nil {
			return err
		}
		if err := count(cnst.ChonkNamespace, false, func(*badger.Item) error { s.UniqueChunks++; return nil }); err != nil {
			return err
		}
		if err := count(cnst.RelationNamespace, false, func(*badger.Item) error { s.TotalChunkRefs++; return nil }); err != nil {
			return err
		}
		// Reverse relations are counted in every form they were kept in,
		// one chunk at a time. Stats used to split their keys on the
		// separator and hold every one in a map; the one-key-per-occurrence
		// form written since W3 has fewer parts, and failed (STO-24).
		s.SharedChunks, err = dbio.CountSharedChunks(ctx, txn, dbmeta.ChunkHasher(db)().Size())
		return err
	})
	if err != nil {
		return nil, err
	}

	s.OnDiskBytes = calcBlobDirBytes(dbio.KVDir(db))
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
	console.Println()
	sectionHdr.Println("  FILES")
	console.Println("  ", dim.Sprint(sep))

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
	console.Println()
	sectionHdr.Println("  CHUNKS")
	console.Println("  ", dim.Sprint(sep))

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
	statRow("Chunk size", humanize.IBytes(uint64(s.ChunkSize)), dim)

	if s.SharedChunks > 0 {
		statRow("Chunks shared by objects", humanize.Comma(s.SharedChunks), good)
	} else {
		statRow("Chunks shared by objects", "0 (or stored without --revrel)", dim)
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
	console.Println()
	sectionHdr.Println("  STRUCTURE")
	console.Println("  ", dim.Sprint(sep))

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
			statRow("Fragmented indexed objects", "0", dim)
		}
	}

	if s.TotalIndexedNames > 0 {
		deletedNamePct := float64(s.DeletedIndexedNames) / float64(s.TotalIndexedNames) * 100
		fragmentedNamePct := float64(s.FragmentedIndexedNames) / float64(s.TotalIndexedNames) * 100
		statRow("Deleted indexed names", fmt.Sprintf("%s (%.1f%%)", humanize.Comma(s.DeletedIndexedNames), deletedNamePct), val)
		if s.FragmentedIndexedNames > 0 {
			statRow("Fragmented indexed names", fmt.Sprintf("%s (%.1f%%)", humanize.Comma(s.FragmentedIndexedNames), fragmentedNamePct), warn)
		} else {
			statRow("Fragmented indexed names", "0", dim)
		}
	}

	// ── STORAGE ────────────────────────────────────────────────────────────
	console.Println()
	sectionHdr.Println("  STORAGE")
	console.Println("  ", dim.Sprint(sep))

	statRow("Logical size", humanize.Bytes(uint64(s.TotalLogicalSize)), val)
	statRow("On-disk size (blobs)", humanize.Bytes(uint64(s.OnDiskBytes)), val)

	if s.TotalLogicalSize > 0 && s.OnDiskBytes > 0 {
		if s.OnDiskBytes < s.TotalLogicalSize {
			saved := s.TotalLogicalSize - s.OnDiskBytes
			pct := float64(saved) / float64(s.TotalLogicalSize) * 100
			statRow("Space saved", fmt.Sprintf("%s (%.1f%%)", humanize.Bytes(uint64(saved)), pct), good)
		} else {
			ratio := float64(s.OnDiskBytes) / float64(s.TotalLogicalSize)
			statRow("Storage overhead", fmt.Sprintf("%.2fx", ratio), dim)
		}
	}

	// ── WRITE ENGINE ───────────────────────────────────────────────────────
	// The counters are this process's. dues stats writes nothing, so they
	// mean something only in a process that also stored data (the TUI).
	fioStats := fio.GetWriteBackendStats()

	console.Println()
	sectionHdr.Println("  WRITE ENGINE (this process)")
	console.Println("  ", dim.Sprint(sep))

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

	console.Println()
}
