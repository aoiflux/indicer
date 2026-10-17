package parser

import (
	"context"
	"encoding/base64"
	"fmt"
	"indicer/internal/console"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/enrichment"
	"indicer/lib/fts"
	"indicer/lib/store"
	"indicer/lib/structs"
	"indicer/lib/util"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/google/uuid"
	"github.com/schollz/progressbar/v3"
)

func countFragmented(files []tuskFile) int {
	var n int
	for _, f := range files {
		if f.IsFragmented {
			n++
		}
	}
	return n
}

func countDeleted(files []tuskFile) int {
	var n int
	for _, f := range files {
		if f.IsDeleted {
			n++
		}
	}
	return n
}

func buildIdxMap(files []tuskFile, idxmap map[string]structs.IndexedFile, pfile structs.InputFile, encodedPfileHash []byte, eviID []byte) error {
	for _, f := range files {
		if f.IsFragmented {
			// Current parser behavior: fragmented entries are intentionally skipped.
			// We still keep IsFragmented in per-name metadata for forward compatibility.
			continue
		}
		iname := string(util.AppendToBytesSlice(eviID, cnst.DataSeperator, encodedPfileHash, cnst.DataSeperator, []byte(f.Filename)))
		for _, frag := range f.Fragments {
			istart := frag.StartOffset
			isize := frag.EndOffset - frag.StartOffset + 1
			if err := registerIndexedRange(idxmap, pfile, eviID, iname, istart, isize, f.IsDeleted, f.times()); err != nil {
				return err
			}
		}
	}
	return nil
}

// timestampsOf builds a stored timestamp set, dropping zero times as "not
// carried by this filesystem". It returns nil when nothing is known, so such a
// record encodes exactly as it did before timestamps existed.
func timestampsOf(modified, accessed, changed, born time.Time) *structs.Timestamps {
	nanos := func(t time.Time) *int64 {
		if t.IsZero() {
			return nil
		}
		n := t.UnixNano()
		return &n
	}
	times := &structs.Timestamps{
		Modified: nanos(modified),
		Accessed: nanos(accessed),
		Changed:  nanos(changed),
		Born:     nanos(born),
	}
	if !times.Any() {
		return nil
	}
	return times
}

func registerIndexedRange(idxmap map[string]structs.IndexedFile, pfile structs.InputFile, eviID []byte, iname string, istart, isize int64, isDeleted bool, times *structs.Timestamps) error {
	if isize <= 0 {
		return fmt.Errorf("invalid indexed file size %d for %s", isize, iname)
	}

	mapped := pfile.GetMappedFile()
	// Compare without adding: a huge size would wrap start+size negative and
	// slip past the check (PAR-8).
	if istart < 0 || istart > int64(len(mapped)) || isize > int64(len(mapped))-istart {
		return fmt.Errorf("indexed file range out of bounds for %s: start=%d size=%d mapped=%d", iname, istart, isize, len(mapped))
	}
	end := istart + isize
	if end > int64(len(mapped)) {
		return fmt.Errorf("indexed file range out of bounds for %s: start=%d end=%d mapped=%d", iname, istart, end, len(mapped))
	}

	chunk := mapped[int(istart):int(end)]
	var ihash []byte
	if cnst.StoreHashStrategy == cnst.HochoHashStrategy && cnst.HochoMode != cnst.HochoModeBaseline {
		reusedHash, reused, reuseErr := store.TryComputeLogicalHochoWithEdges(eviID, istart, isize, mapped, pfile.GetDB())
		if reuseErr != nil {
			return reuseErr
		}
		if reused {
			ihash = reusedHash
		}
	}
	if len(ihash) == 0 {
		var err error
		ihash, err = util.GetLogicalFileHash(pfile.GetHandle(), cnst.GetHashAlgo(true), istart, isize, false)
		if err != nil {
			return err
		}
	}

	indexedID, err := uuid.NewV7()
	if err != nil {
		return err
	}
	idB64 := base64.StdEncoding.EncodeToString(indexedID[:])
	typeHint := util.DetectFileType(iname, chunk)
	indexed := structs.NewIndexedFile(iname, istart, isize, typeHint, isDeleted, base64.StdEncoding.EncodeToString(ihash))
	indexed.Times = times
	idxmap[idB64] = indexed
	pfile.UpdateInternalObjects(istart, isize, indexedID[:])
	return nil
}

type indexedRef struct {
	idB64   string
	hashB64 string
}

