package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"indicer/lib/dbio"
	"indicer/lib/parser/core"
	"indicer/lib/parser/image"
	"indicer/lib/structs"

	"github.com/dgraph-io/badger/v4"
)

// QuarantinedEvidence is one evidence whose index QuarantineRawContainerIndexes
// marked invalid.
type QuarantinedEvidence struct {
	ID     []byte
	Name   string
	Format core.ImageFormat
}

// quarantineReason is what an invalid index records.
const quarantineReason = "indexed before W6 over the raw bytes of an EWF or VHD container, not the disk it holds (ING-7); store the evidence again to index its decoded disk"

// QuarantineRawContainerIndexes marks invalid (structs.IndexInvalid) the
// index of every evidence an ingest before W6 stored as the raw bytes of an
// EWF set or a dynamic or differencing VHD/VHDX. Its partitions and files were
// parsed out of the container's bytes as if they were the disk (ING-7), so
// their offsets, hashes and content are wrong. The records stay, and listings
// hide them; storing the evidence again stores its decoded disk and indexes
// that. A fixed VHD is a raw disk followed by a footer, so an index over it is
// right and is kept. Running it again changes nothing.
func QuarantineRawContainerIndexes(ctx context.Context, db *badger.DB) ([]QuarantinedEvidence, error) {
	type candidate struct {
		id []byte
		e  structs.EvidenceFile
	}
	var candidates []candidate
	err := db.View(func(txn *badger.Txn) error {
		return forEachEvidenceRecord(txn, func(id []byte, e structs.EvidenceFile) error {
			indexed := len(e.InternalObjects) > 0
			quarantined := e.Index != nil && e.Index.State == structs.IndexInvalid
			if e.ContentModel == "" && e.Completed && indexed && !quarantined {
				candidates = append(candidates, candidate{id: id, e: e})
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	var out []QuarantinedEvidence
	for _, c := range candidates {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		format, err := storedContainerFormat(db, c.id)
		if err != nil {
			return out, fmt.Errorf("evidence %s: %w", firstCleanName(c.e.Name), err)
		}
		if format == core.FormatRaw {
			continue
		}
		err = dbio.UpdateEvidence(db, c.id, func(_ *badger.Txn, e *structs.EvidenceFile) error {
			if e.Index != nil && e.Index.State == structs.IndexInvalid {
				return dbio.ErrNoChange
			}
			e.Index = &structs.IndexInfo{State: structs.IndexInvalid, Reason: quarantineReason, Finished: time.Now().UnixNano()}
			return nil
		})
		if err != nil {
			return out, err
		}
		out = append(out, QuarantinedEvidence{ID: c.id, Name: firstCleanName(c.e.Name), Format: format})
	}
	return out, nil
}

// storedContainerFormat sniffs the head of a stored evidence: EWF, or a
// VHD/VHDX whose disk does not start at offset 0. Only the head is read, so a
// fixed VHD (its footer is at the end) reads as raw.
func storedContainerFormat(db *badger.DB, id []byte) (core.ImageFormat, error) {
	ec, err := dbio.OpenEvidenceChunks(db, id, dbio.ReadOptions{CacheBytes: -1})
	if err != nil {
		return "", err
	}
	head := make([]byte, min(ec.Size(), 4096))
	n, err := ec.ReadAt(head, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	// A size of 0 skips the footer check: the head is all that matters.
	return image.DetectFormat(byteReaderAt(head[:n]), 0), nil
}