func finalizeIndexedFiles(idxmap map[string]structs.IndexedFile, pfile structs.InputFile, batch *badger.WriteBatch, eviID []byte, enableFTS bool, enableEnrichment bool) error {
	indexedIDs := make([]string, 0, len(idxmap))
	refs := make([]indexedRef, 0, len(idxmap))
	for indexedID, ifile := range idxmap {
		indexedIDs = append(indexedIDs, indexedID)
		refs = append(refs, indexedRef{idB64: indexedID, hashB64: ifile.FileHash})
	}

	ftsJobs, err := storeIndexedFiles(idxmap, batch)
	if err != nil {
		return err
	}

	fmt.Fprintln(console.Err(), "Flushing indexed metadata batch...")
	err = batch.Flush()
	if err != nil {
		return err
	}
	fmt.Fprintln(console.Err(), "Indexed metadata batch flush complete.")

	fmt.Fprintln(console.Err(), "Linking indexed hash lookups...")
	if err := appendIndexedHashLookupsBatched(pfile.GetDB(), refs); err != nil {
		return err
	}
	fmt.Fprintln(console.Err(), "Indexed hash lookup linking complete.")

	// A timeout here once abandoned the write still running (ING-3).
	fmt.Fprintln(console.Err(), "Persisting partition metadata...")
	if err := store.StoreInputFile(context.Background(), pfile); err != nil {
		return err
	}
	fmt.Fprintln(console.Err(), "Partition metadata persist complete.")

	if enableEnrichment {
		repo, err := enrichment.OpenGrapheneRepository(pfile.GetDB().Opts().Dir)
		if err != nil {
			return err
		}
		service := enrichment.NewService(pfile.GetDB(), repo)
		defer service.Close()
		if err := service.EnrichPartition(pfile, eviID, indexedIDs); err != nil {
			return err
		}
	}

	if enableFTS {
		return indexFullTextSidecar(pfile.GetDB(), ftsJobs)
	}
	return nil
}

func appendIndexedHashLookupsBatched(db *badger.DB, refs []indexedRef) error {
	if len(refs) == 0 {
		return nil
	}

	const batchSize = 512
	bar := progressbar.NewOptions64(
		int64(len(refs)),
		progressbar.OptionSetDescription("Linking hash lookups"),
		progressbar.OptionSetWriter(console.Progress()),
		progressbar.OptionSetTheme(cnst.CommonProgressBarTheme),
	)

	for start := 0; start < len(refs); start += batchSize {
		end := start + batchSize
		if end > len(refs) {
			end = len(refs)
		}

		if err := db.Update(func(txn *badger.Txn) error {
			for _, ref := range refs[start:end] {
				rawID, err := base64.StdEncoding.DecodeString(ref.idB64)
				if err != nil {
					return err
				}
				indexedKey := dbio.CanonicalIndexedKey(rawID)
				lookupKey := util.AppendToBytesSlice(cnst.IdxFileHashLookupNamespace, ref.hashB64)
				if err := dbio.AppendHashLookupUUIDByKeyTxn(txn, lookupKey, indexedKey); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}

		bar.Add(end - start)
	}

	bar.Finish()
	fmt.Fprintln(console.Err())
	return nil
}

type ftsIndexJob = fts.IndexedFileJob

func indexFullTextSidecar(db *badger.DB, jobs []ftsIndexJob) error {
	if len(jobs) == 0 {
		return nil
	}

	fmt.Fprintln(console.Err(), "Updating full-text index sidecar...")
	bar := progressbar.NewOptions64(
		int64(len(jobs)),
		progressbar.OptionSetDescription("Indexing full-text sidecar"),
		progressbar.OptionSetWriter(console.Progress()),
		progressbar.OptionSetTheme(cnst.CommonProgressBarTheme),
	)

	if err := fts.IndexIndexedFileJobs(db, jobs, func() {
		bar.Add(1)
	}); err != nil {
		return err
	}

	bar.Finish()
	fmt.Fprintln(console.Err(), "Full-text index sidecar update complete.")
	fmt.Fprintln(console.Err())
	return nil
}

func storeIndexedFiles(idxmap map[string]structs.IndexedFile, batch *badger.WriteBatch) ([]ftsIndexJob, error) {
	ftsJobs := make([]ftsIndexJob, 0, len(idxmap))
	total := int64(len(idxmap))
	bar := progressbar.NewOptions64(
		total,
		progressbar.OptionSetDescription("Indexing files"),
		progressbar.OptionSetWriter(console.Progress()),
		progressbar.OptionSetTheme(cnst.CommonProgressBarTheme),
	)
	bar.Clear()

	for indexedID, newIdxfile := range idxmap {
		rawID, err := base64.StdEncoding.DecodeString(indexedID)
		if err != nil {
			return nil, err
		}
		id := dbio.CanonicalIndexedKey(rawID)
		if err := dbio.SetIndexedFile(id, newIdxfile, batch); err != nil {
			return nil, err
		}
		ftsJobs = append(ftsJobs, ftsIndexJob{FileID: string(id), Name: newIdxfile.Name})
		bar.Add(1)
	}

	if len(idxmap) > 0 {
		bar.Finish()
		fmt.Fprintln(console.Err())
	}
	return ftsJobs, nil
}
